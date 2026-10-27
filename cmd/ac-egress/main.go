// SPDX-License-Identifier: MIT

// Command ac-egress runs the egress relay (package egress) for Apple `container` VMs on one vmnet
// bridge. It binds only the bridge address, serves only guests on that bridge's subnet, and exits
// on SIGINT/SIGTERM. It does not touch the guests: print the iptables script with -guest-script
// and run it inside each guest (k3ac -egress does both for a k3s cluster).
//
//	ac-egress -listen 192.168.200.1
//	ac-egress -listen 192.168.200.1 -guest-script | container exec -i <vm> sh
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/BinHsu/aegis-apple-container-provisioner-k3s/egress"
)

type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

func main() {
	if err := run(); err != nil {
		log.Fatalf("ac-egress: %v", err)
	}
}

func run() error {
	var upstreams, bypass listFlag

	listen := flag.String("listen", "", "vmnet bridge (gateway) IPv4 to bind, e.g. 192.168.200.1 (required; must sit on a bridge* interface)")
	httpPort := flag.Int("http-port", egress.DefaultHTTPPort, "transparent HTTP + explicit HTTP/CONNECT proxy port")
	tlsPort := flag.Int("tls-port", egress.DefaultTLSPort, "transparent TLS (SNI passthrough) port")
	dnsPort := flag.Int("dns-port", egress.DefaultDNSPort, "DNS forwarder port (udp+tcp)")
	guestScript := flag.Bool("guest-script", false, "print the guest iptables script for this relay and exit")
	guestRemove := flag.Bool("guest-remove-script", false, "print the script that removes the guest rules and exit")

	flag.Var(&upstreams, "dns-upstream", "DNS upstream ip:port (repeatable; default = host /etc/resolv.conf)")
	flag.Var(&bypass, "bypass", "extra guest destination CIDR never redirected (repeatable; private ranges are always bypassed)")
	flag.Parse()

	if *guestRemove {
		fmt.Print(egress.GuestRemoveScript())

		return nil
	}

	ip, err := netip.ParseAddr(*listen)
	if err != nil {
		return fmt.Errorf("-listen %q: %w", *listen, err)
	}

	if *guestScript {
		s, err := egress.GuestScript(ip, *httpPort, *tlsPort, *dnsPort, append(append([]string{}, egress.DefaultBypass...), bypass...))
		if err != nil {
			return err
		}

		fmt.Print(s)

		return nil
	}

	ifname, subnet, err := egress.BridgeFor(ip)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logger := log.New(os.Stderr, "ac-egress: ", log.LstdFlags)
	logger.Printf("bridge %s (%s)", ifname, subnet)

	r, err := egress.Start(ctx, egress.Config{
		ListenIP: ip, AllowedSource: subnet,
		HTTPPort: *httpPort, TLSPort: *tlsPort, DNSPort: *dnsPort,
		DNSUpstreams: upstreams, Logger: logger,
	})
	if err != nil {
		return err
	}

	<-ctx.Done()
	r.Close() //nolint:errcheck
	r.Wait()
	logger.Printf("stopped")

	return nil
}
