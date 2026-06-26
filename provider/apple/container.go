// SPDX-License-Identifier: MIT

package apple

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"os/exec"
	"strings"
)

// This file wraps the Apple `container` CLI. We exec the binary rather than call a
// daemon API — the same pattern the Talos sibling and the in-tree qemu provider use.
// apple/container has no Go SDK; it is a Swift runtime exposing a CLI + launchd helper.

// run executes `container <args...>` and returns trimmed stdout. On failure it returns
// an error that includes stderr, so callers surface the CLI's own diagnostics.
// Identical to the Talos sibling's run().
func (p *provisioner) run(ctx context.Context, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer

	cmd := exec.CommandContext(ctx, p.containerCLI, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("container %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}

	return strings.TrimSpace(stdout.String()), nil
}

// exec runs a command inside a node via `container exec <id> <args...>` and returns
// trimmed stdout. The Talos sibling never needed this (machined hid sysctl/readiness
// inside the node); k3s has no systemd, so the launcher must poke the node directly:
//   - sysctl net.ipv4.ip_forward=1 (mandatory; the kiac spike proved it)
//   - readiness polling via `k3s kubectl get --raw /readyz`
//
// UNVERIFIED ASSUMPTION: that `container exec` exists and takes this exact form. The
// Talos sibling verified `run`, `inspect`, `stop`, `rm`, `network`; it never exercised
// `exec`. This is gate G1/G4 in docs/VERIFICATION.md.
func (p *provisioner) exec(ctx context.Context, id string, args ...string) (string, error) {
	full := append([]string{"exec", id}, args...)

	return p.run(ctx, full...)
}

// containerInspect is the minimal subset of `container inspect <id>` JSON we consume.
// Schema carried over from the Talos sibling's G3 finding:
// `.[0].status.networks[0].ipv4Address` == "192.168.64.x/24".
//
// UNVERIFIED ASSUMPTION: that the inspect schema is identical for a rancher/k3s node
// (it is the runtime's schema, not the guest's, so this should hold — but unconfirmed).
type containerInspect struct {
	Status struct {
		Networks []struct {
			IPv4Address string `json:"ipv4Address"`
		} `json:"networks"`
	} `json:"status"`
}

// inspectIPv4 returns the node's vmnet IPv4 address. apple/container assigns it via DHCP
// (no static --ip; the Talos sibling verified this), so the address is only knowable
// after the node is running.
func (p *provisioner) inspectIPv4(ctx context.Context, id string) (netip.Addr, error) {
	out, err := p.run(ctx, "inspect", id)
	if err != nil {
		return netip.Addr{}, err
	}

	var items []containerInspect
	if err := json.Unmarshal([]byte(out), &items); err != nil {
		return netip.Addr{}, fmt.Errorf("parsing inspect for %q: %w", id, err)
	}

	if len(items) == 0 || len(items[0].Status.Networks) == 0 {
		return netip.Addr{}, fmt.Errorf("no network info for %q yet", id)
	}

	cidr := items[0].Status.Networks[0].IPv4Address
	if cidr == "" {
		return netip.Addr{}, fmt.Errorf("no IPv4 assigned to %q yet", id)
	}

	// strip the /prefix; we want the bare address.
	addrStr, _, _ := strings.Cut(cidr, "/")

	addr, err := netip.ParseAddr(addrStr)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("parsing IPv4 %q for %q: %w", cidr, id, err)
	}

	return addr, nil
}

// stop stops a node, ignoring "not found" so teardown is idempotent.
func (p *provisioner) stop(ctx context.Context, id string) error {
	_, err := p.run(ctx, "stop", id)
	if err != nil && strings.Contains(err.Error(), "not found") {
		return nil
	}

	return err
}

// remove removes a node, ignoring "not found" so teardown is idempotent.
func (p *provisioner) remove(ctx context.Context, id string) error {
	_, err := p.run(ctx, "rm", id)
	if err != nil && strings.Contains(err.Error(), "not found") {
		return nil
	}

	return err
}
