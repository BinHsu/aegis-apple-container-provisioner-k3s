// SPDX-License-Identifier: MIT

package apple

import (
	"net/netip"
	"strings"
	"testing"
)

func TestParseNetworkGateway(t *testing.T) {
	got, err := parseNetworkGateway(`[{"id":"tw-poc-net","status":{"ipv4Gateway":"192.168.200.1","ipv4Subnet":"192.168.200.0/24"}}]`)
	if err != nil || got != netip.MustParseAddr("192.168.200.1") {
		t.Fatalf("got %v, %v", got, err)
	}

	for _, bad := range []string{`[]`, `[{"status":{}}]`, `not json`} {
		if _, err := parseNetworkGateway(bad); err == nil {
			t.Errorf("parseNetworkGateway(%s) accepted", bad)
		}
	}
}

func TestIsRelayCommand(t *testing.T) {
	for line, want := range map[string]bool{
		"/Users/x/k3s/_out/ac-egress -listen 192.168.200.1\n": true,
		"ac-egress -listen 192.168.64.1":                      true,
		"/usr/bin/python3 -m http.server":                     false,
		"/bin/zsh -c ac-egress -listen 1.2.3.4":               false, // pid reused by a shell: never signal
		"ac-egress -guest-script":                             false,
		"":                                                    false,
	} {
		if isRelayCommand(line) != want {
			t.Errorf("isRelayCommand(%q) != %v", line, want)
		}
	}
}

func TestClusterHostsBlock(t *testing.T) {
	nodes := []NodeInfo{
		{Name: "tw-poc-server-1", IPs: []netip.Addr{netip.MustParseAddr("192.168.200.4")}},
		{Name: "no-ip"},
		{Name: "tw-poc-agent-1", IPs: []netip.Addr{netip.MustParseAddr("192.168.200.5")}},
	}

	got := clusterHostsBlock(nodes, "aegis")
	want := "192.168.200.4 tw-poc-server-1.aegis tw-poc-server-1 # k3ac\n192.168.200.5 tw-poc-agent-1.aegis tw-poc-agent-1 # k3ac\n"

	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}

	if strings.Count(got, hostsMarker) != 2 {
		t.Fatal("every line must carry the marker so a refresh can remove it")
	}
}
