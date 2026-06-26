// SPDX-License-Identifier: MIT

package apple

import (
	"context"
	"fmt"
	"net/netip"
	"path/filepath"
	"time"
)

// defaultK3sImage is the rancher/k3s node image.
//
// PLACEHOLDER TO VERIFY (UNVERIFIED): this is a plausible recent stable tag of the
// v1.32.x-k3s1 form, NOT a tag confirmed to exist or to boot under apple/container.
// Verify the exact tag against https://hub.docker.com/r/rancher/k3s/tags before use.
// The rancher/k3s image entrypoint runs the `k3s` binary directly — there is NO
// systemd/openrc in this image (unlike a full distro), which is why readiness must be
// polled via `container exec` rather than a service-manager query (see create.go).
const defaultK3sImage = "rancher/k3s:v1.32.5-k3s1"

// defaultClusterDNS is the stable name added as a --tls-san so the API server cert
// survives vmnet IP changes. UNVERIFIED that this name resolves anywhere; it only has
// to be a stable string baked into the cert SAN list.
const defaultClusterDNS = "aegis-k3s.local"

// k3sDatastoreMount is the in-node path k3s uses for its sqlite datastore and all
// server state. We bind-mount a host dir here so the datastore survives container
// stop/start AND rm. Do NOT change without re-checking G3.
const k3sDatastoreMount = "/var/lib/rancher/k3s"

// k3sAPIPort is the k3s API server / join port.
const k3sAPIPort = 6443

// nodeTmpfsPaths returns the in-VM paths mounted as writable tmpfs for a k3s node.
//
// This is the central recipe DIVERGENCE from the Talos sibling. The Talos node needed
// /run,/system,/tmp,/var,/system/state plus the CNI/k8s overlay dirs as tmpfs. For k3s:
//   - ONLY /run and /tmp. k3s repopulates these at boot and they must be real mount
//     points, same reasoning as Talos.
//   - DO NOT tmpfs /var. k3s's datastore and all server state live under
//     /var/lib/rancher/k3s; that path is a persistent host BIND-MOUNT (see buildRunArgs),
//     not tmpfs. tmpfs-ing /var would shadow the bind-mount and wipe the datastore on
//     every restart — the exact opposite of what gate G3/G5 needs.
//   - ALL Talos-specific paths are dropped: /system, /system/state, /etc/cni,
//     /etc/kubernetes, /usr/libexec/kubernetes. k3s manages its own CNI (flannel) and
//     kubelet dirs inside the writable rootfs; it has no Talos /system layout.
//   - CARRY OVER the Talos lesson: do NOT tmpfs /opt. tmpfs does not copy-up, so it
//     would shadow any shipped /opt content; leaving /opt on the writable rootfs
//     preserves it. (k3s's CNI lives under /var/lib/rancher/k3s/data, not /opt/cni/bin,
//     so this is belt-and-suspenders — UNVERIFIED whether k3s touches /opt at all.)
func nodeTmpfsPaths() []string {
	return []string{"/run", "/tmp"}
}

// nodeDatastoreHostPath is the host directory bind-mounted into the node at
// k3sDatastoreMount. Per-node so a multi-node cluster doesn't collide. Removing this
// dir is what makes Destroy truly clean (vs. leaving it for a restart) — see destroy.go.
func nodeDatastoreHostPath(cfg ClusterConfig, node NodeConfig) string {
	return filepath.Join(cfg.StateDir, cfg.Name, node.Name, "k3s")
}

// buildRunArgs assembles the `container run` argument vector for one k3s node. It is a
// pure function so the recipe can be unit-tested (incl. BVA on node fields) without
// launching a VM — same discipline as the Talos sibling's buildRunArgs.
//
// serverURL is "" for a server node and "https://<server-ip>:6443" for an agent. The
// caller (Create) discovers the server IP after launching the server, then passes it
// when launching agents.
func buildRunArgs(cfg ClusterConfig, node NodeConfig, serverURL string) []string {
	args := []string{
		"run", "--detach",
		"--name", node.Name,
		// G1: k3s's embedded containerd needs CAP_SYS_ADMIN (mount/pivot_root/cgroup).
		// apple/container has no --privileged; --cap-add ALL is the equivalent, same as
		// the Talos sibling. THE central unknown is whether even ALL caps are enough
		// under Apple's vminitd — see G1.
		"--cap-add", "ALL",
	}

	// Memory limit. CARRY OVER the Talos G4 finding: the value MUST use the "MB" suffix;
	// a bare "M" (e.g. "2048M") is rejected by the Apple container CLI.
	if node.Memory > 0 {
		args = append(args, "--memory", fmt.Sprintf("%dMB", node.Memory/(1024*1024)))
	}

	if node.NanoCPUs > 0 {
		args = append(args, "--cpus", fmt.Sprintf("%d", node.NanoCPUs/(1000*1000*1000)))
	}

	for _, path := range nodeTmpfsPaths() {
		args = append(args, "--tmpfs", path)
	}

	// DATASTORE PERSISTENCE: bind-mount a host dir into the node so /var/lib/rancher/k3s
	// (the sqlite datastore + all server state) survives container stop/start AND rm.
	// (`container run` supports -v/--volume; the rootfs.ext4 also persists across
	// stop/start, but a host bind-mount is the robust choice — see G3.)
	args = append(args, "--volume", nodeDatastoreHostPath(cfg, node)+":"+k3sDatastoreMount)

	// Labels: k3s.* replacing the Talos sibling's talos.* scheme. Node IDs are also
	// tracked in state.json so teardown does not depend on label-listing (the CLI does
	// not support label filters — Talos sibling finding).
	args = append(args,
		"--label", "k3s.owned=true",
		"--label", "k3s.cluster.name="+cfg.Name,
		"--label", "k3s.role="+node.Role.String(),
	)

	// Environment. K3S_TOKEN is the shared cluster secret (server presets it, agents
	// reuse it). The Talos sibling's PLATFORM=container and TALOSSKU are REMOVED — they
	// are Talos-only and meaningless to k3s.
	args = append(args, "--env", "K3S_TOKEN="+cfg.Token)

	// Agents additionally need to know where the server is. The server node gets no
	// K3S_URL (it IS the server).
	if node.Role == RoleAgent {
		args = append(args, "--env", "K3S_URL="+serverURL)
	}

	if cfg.Network != "" {
		args = append(args, "--network", cfg.Network)
	}

	// Image is the next positional argument; the k3s subcommand + flags follow it,
	// because the rancher/k3s entrypoint runs `k3s <args>` directly.
	args = append(args, cfg.Image)

	// k3s subcommand: "server" or "agent".
	args = append(args, node.Role.String())

	// Server-only flags.
	if node.Role == RoleServer {
		// DATASTORE ENGINE: sqlite (k3s default single-server). We deliberately do NOT
		// pass --cluster-init (embedded etcd). Rationale: sqlite has no IP-bound cluster
		// membership, so it survives the vmnet IP-change problem; embedded etcd encodes
		// peer/client URLs bound to IPs and would not come back after a cold restart on a
		// new DHCP address (the IP-stability gap the Talos sibling documented in G3/G5).
		//
		// NETWORKING: host-gw flannel backend. vmnet places all node VMs on the same L2
		// segment, so host-gw L2 routes work and avoid the UNVERIFIED br_netfilter/VXLAN
		// kernel dependency (the default flannel VXLAN backend needs br_netfilter; whether
		// it is present under Apple's kernel is gate G2).
		args = append(args, "--flannel-backend=host-gw")
		// --tls-san: pin a stable name into the API server cert SANs so the cert stays
		// valid across IP changes (the cert is generated from cluster state, and without
		// a stable SAN a new DHCP IP would not be covered).
		args = append(args, "--tls-san", cfg.ClusterDNS)
	}

	return args
}

// ipDiscoveryTimeout bounds how long we wait for vmnet DHCP to assign a node its address.
const ipDiscoveryTimeout = 30 * time.Second

// waitForIPv4 polls `container inspect` until the node has a vmnet IPv4 or the timeout
// elapses. Identical pattern to the Talos sibling.
func (p *provisioner) waitForIPv4(ctx context.Context, id string) (netip.Addr, error) {
	deadline := time.Now().Add(ipDiscoveryTimeout)

	for {
		addr, err := p.inspectIPv4(ctx, id)
		if err == nil {
			return addr, nil
		}

		if time.Now().After(deadline) {
			return netip.Addr{}, fmt.Errorf("timed out waiting for %q to get an IPv4: %w", id, err)
		}

		select {
		case <-ctx.Done():
			return netip.Addr{}, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}
