// SPDX-License-Identifier: MIT

package egress

import (
	"bufio"
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"
)

// clientHello captures the first TLS record a real crypto/tls client sends.
func clientHello(t *testing.T, serverName string) []byte {
	t.Helper()

	a, b := net.Pipe()
	defer a.Close()

	go func() {
		c := tls.Client(b, &tls.Config{ServerName: serverName, InsecureSkipVerify: true}) //nolint:gosec // test
		c.Handshake()                                                                     //nolint:errcheck
	}()

	buf := make([]byte, 5)
	if _, err := io.ReadFull(a, buf); err != nil {
		t.Fatal(err)
	}

	body := make([]byte, int(buf[3])<<8|int(buf[4]))
	if _, err := io.ReadFull(a, body); err != nil {
		t.Fatal(err)
	}

	b.Close()

	return append(buf, body...)
}

func TestParseSNI(t *testing.T) {
	rec := clientHello(t, "ingest.example.com")

	got, err := parseSNI(rec)
	if err != nil || got != "ingest.example.com" {
		t.Fatalf("parseSNI = %q, %v", got, err)
	}

	// No SNI (crypto/tls omits it for an IP ServerName): routable = no, error = no.
	if got, err := parseSNI(clientHello(t, "93.184.215.14")); err != nil || got != "" {
		t.Fatalf("IP hello: %q, %v; want empty SNI", got, err)
	}

	// Not TLS at all (plain HTTP on 443).
	if _, err := parseSNI([]byte("GET / HTTP/1.1\r\n")); err == nil {
		t.Fatal("plain HTTP parsed as a ClientHello")
	}

	// Every truncation of a valid hello must fail cleanly, never panic or return a bogus name.
	for i := range len(rec) - 1 {
		if got, err := parseSNI(rec[:i]); err == nil && got == "ingest.example.com" {
			t.Fatalf("truncated hello (%d/%d bytes) yielded the full SNI", i, len(rec))
		}
	}
}

func TestIsPublic(t *testing.T) {
	cases := map[string]bool{
		"1.1.1.1": true, "93.184.215.14": true, "2606:4700::1111": true,
		"127.0.0.1": false, "127.0.2.2": false, "10.42.0.1": false, "172.16.0.2": false,
		"172.31.255.255": false, "172.32.0.1": true, "172.15.255.255": true,
		"192.168.200.1": false, "169.254.1.1": false, "0.0.0.0": false, "::1": false,
		"224.0.0.1": false, "::ffff:127.0.0.1": false, "fd00::1": false,
	}
	for s, want := range cases {
		if got := isPublic(netip.MustParseAddr(s)); got != want {
			t.Errorf("isPublic(%s) = %v, want %v", s, got, want)
		}
	}
}

func TestParseResolvConf(t *testing.T) {
	got, err := parseResolvConf(strings.NewReader("# x\nnameserver 127.0.2.2\nsearch lan\nnameserver bogus\nnameserver ::1\n"))
	if err != nil || strings.Join(got, ",") != "127.0.2.2:53,[::1]:53" {
		t.Fatalf("got %v, %v", got, err)
	}

	if _, err := parseResolvConf(strings.NewReader("search lan\n")); err == nil {
		t.Fatal("no nameserver must be an error, not an empty upstream list")
	}
}

func TestGuestScript(t *testing.T) {
	s, err := GuestScript(netip.MustParseAddr("192.168.200.1"), 15080, 15443, 15053, DefaultBypass)
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		"-d 10.0.0.0/8 -j RETURN",
		"--dport 443 -j DNAT --to-destination 192.168.200.1:15443",
		"-p udp --dport 53 -j DNAT --to-destination 192.168.200.1:15053",
		"iptables -t nat -I PREROUTING 1 -j AC-EGRESS",
		"iptables -t nat -I OUTPUT 1 -j AC-EGRESS",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("script missing %q", want)
		}
	}

	// Bypass RETURNs must precede the first DNAT, else private traffic is redirected.
	if strings.LastIndex(s, "-j RETURN") > strings.Index(s, "-j DNAT") {
		t.Error("a bypass RETURN comes after a DNAT")
	}

	if _, err := GuestScript(netip.MustParseAddr("fd00::1"), 1, 2, 3, nil); err == nil {
		t.Error("IPv6 relay accepted")
	}

	if _, err := GuestScript(netip.MustParseAddr("192.168.200.1"), 1, 2, 3, []string{"10.0.0.0/33"}); err == nil {
		t.Error("bad bypass CIDR accepted")
	}
}

func TestIsBridgeName(t *testing.T) {
	for name, want := range map[string]bool{"bridge100": true, "bridge101": true, "bridge0": false, "en0": false, "utun1": false, "lo0": false} {
		if IsBridgeName(name) != want {
			t.Errorf("IsBridgeName(%s) != %v", name, want)
		}
	}
}

func TestStartRefusesUnspecified(t *testing.T) {
	_, err := Start(context.Background(), Config{ListenIP: netip.MustParseAddr("0.0.0.0"), AllowedSource: netip.MustParsePrefix("127.0.0.0/8")})
	if err == nil {
		t.Fatal("Start bound 0.0.0.0")
	}
}

// TestHTTPRelay exercises the three HTTP request shapes end to end on loopback, with the
// public-only check lifted for the duration (the upstream is a loopback test server).
func TestHTTPRelay(t *testing.T) {
	orig := upstreamDialer.Control
	upstreamDialer.Control = nil

	t.Cleanup(func() { upstreamDialer.Control = orig })

	origin, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer origin.Close()

	go http.Serve(origin, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { //nolint:errcheck,gosec
		io.WriteString(w, "origin:"+r.URL.Path) //nolint:errcheck
	}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	r, err := Start(ctx, Config{
		ListenIP: netip.MustParseAddr("127.0.0.1"), AllowedSource: netip.MustParsePrefix("127.0.0.0/8"),
		HTTPPort: freePort(t), TLSPort: freePort(t), DNSPort: freePort(t), DNSUpstreams: []string{"127.0.0.1:9"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	relay := net.JoinHostPort("127.0.0.1", strconv.Itoa(r.cfg.HTTPPort))
	up := origin.Addr().String()

	for name, raw := range map[string]string{
		"transparent": "GET /a HTTP/1.1\r\nHost: " + up + "\r\n\r\n",
		"absolute":    "GET http://" + up + "/a HTTP/1.1\r\nHost: " + up + "\r\n\r\n",
	} {
		if got := roundTrip(t, relay, raw); !strings.Contains(got, "origin:/a") {
			t.Errorf("%s: response %q", name, got)
		}
	}

	c, err := net.Dial("tcp", relay)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	io.WriteString(c, "CONNECT "+up+" HTTP/1.1\r\nHost: "+up+"\r\n\r\n") //nolint:errcheck

	br := bufio.NewReader(c)

	line, _ := br.ReadString('\n')
	if !strings.Contains(line, "200") {
		t.Fatalf("CONNECT status %q", line)
	}

	br.ReadString('\n')                                                          //nolint:errcheck // blank line
	io.WriteString(c, "GET /b HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n") //nolint:errcheck

	rest, _ := io.ReadAll(br)
	if !strings.Contains(string(rest), "origin:/b") {
		t.Fatalf("CONNECT tunnel response %q", rest)
	}
}

func roundTrip(t *testing.T, addr, raw string) string {
	t.Helper()

	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	c.SetDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck
	io.WriteString(c, raw)                         //nolint:errcheck

	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}

	b, _ := io.ReadAll(resp.Body)

	return string(b)
}

func freePort(t *testing.T) int {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	return l.Addr().(*net.TCPAddr).Port
}
