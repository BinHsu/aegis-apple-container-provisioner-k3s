// SPDX-License-Identifier: MIT

package egress

import (
	"fmt"
	"net/netip"
	"strings"
)

// GuestChain is the iptables nat chain GuestScript owns inside a guest. Re-running the script
// flushes and rebuilds it, so it is idempotent across restarts.
const GuestChain = "AC-EGRESS"

// DefaultBypass is never redirected: loopback, RFC1918, link-local, CGNAT, multicast. Callers add
// their own (pod/service CIDRs are usually already inside 10.0.0.0/8).
var DefaultBypass = []string{
	"127.0.0.0/8", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16",
	"169.254.0.0/16", "100.64.0.0/10", "224.0.0.0/4",
}

// GuestScript returns a POSIX sh script that, run as root inside a Linux guest with iptables,
// redirects the guest's outbound plain HTTP (tcp/80), TLS (tcp/443) and DNS (udp+tcp/53) to the
// relay at relayIP. It hooks nat OUTPUT (the guest's own processes) and nat PREROUTING (traffic
// the guest forwards, e.g. containers/pods behind it). Destinations in bypass are left alone.
func GuestScript(relayIP netip.Addr, httpPort, tlsPort, dnsPort int, bypass []string) (string, error) {
	if !relayIP.Is4() {
		return "", fmt.Errorf("egress: relay IP %s must be IPv4", relayIP)
	}

	for _, c := range bypass {
		if _, err := netip.ParsePrefix(c); err != nil {
			return "", fmt.Errorf("egress: bypass %q: %w", c, err)
		}
	}

	var b strings.Builder

	b.WriteString("set -e\n")
	fmt.Fprintf(&b, "iptables -t nat -N %s 2>/dev/null || iptables -t nat -F %s\n", GuestChain, GuestChain)

	for _, c := range bypass {
		fmt.Fprintf(&b, "iptables -t nat -A %s -d %s -j RETURN\n", GuestChain, c)
	}

	r := relayIP.String()
	fmt.Fprintf(&b, "iptables -t nat -A %s -p tcp --dport 80 -j DNAT --to-destination %s:%d\n", GuestChain, r, httpPort)
	fmt.Fprintf(&b, "iptables -t nat -A %s -p tcp --dport 443 -j DNAT --to-destination %s:%d\n", GuestChain, r, tlsPort)
	fmt.Fprintf(&b, "iptables -t nat -A %s -p udp --dport 53 -j DNAT --to-destination %s:%d\n", GuestChain, r, dnsPort)
	fmt.Fprintf(&b, "iptables -t nat -A %s -p tcp --dport 53 -j DNAT --to-destination %s:%d\n", GuestChain, r, dnsPort)

	for _, hook := range []string{"OUTPUT", "PREROUTING"} {
		fmt.Fprintf(&b, "iptables -t nat -C %s -j %s 2>/dev/null || iptables -t nat -I %s 1 -j %s\n", hook, GuestChain, hook, GuestChain)
	}

	return b.String(), nil
}

// GuestRemoveScript undoes GuestScript (best effort, idempotent).
func GuestRemoveScript() string {
	var b strings.Builder

	for _, hook := range []string{"OUTPUT", "PREROUTING"} {
		fmt.Fprintf(&b, "while iptables -t nat -D %s -j %s 2>/dev/null; do :; done\n", hook, GuestChain)
	}

	fmt.Fprintf(&b, "iptables -t nat -F %s 2>/dev/null; iptables -t nat -X %s 2>/dev/null; true\n", GuestChain, GuestChain)

	return b.String()
}
