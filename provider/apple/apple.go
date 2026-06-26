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
package apple

import "context"

// ProviderName is the name this launcher records in saved state.
const ProviderName = "apple-container-k3s"

// Config holds constructor-time configuration for the k3s launcher.
type Config struct {
	// DNSDomain, when non-empty, names every container as <node>.<domain> — an FQDN
	// that Apple's container DNS resolves from the host — and uses it as the stable
	// control-plane endpoint (survives cold-restart DHCP IP changes). The domain must
	// be pre-registered before Create: sudo container system dns create <domain>.
	// This registration persists until the next macOS reboot; re-run it after a reboot.
	// Empty string disables FQDN naming and falls back to IP-based naming (v0.1.x).
	DNSDomain string
}

// provisioner drives Apple's `container` CLI to boot k3s nodes.
//
// Like the Talos sibling it holds no mapped host ports: apple/container nodes get
// vmnet IPs reachable directly from the host.
type provisioner struct {
	// containerCLI is the `container` binary we exec (the qemu-provider exec pattern).
	containerCLI string
	// dnsDomain drives FQDN container naming when non-empty. See Config.DNSDomain.
	dnsDomain string
}

// NewProvisioner initializes the apple/container k3s launcher.
//
//nolint:revive // ctx kept for signature parity with the Talos sibling / future use.
func NewProvisioner(ctx context.Context, cfg Config) (*provisioner, error) {
	return &provisioner{
		containerCLI: "container",
		dnsDomain:    cfg.DNSDomain,
	}, nil
}

// Close releases resources. The `container` daemon is long-lived and host-managed
// (launchd), and we exec a CLI rather than holding a client handle, so there is
// nothing to close. Mirrors the Talos sibling.
func (p *provisioner) Close() error {
	return nil
}
