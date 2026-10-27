// SPDX-License-Identifier: MIT

package egress

import (
	"bufio"
	"net"
	"net/http"
	"strings"
	"time"
)

// serveHTTP handles one guest connection on the HTTP port. Three request shapes:
//
//   - CONNECT host:port         explicit proxy tunnel (HTTPS_PROXY users)
//   - GET http://host/path      explicit proxy, absolute-form (HTTP_PROXY users)
//   - GET /path + Host: host    transparent (a DNAT'd plain-HTTP request)
//
// After the first request is forwarded the connection becomes a byte pipe to that upstream, so a
// keep-alive client stays on the same origin. For the absolute form the relay adds
// "Connection: close" so a proxy client never reuses the pipe for a different origin.
func (r *Relay) serveHTTP(c net.Conn) {
	c.SetReadDeadline(time.Now().Add(headerTimeout)) //nolint:errcheck

	br := bufio.NewReader(c)

	req, err := http.ReadRequest(br)
	if err != nil {
		c.Close() //nolint:errcheck

		return
	}

	c.SetReadDeadline(time.Time{}) //nolint:errcheck

	if req.Method == http.MethodConnect {
		target := withDefaultPort(req.Host, "443")

		up, err := upstreamDialer.Dial("tcp", target)
		if err != nil {
			r.cfg.Logger.Printf("http CONNECT %s from %s: %v", target, c.RemoteAddr(), err)
			c.Write([]byte("HTTP/1.1 502 Bad Gateway\r\nConnection: close\r\n\r\n")) //nolint:errcheck
			c.Close()                                                                //nolint:errcheck

			return
		}

		r.cfg.Logger.Printf("http CONNECT %s from %s", target, c.RemoteAddr())
		c.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")) //nolint:errcheck
		splice(&bufferedConn{Conn: c, r: br}, up)

		return
	}

	host := req.Host
	absolute := req.URL.IsAbs()

	if absolute {
		host = req.URL.Host
		req.Header.Set("Connection", "close")
		req.Header.Del("Proxy-Connection")
		req.Close = true
	}

	if host == "" {
		c.Write([]byte("HTTP/1.1 400 Bad Request\r\nConnection: close\r\n\r\n")) //nolint:errcheck
		c.Close()                                                                //nolint:errcheck

		return
	}

	target := withDefaultPort(host, "80")

	up, err := upstreamDialer.Dial("tcp", target)
	if err != nil {
		r.cfg.Logger.Printf("http %s %s from %s: %v", req.Method, target, c.RemoteAddr(), err)
		c.Write([]byte("HTTP/1.1 502 Bad Gateway\r\nConnection: close\r\n\r\n")) //nolint:errcheck
		c.Close()                                                                //nolint:errcheck

		return
	}

	r.cfg.Logger.Printf("http %s %s from %s (absolute=%v)", req.Method, target, c.RemoteAddr(), absolute)

	// req.Write emits origin-form (path only), which is what an origin server expects.
	if err := req.Write(up); err != nil {
		up.Close() //nolint:errcheck
		c.Close()  //nolint:errcheck

		return
	}

	splice(&bufferedConn{Conn: c, r: br}, up)
}

// withDefaultPort appends port when hostport has none (IPv6 literals included).
func withDefaultPort(hostport, port string) string {
	if _, _, err := net.SplitHostPort(hostport); err == nil {
		return hostport
	}

	return net.JoinHostPort(strings.Trim(hostport, "[]"), port)
}

// bufferedConn reads through a bufio.Reader so bytes already buffered while parsing the request
// are not lost when the connection turns into a pipe. CloseWrite is forwarded for half-close.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufferedConn) Read(p []byte) (int, error) { return b.r.Read(p) }

func (b *bufferedConn) CloseWrite() error {
	if tc, ok := b.Conn.(*net.TCPConn); ok {
		return tc.CloseWrite()
	}

	return nil
}
