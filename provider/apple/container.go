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

// Label keys stamped on every container (buildRunArgs) and named volume (volumeCreate)
// this provisioner creates. They are the single source of truth for the destroy label
// sweep, which cleans orphaned resources from a Create that failed before saveState.
const (
	labelOwned       = "k3s.owned"        // k3s.owned=true marks resources this tool owns
	labelClusterName = "k3s.cluster.name" // k3s.cluster.name=<name> scopes them to one cluster
)

// clusterLabelSelector is the "key=value" label that identifies every container and
// volume created for a cluster. create stamps it; destroy sweeps by it.
func clusterLabelSelector(clusterName string) string {
	return labelClusterName + "=" + clusterName
}

// volumeLabels is the label set stamped on each node's named volume at create time,
// so the destroy label sweep can find it even when state.json is absent.
func volumeLabels(clusterName string) []string {
	return []string{clusterLabelSelector(clusterName), labelOwned + "=true"}
}

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
// trimmed stdout. Used for:
//   - sysctl net.ipv4.ip_forward=1 (mandatory; the kiac spike proved it)
//
// G1 HARDWARE FINDING (2026-06-26): `container exec` mangles args for the rancher/k3s
// image because k3s is a multi-call binary (kubectl and crictl are symlinks to the same
// k3s binary). The container entrypoint runs k3s directly; exec prepends it again, so
// `container exec <id> k3s kubectl ...` becomes effectively `k3s k3s kubectl ...` and
// fails with "unknown command 'kubectl' for 'kubectl'". Only use exec for commands whose
// first argument is NOT a k3s multi-call subcommand — sysctl passes cleanly because it
// is a separate system binary, not a k3s symlink. Readiness is probed via containerCP
// (see waitForReady in create.go), which has no such restriction.
func (p *provisioner) exec(ctx context.Context, id string, args ...string) (string, error) {
	full := append([]string{"exec", id}, args...)

	return p.run(ctx, full...)
}

// containerCP copies a path from inside a container to the host via
// `container cp <src> <dst>`. The primary use is the readiness probe in waitForReady:
// k3s writes /etc/rancher/k3s/k3s.yaml only once the API server is fully initialized
// (CA issued, control-plane healthy), so a successful cp is a reliable "server is up"
// signal — and simultaneously delivers the operator's kubeconfig without relying on
// `container exec` (which mangles the rancher/k3s multi-call binary's args).
func (p *provisioner) containerCP(ctx context.Context, src, dst string) error {
	_, err := p.run(ctx, "cp", src, dst)

	return err
}

// containerInspect is the minimal subset of `container inspect <id>` JSON we consume.
// Schema carried over from the Talos sibling's G3 finding:
// `.[0].status.networks[0].ipv4Address` == "192.168.64.x/24".
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

// volumeCreate creates a named volume (`container volume create [--label k=v ...] <name>`).
// Apple named volumes are block-backed ext4 owned by the guest root, so k3s can write
// its datastore without hitting the virtio-fs chmod restriction that host-path bind-mounts
// suffer from (guest chmod returns EPERM on a virtio-fs share).
// Labels (k3s.cluster.name, k3s.owned) let the destroy label sweep find these volumes
// when state.json is absent.
func (p *provisioner) volumeCreate(ctx context.Context, name string, labels ...string) error {
	_, err := p.run(ctx, volumeCreateArgs(name, labels...)...)

	return err
}

// volumeCreateArgs builds the `container volume create` argument vector. Pure so the
// label flags and trailing positional <name> can be unit-tested without the CLI.
// The name MUST be last (it is the positional argument).
func volumeCreateArgs(name string, labels ...string) []string {
	args := []string{"volume", "create"}

	for _, l := range labels {
		args = append(args, "--label", l)
	}

	return append(args, name)
}

// volumeExists reports whether a named volume exists, via `container volume inspect <name>`:
// success means it exists; a "not found" error means it does not. Any other error propagates.
func (p *provisioner) volumeExists(ctx context.Context, name string) (bool, error) {
	_, err := p.run(ctx, "volume", "inspect", name)
	if err == nil {
		return true, nil
	}

	if strings.Contains(err.Error(), "not found") {
		return false, nil
	}

	return false, err
}

// volumeDelete deletes a named volume, ignoring "not found" so teardown is idempotent.
func (p *provisioner) volumeDelete(ctx context.Context, name string) error {
	_, err := p.run(ctx, "volume", "delete", name)
	if err != nil && strings.Contains(err.Error(), "not found") {
		return nil
	}

	return err
}

// Label filtering: the `container` CLI's `list` and `volume list` have NO native
// --label/--filter flag (verified from `container list --help` / `container volume
// list --help`). So we list everything as JSON and match labels client-side. Labels
// live under `.configuration.labels` for both resources; a container's identity is
// `.configuration.id`, a volume's is `.configuration.name`.

// containerListItem is the minimal subset of `container list --all --format json` we consume.
type containerListItem struct {
	Configuration struct {
		ID     string            `json:"id"`
		Labels map[string]string `json:"labels"`
	} `json:"configuration"`
}

// volumeListItem is the minimal subset of `container volume list --format json` we consume.
type volumeListItem struct {
	Configuration struct {
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
	} `json:"configuration"`
}

// listContainersByLabel returns the IDs of all containers (running or not) whose labels
// match the "key=value" selector. There is no native CLI filter, so it lists
// `--all --format json` and matches client-side.
func (p *provisioner) listContainersByLabel(ctx context.Context, selector string) ([]string, error) {
	out, err := p.run(ctx, "list", "--all", "--format", "json")
	if err != nil {
		return nil, err
	}

	return containersMatchingLabel(out, selector)
}

// listVolumesByLabel returns the names of all volumes whose labels match the "key=value"
// selector. Same client-side approach as listContainersByLabel (no native filter).
func (p *provisioner) listVolumesByLabel(ctx context.Context, selector string) ([]string, error) {
	out, err := p.run(ctx, "volume", "list", "--format", "json")
	if err != nil {
		return nil, err
	}

	return volumesMatchingLabel(out, selector)
}

// containersMatchingLabel parses `container list` JSON and returns the IDs whose labels
// satisfy the selector. Pure (JSON in, IDs out) so the filter is unit-testable.
func containersMatchingLabel(jsonOut, selector string) ([]string, error) {
	key, value, ok := strings.Cut(selector, "=")
	if !ok {
		return nil, fmt.Errorf("invalid label selector %q (want key=value)", selector)
	}

	var items []containerListItem
	if err := json.Unmarshal([]byte(jsonOut), &items); err != nil {
		return nil, fmt.Errorf("parsing container list JSON: %w", err)
	}

	var matches []string

	for _, it := range items {
		if it.Configuration.Labels[key] == value {
			matches = append(matches, it.Configuration.ID)
		}
	}

	return matches, nil
}

// volumesMatchingLabel parses `container volume list` JSON and returns the names whose
// labels satisfy the selector. Pure so the filter is unit-testable without the CLI.
func volumesMatchingLabel(jsonOut, selector string) ([]string, error) {
	key, value, ok := strings.Cut(selector, "=")
	if !ok {
		return nil, fmt.Errorf("invalid label selector %q (want key=value)", selector)
	}

	var items []volumeListItem
	if err := json.Unmarshal([]byte(jsonOut), &items); err != nil {
		return nil, fmt.Errorf("parsing volume list JSON: %w", err)
	}

	var matches []string

	for _, it := range items {
		if it.Configuration.Labels[key] == value {
			matches = append(matches, it.Configuration.Name)
		}
	}

	return matches, nil
}

// dnsDomainInList parses the JSON output of `container system dns list --format json`
// and reports whether domain is present. Pure function — extracted from dnsDomainExists
// so the JSON parsing is unit-testable without the CLI.
func dnsDomainInList(jsonOut, domain string) (bool, error) {
	var domains []string
	if err := json.Unmarshal([]byte(jsonOut), &domains); err != nil {
		return false, fmt.Errorf("parsing DNS domain list: %w", err)
	}

	for _, d := range domains {
		if d == domain {
			return true, nil
		}
	}

	return false, nil
}

// dnsDomainExists reports whether domain is registered with the Apple container system
// DNS (`container system dns list --format json`). The CLI returns a JSON array of strings.
func (p *provisioner) dnsDomainExists(ctx context.Context, domain string) (bool, error) {
	out, err := p.run(ctx, "system", "dns", "list", "--format", "json")
	if err != nil {
		return false, fmt.Errorf("listing container DNS domains: %w", err)
	}

	return dnsDomainInList(out, domain)
}

// checkDNSDomain returns a clear, actionable error when domain is not registered. An
// absent domain means Apple's container DNS forwarding has no resolver entry for it,
// so host-to-container FQDN lookups will silently fall through to public DNS instead
// of reaching the node.
func (p *provisioner) checkDNSDomain(ctx context.Context, domain string) error {
	ok, err := p.dnsDomainExists(ctx, domain)
	if err != nil {
		return err
	}

	if !ok {
		return fmt.Errorf("DNS domain %q not found — run: sudo container system dns create %s", domain, domain)
	}

	return nil
}
