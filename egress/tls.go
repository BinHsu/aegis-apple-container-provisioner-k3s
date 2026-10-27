// SPDX-License-Identifier: MIT

package egress

import (
	"bufio"
	"encoding/binary"
	"errors"
	"net"
	"time"
)

// maxHello bounds how much of a ClientHello is buffered to find the SNI (one TLS record max).
const maxHello = 5 + 16384

// serveTLS handles one DNAT'd TLS connection: it peeks the ClientHello, reads the SNI, dials
// SNI:443 from the host and pipes bytes unchanged (no decryption, no certificates here). A hello
// without SNI (a literal-IP HTTPS request) cannot be routed — DNAT erased the original address —
// and is closed.
func (r *Relay) serveTLS(c net.Conn) {
	c.SetReadDeadline(time.Now().Add(headerTimeout)) //nolint:errcheck

	br := bufio.NewReaderSize(c, maxHello)

	hdr, err := br.Peek(5)
	if err != nil {
		c.Close() //nolint:errcheck

		return
	}

	recLen := int(binary.BigEndian.Uint16(hdr[3:5]))

	rec, err := br.Peek(5 + min(recLen, maxHello-5))
	if err != nil {
		c.Close() //nolint:errcheck

		return
	}

	sni, err := parseSNI(rec)
	if err != nil || sni == "" {
		r.cfg.Logger.Printf("tls from %s: no SNI (%v); closed", c.RemoteAddr(), err)
		c.Close() //nolint:errcheck

		return
	}

	c.SetReadDeadline(time.Time{}) //nolint:errcheck

	target := net.JoinHostPort(sni, "443")

	up, err := upstreamDialer.Dial("tcp", target)
	if err != nil {
		r.cfg.Logger.Printf("tls %s from %s: %v", target, c.RemoteAddr(), err)
		c.Close() //nolint:errcheck

		return
	}

	r.cfg.Logger.Printf("tls %s from %s", target, c.RemoteAddr())
	splice(&bufferedConn{Conn: c, r: br}, up)
}

var errNotHello = errors.New("not a TLS ClientHello")

// parseSNI extracts the server_name from a TLS record holding a ClientHello. It returns "" with
// no error when the hello carries no SNI extension. Pure, for unit tests.
func parseSNI(rec []byte) (string, error) {
	// Record header: type(1)=22 handshake, version(2), length(2).
	if len(rec) < 5 || rec[0] != 22 {
		return "", errNotHello
	}

	b := rec[5:]
	// Handshake header: type(1)=1 client_hello, length(3).
	if len(b) < 4 || b[0] != 1 {
		return "", errNotHello
	}

	b = b[4:]

	// client_version(2) + random(32)
	if len(b) < 34 {
		return "", errNotHello
	}

	b = b[34:]

	skip := func(lenBytes int) bool {
		if len(b) < lenBytes {
			return false
		}

		n := 0
		for i := range lenBytes {
			n = n<<8 | int(b[i])
		}

		if len(b) < lenBytes+n {
			return false
		}

		b = b[lenBytes+n:]

		return true
	}

	// session_id, cipher_suites, compression_methods
	if !skip(1) || !skip(2) || !skip(1) {
		return "", errNotHello
	}

	if len(b) < 2 {
		return "", nil // no extensions
	}

	extLen := int(binary.BigEndian.Uint16(b))
	b = b[2:]

	if len(b) < extLen {
		return "", errNotHello
	}

	b = b[:extLen]

	for len(b) >= 4 {
		typ := binary.BigEndian.Uint16(b)
		l := int(binary.BigEndian.Uint16(b[2:]))
		b = b[4:]

		if len(b) < l {
			return "", errNotHello
		}

		if typ == 0 { // server_name
			return firstHostName(b[:l])
		}

		b = b[l:]
	}

	return "", nil
}

func firstHostName(e []byte) (string, error) {
	if len(e) < 2 {
		return "", errNotHello
	}

	e = e[2:] // server_name_list length

	for len(e) >= 3 {
		typ := e[0]
		l := int(binary.BigEndian.Uint16(e[1:]))
		e = e[3:]

		if len(e) < l {
			return "", errNotHello
		}

		if typ == 0 {
			return string(e[:l]), nil
		}

		e = e[l:]
	}

	return "", nil
}
