// SPDX-License-Identifier: MIT

package apple

import (
	"context"
	"fmt"
	"strings"
)

// kernelFS is one pseudo-filesystem that eBPF tooling expects at its conventional mount point.
type kernelFS struct {
	FSType string // as listed in /proc/filesystems and passed to mount -t
	Target string
}

// ebpfKernelFS lists the pseudo-filesystems a systemd-booted Linux host mounts and eBPF agents
// rely on: debugfs/tracefs for kprobe/uprobe/tracepoint attach and offset discovery, bpffs for
// pinned maps. Under apple/container (kernel 6.18 arm64, measured 2026-10-27) the directories exist
// but nothing is mounted on them. Order matters only for readability; the three are independent.
var ebpfKernelFS = []kernelFS{
	{FSType: "debugfs", Target: "/sys/kernel/debug"},
	{FSType: "tracefs", Target: "/sys/kernel/tracing"},
	{FSType: "bpf", Target: "/sys/fs/bpf"},
}

// missingKernelFS returns the entries of want that the kernel supports (per /proc/filesystems)
// but that are not yet mounted at their target (per /proc/mounts). Pure, so the decision is
// unit-testable. A filesystem the kernel does not offer is skipped, not reported: on such a
// kernel there is nothing to mount and node preparation must not fail for it.
func missingKernelFS(procMounts, procFilesystems string, want []kernelFS) []kernelFS {
	supported := map[string]bool{}

	for _, line := range strings.Split(procFilesystems, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 {
			supported[fields[len(fields)-1]] = true // "nodev\tdebugfs" or "\text4"
		}
	}

	mounted := map[string]bool{}

	for _, line := range strings.Split(procMounts, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 {
			mounted[fields[1]] = true // "<source> <target> <fstype> ..."
		}
	}

	var missing []kernelFS

	for _, fs := range want {
		if supported[fs.FSType] && !mounted[fs.Target] {
			missing = append(missing, fs)
		}
	}

	return missing
}

// kernelFSMountArgs is the in-guest command that mounts one pseudo-filesystem.
func kernelFSMountArgs(fs kernelFS) []string {
	return []string{"mount", "-t", fs.FSType, fs.FSType, fs.Target}
}

// mountEBPFKernelFS (v0.8.0) mounts debugfs, tracefs and bpffs inside a node where they are
// missing. Idempotent: anything already mounted is left alone, so re-running it on -start or a
// repeated preparation step changes nothing. It runs after `/` is made rshared, so the new mounts
// propagate into pods that bind-mount the host's /sys (eBPF DaemonSets).
func (p *provisioner) mountEBPFKernelFS(ctx context.Context, id string) error {
	mounts, err := p.exec(ctx, id, "cat", "/proc/mounts")
	if err != nil {
		return fmt.Errorf("reading /proc/mounts: %w", err)
	}

	filesystems, err := p.exec(ctx, id, "cat", "/proc/filesystems")
	if err != nil {
		return fmt.Errorf("reading /proc/filesystems: %w", err)
	}

	for _, fs := range missingKernelFS(mounts, filesystems, ebpfKernelFS) {
		if _, err := p.exec(ctx, id, kernelFSMountArgs(fs)...); err != nil {
			return fmt.Errorf("mounting %s on %s: %w", fs.FSType, fs.Target, err)
		}
	}

	return nil
}
