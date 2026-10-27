// SPDX-License-Identifier: MIT

package egress

import (
	"fmt"
	"net"
	"net/netip"
	"syscall"
)

// upstreamDialer dials the public internet only. The address check runs in Control, i.e. on the
// IP actually being connected after DNS resolution, so a guest cannot reach the host's loopback
// services, the LAN, or the bridge itself through the relay (by literal IP, SNI, Host header or a
// name that resolves to such an address).
var upstreamDialer = &net.Dialer{Timeout: dialTimeout, Control: publicOnly}

func publicOnly(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("egress: unparsable upstream %q: %w", address, err)
	}

	if !isPublic(ap.Addr()) {
		return fmt.Errorf("egress: upstream %s is not a public address; refused", ap.Addr())
	}

	return nil
}

// isPublic reports whether a is a globally routable unicast address. Pure, for unit tests.
func isPublic(a netip.Addr) bool {
	a = a.Unmap()

	return a.IsGlobalUnicast() && !a.IsPrivate() && !a.IsLoopback() && !a.IsLinkLocalUnicast() &&
		!a.IsMulticast() && !a.IsUnspecified()
}
