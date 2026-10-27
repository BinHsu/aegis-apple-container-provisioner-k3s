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

func TestMissingKernelFS(t *testing.T) {
	filesystems := "nodev\tsysfs\nnodev\tproc\nnodev\tdebugfs\nnodev\ttracefs\nnodev\tbpf\n\text4\n"

	// Fresh apple/container node: the targets are empty directories, nothing mounted.
	fresh := "/dev/vda / ext4 rw,relatime 0 0\nsysfs /sys sysfs rw,nosuid,nodev,noexec,relatime 0 0\n"

	got := missingKernelFS(fresh, filesystems, ebpfKernelFS)
	if len(got) != 3 {
		t.Fatalf("fresh node: want all 3 missing, got %v", got)
	}

	// Already prepared (second -start): nothing to do — the idempotence guarantee.
	prepared := fresh +
		"debugfs /sys/kernel/debug debugfs rw,relatime 0 0\n" +
		"tracefs /sys/kernel/tracing tracefs rw,relatime 0 0\n" +
		"bpf /sys/fs/bpf bpf rw,relatime 0 0\n"
	if got := missingKernelFS(prepared, filesystems, ebpfKernelFS); len(got) != 0 {
		t.Fatalf("prepared node: want none missing, got %v", got)
	}

	// Partial: only tracefs mounted (debugfs auto-mounting tracefs under debug/tracing must not count).
	partial := fresh +
		"tracefs /sys/kernel/tracing tracefs rw,relatime 0 0\n" +
		"tracefs /sys/kernel/debug/tracing tracefs rw,relatime 0 0\n"
	got = missingKernelFS(partial, filesystems, ebpfKernelFS)
	if len(got) != 2 || got[0].FSType != "debugfs" || got[1].FSType != "bpf" {
		t.Fatalf("partial node: got %v", got)
	}

	// Kernel without tracefs/bpf: skipped, never an attempted (failing) mount.
	got = missingKernelFS(fresh, "nodev\tsysfs\nnodev\tdebugfs\n", ebpfKernelFS)
	if len(got) != 1 || got[0].FSType != "debugfs" {
		t.Fatalf("limited kernel: got %v", got)
	}
}

func TestKernelFSMountArgs(t *testing.T) {
	want := map[string]string{
		"debugfs": "mount -t debugfs debugfs /sys/kernel/debug",
		"tracefs": "mount -t tracefs tracefs /sys/kernel/tracing",
		"bpf":     "mount -t bpf bpf /sys/fs/bpf",
	}

	if len(ebpfKernelFS) != len(want) {
		t.Fatalf("ebpfKernelFS has %d entries, want %d", len(ebpfKernelFS), len(want))
	}

	for _, fs := range ebpfKernelFS {
		if got := strings.Join(kernelFSMountArgs(fs), " "); got != want[fs.FSType] {
			t.Errorf("%s: got %q want %q", fs.FSType, got, want[fs.FSType])
		}
	}
}
