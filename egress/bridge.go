// SPDX-License-Identifier: MIT

package egress

import (
	"fmt"
	"net"
	"net/netip"
	"strings"
)

// BridgeFor returns the host interface name that carries ip and that interface's subnet, and
// refuses unless the interface is a vmnet bridge ("bridge*"). This is the inbound-safety guard:
// the relay must never listen on a LAN, Wi-Fi, VPN or loopback address.
func BridgeFor(ip netip.Addr) (string, netip.Prefix, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return "", netip.Prefix{}, err
	}

	for _, ifc := range ifs {
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}

		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}

			p, err := netip.ParsePrefix(ipn.String())
			if err != nil || p.Addr().Unmap() != ip {
				continue
			}

			if !IsBridgeName(ifc.Name) {
				return "", netip.Prefix{}, fmt.Errorf("egress: %s is on %s, not a vmnet bridge; refusing to listen there", ip, ifc.Name)
			}

			return ifc.Name, p.Masked(), nil
		}
	}

	return "", netip.Prefix{}, fmt.Errorf("egress: %s is not assigned to any host interface (is a VM on that network running?)", ip)
}

// IsBridgeName reports whether an interface name is a macOS vmnet bridge (bridge100, bridge101…).
// bridge0 is the Thunderbolt bridge and is excluded. Pure.
func IsBridgeName(name string) bool {
	return strings.HasPrefix(name, "bridge") && name != "bridge0"
}
