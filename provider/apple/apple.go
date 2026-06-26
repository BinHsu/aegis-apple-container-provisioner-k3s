// SPDX-License-Identifier: MIT

// Package apple boots k3s clusters on Apple's `container` runtime
// (Virtualization.framework micro-VMs, one micro-VM per node).
//
// It is the sibling of aegis-talos-apple-container-provisioner, and deliberately
// mirrors that repo's shape: a provider/apple package that execs the `container`
// CLI (the qemu-provider exec pattern), a cmd driver, and a G-gate verification log.
//
// The KEY difference from the Talos sibling: k3s has NO pluggable provisioner
// interface upstream (Talos has pkg/provision; k3s has nothing equivalent). So this
// is a STANDALONE launcher, not an interface implementation — there is no
// compile-time `var _ Provisioner = ...` assertion to keep, and the lifecycle
// (server-first, readiness-gate, then agents) is encoded here rather than inherited
// from a framework.
//
// SPIKE DRAFT — NOT yet run end to end. Every launch-recipe and lifecycle assumption
// is a hypothesis until the G-gates in docs/VERIFICATION.md are executed. Search this
// tree for "UNVERIFIED" to find every such assumption.
package apple

import "context"

// ProviderName is the name this launcher records in saved state.
const ProviderName = "apple-container-k3s"

// firstInterface is the in-VM NIC name the node sees.
//
// UNVERIFIED ASSUMPTION: carried over from the Talos sibling (which saw "eth0" under
// apple/container). Not yet confirmed that rancher/k3s sees the same NIC name.
const firstInterface = "eth0"

// provisioner drives Apple's `container` CLI to boot k3s nodes.
//
// Like the Talos sibling it holds no mapped host ports: apple/container nodes get
// vmnet IPs reachable directly from the host (the Talos spike verified host -> node
// :6443; UNVERIFIED that the same reachability holds for the k3s API server).
type provisioner struct {
	// containerCLI is the `container` binary we exec (the qemu-provider exec pattern).
	containerCLI string
}

// NewProvisioner initializes the apple/container k3s launcher.
//
//nolint:revive // ctx kept for signature parity with the Talos sibling / future use.
func NewProvisioner(ctx context.Context) (*provisioner, error) {
	return &provisioner{
		containerCLI: "container",
	}, nil
}

// Close releases resources. The `container` daemon is long-lived and host-managed
// (launchd), and we exec a CLI rather than holding a client handle, so there is
// nothing to close. Mirrors the Talos sibling.
func (p *provisioner) Close() error {
	return nil
}
