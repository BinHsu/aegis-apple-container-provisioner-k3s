// SPDX-License-Identifier: MIT

package egress

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"strings"
	"time"
)

// maxDNSUDP is the largest UDP DNS message accepted (EDNS0 practical ceiling).
const maxDNSUDP = 4096

// HostResolvers reads "nameserver" lines from a resolv.conf-style file and returns them as
// "ip:53". On macOS with WARP connected /etc/resolv.conf lists WARP's local resolvers
// (127.0.2.2, 127.0.2.3), which is exactly the path ordinary host processes use.
func HostResolvers(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("egress: reading host resolvers: %w", err)
	}
	defer f.Close() //nolint:errcheck

	return parseResolvConf(f)
}

func parseResolvConf(r io.Reader) ([]string, error) {
	var out []string

	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 || fields[0] != "nameserver" {
			continue
		}

		a, err := netip.ParseAddr(fields[1])
		if err != nil {
			continue
		}

		out = append(out, net.JoinHostPort(a.String(), "53"))
	}

	if len(out) == 0 {
		return nil, errors.New("egress: no nameserver found in host resolv.conf; pass upstreams explicitly")
	}

	return out, sc.Err()
}

// serveDNSUDP forwards each guest query verbatim to the first upstream that answers.
func (r *Relay) serveDNSUDP(pc net.PacketConn) {
	defer r.wg.Done()

	buf := make([]byte, maxDNSUDP)

	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}

			continue
		}

		if !r.allowed(from) {
			continue
		}

		q := append([]byte(nil), buf[:n]...)

		go func() {
			if resp, err := r.exchangeUDP(q); err == nil {
				pc.WriteTo(resp, from) //nolint:errcheck
			} else {
				r.cfg.Logger.Printf("dns udp from %s: %v", from, err)
			}
		}()
	}
}

func (r *Relay) exchangeUDP(q []byte) ([]byte, error) {
	var lastErr error

	for _, up := range r.cfg.DNSUpstreams {
		resp, err := func() ([]byte, error) {
			c, err := net.DialTimeout("udp", up, dnsTimeout)
			if err != nil {
				return nil, err
			}
			defer c.Close() //nolint:errcheck

			c.SetDeadline(time.Now().Add(dnsTimeout)) //nolint:errcheck

			if _, err := c.Write(q); err != nil {
				return nil, err
			}

			b := make([]byte, maxDNSUDP)

			n, err := c.Read(b)
			if err != nil {
				return nil, err
			}

			return b[:n], nil
		}()
		if err == nil {
			return resp, nil
		}

		lastErr = err
	}

	return nil, lastErr
}

// serveDNSTCP handles DNS over TCP (2-byte length prefix). Each query is answered via the UDP
// upstream path, so a truncated (TC) UDP answer is passed back as-is.
func (r *Relay) serveDNSTCP(c net.Conn) {
	defer c.Close() //nolint:errcheck

	for {
		c.SetReadDeadline(time.Now().Add(headerTimeout)) //nolint:errcheck

		var l [2]byte
		if _, err := io.ReadFull(c, l[:]); err != nil {
			return
		}

		q := make([]byte, binary.BigEndian.Uint16(l[:]))
		if _, err := io.ReadFull(c, q); err != nil {
			return
		}

		resp, err := r.exchangeUDP(q)
		if err != nil {
			r.cfg.Logger.Printf("dns tcp from %s: %v", c.RemoteAddr(), err)

			return
		}

		out := binary.BigEndian.AppendUint16(nil, uint16(len(resp))) //nolint:gosec // len <= maxDNSUDP
		if _, err := c.Write(append(out, resp...)); err != nil {
			return
		}
	}
}
