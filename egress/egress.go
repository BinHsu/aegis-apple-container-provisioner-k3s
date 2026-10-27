// SPDX-License-Identifier: MIT

// Package egress is a userspace egress relay for Apple `container` VMs on hosts where the vmnet
// NAT path is dead but ordinary host-process sockets still reach the internet — the measured case
// is Cloudflare WARP connected on macOS (see docs/ADR/0004). It knows nothing about k3s: it serves
// any guest on the bridge network it is bound to.
//
// The relay listens ONLY on the vmnet bridge (gateway) address and offers three services:
//
//   - HTTP  (default :15080): transparent HTTP (routes by the Host header) and an explicit
//     HTTP proxy (CONNECT and absolute-form requests), so HTTP_PROXY users also work.
//   - TLS   (default :15443): transparent TLS passthrough routed by the ClientHello SNI. Bytes are
//     never decrypted here; whatever the host network does to TLS (WARP inspection) still applies,
//     so guests need that CA (k3ac -ca-dir).
//   - DNS   (default :15053, UDP and TCP): forwards raw queries to the host's own resolvers.
//
// Guests are steered to it by the iptables script from GuestScript (DNAT of outbound 80/443/53
// to non-private destinations), so no per-process proxy configuration is needed.
//
// The package imports only the Go standard library and must never import k3ac code.
package egress

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"time"
)

// Default ports. High, unprivileged, bound only on the bridge address.
const (
	DefaultHTTPPort = 15080
	DefaultTLSPort  = 15443
	DefaultDNSPort  = 15053
)

// dialTimeout bounds every upstream dial; idleTimeout bounds a stalled header read.
const (
	dialTimeout   = 15 * time.Second
	headerTimeout = 30 * time.Second
	dnsTimeout    = 5 * time.Second
)

// Config configures a Relay.
type Config struct {
	// ListenIP is the bridge address to bind (e.g. 192.168.200.1). Required; never unspecified.
	ListenIP netip.Addr
	// AllowedSource is the guest subnet allowed to use the relay (e.g. 192.168.200.0/24).
	// Connections from elsewhere are closed. Required.
	AllowedSource netip.Prefix
	HTTPPort      int
	TLSPort       int
	DNSPort       int
	// DNSUpstreams are host resolvers as "ip:port". Empty = HostResolvers().
	DNSUpstreams []string
	// Logger receives one line per relayed flow and every error. Nil = discard.
	Logger *log.Logger
}

// Relay is a running egress relay.
type Relay struct {
	cfg       Config
	listeners []net.Listener
	udp       net.PacketConn
	wg        sync.WaitGroup
}

func (c *Config) defaults() error {
	if !c.ListenIP.IsValid() || c.ListenIP.IsUnspecified() {
		return errors.New("egress: ListenIP must be a concrete bridge address, never unspecified")
	}

	if !c.AllowedSource.IsValid() {
		return errors.New("egress: AllowedSource subnet is required")
	}

	if c.HTTPPort == 0 {
		c.HTTPPort = DefaultHTTPPort
	}

	if c.TLSPort == 0 {
		c.TLSPort = DefaultTLSPort
	}

	if c.DNSPort == 0 {
		c.DNSPort = DefaultDNSPort
	}

	if len(c.DNSUpstreams) == 0 {
		ups, err := HostResolvers("/etc/resolv.conf")
		if err != nil {
			return err
		}

		c.DNSUpstreams = ups
	}

	if c.Logger == nil {
		c.Logger = log.New(io.Discard, "", 0)
	}

	return nil
}

// Start binds every listener and serves in the background until ctx is cancelled or Close is
// called. A bind failure closes whatever was already bound.
func Start(ctx context.Context, cfg Config) (*Relay, error) {
	if err := cfg.defaults(); err != nil {
		return nil, err
	}

	r := &Relay{cfg: cfg}
	addr := func(port int) string { return net.JoinHostPort(cfg.ListenIP.String(), strconv.Itoa(port)) }

	var lc net.ListenConfig

	for _, s := range []struct {
		port  int
		serve func(net.Conn)
	}{{cfg.HTTPPort, r.serveHTTP}, {cfg.TLSPort, r.serveTLS}, {cfg.DNSPort, r.serveDNSTCP}} {
		ln, err := lc.Listen(ctx, "tcp", addr(s.port))
		if err != nil {
			r.Close() //nolint:errcheck

			return nil, fmt.Errorf("egress: listen %s: %w", addr(s.port), err)
		}

		r.listeners = append(r.listeners, ln)
		r.wg.Add(1)

		go r.acceptLoop(ln, s.serve)
	}

	pc, err := lc.ListenPacket(ctx, "udp", addr(cfg.DNSPort))
	if err != nil {
		r.Close() //nolint:errcheck

		return nil, fmt.Errorf("egress: listen udp %s: %w", addr(cfg.DNSPort), err)
	}

	r.udp = pc
	r.wg.Add(1)

	go r.serveDNSUDP(pc)

	go func() {
		<-ctx.Done()
		r.Close() //nolint:errcheck
	}()

	cfg.Logger.Printf("egress relay on %s (http %d, tls %d, dns %d tcp+udp), guests %s, dns upstreams %v",
		cfg.ListenIP, cfg.HTTPPort, cfg.TLSPort, cfg.DNSPort, cfg.AllowedSource, cfg.DNSUpstreams)

	return r, nil
}

// Close stops every listener. In-flight flows finish on their own.
func (r *Relay) Close() error {
	for _, ln := range r.listeners {
		ln.Close() //nolint:errcheck
	}

	if r.udp != nil {
		r.udp.Close() //nolint:errcheck
	}

	return nil
}

// Wait blocks until every listener loop has exited.
func (r *Relay) Wait() { r.wg.Wait() }

func (r *Relay) allowed(a net.Addr) bool {
	ap, err := netip.ParseAddrPort(a.String())
	if err != nil {
		return false
	}

	return r.cfg.AllowedSource.Contains(ap.Addr().Unmap())
}

func (r *Relay) acceptLoop(ln net.Listener, serve func(net.Conn)) {
	defer r.wg.Done()

	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}

			r.cfg.Logger.Printf("accept: %v", err)

			continue
		}

		if !r.allowed(c.RemoteAddr()) {
			r.cfg.Logger.Printf("refused %s: not in %s", c.RemoteAddr(), r.cfg.AllowedSource)
			c.Close() //nolint:errcheck

			continue
		}

		go serve(c)
	}
}

// splice copies both directions until either side finishes, then closes both.
func splice(a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		io.Copy(dst, src) //nolint:errcheck
		if hc, ok := dst.(interface{ CloseWrite() error }); ok {
			hc.CloseWrite() //nolint:errcheck
		}
		done <- struct{}{}
	}

	go cp(a, b)
	go cp(b, a)
	<-done
	<-done
	a.Close() //nolint:errcheck
	b.Close() //nolint:errcheck
}
