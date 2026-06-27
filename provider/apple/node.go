// SPDX-License-Identifier: MIT

package apple

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

// defaultK3sImage is the rancher/k3s node image.
// G1 VERIFIED (2026-06-26): rancher/k3s:v1.32.5-k3s1 boots fully under Apple
// container 1.0.0 — embedded containerd runs, full control plane + pods come up,
// clean node name `k3sg1`. Use this exact tag.
const defaultK3sImage = "rancher/k3s:v1.32.5-k3s1"

// k3sDatastoreMount is the in-node path k3s uses for its sqlite datastore and all
// server state. We back it with a named volume (block-backed ext4, guest-owned, chmod
// works) — NOT a host-path bind-mount (virtio-fs share, guest chmod returns EPERM).
// Do NOT change without re-checking G3.
const k3sDatastoreMount = "/var/lib/rancher/k3s"

// k3sAPIPort is the k3s API server / join port.
const k3sAPIPort = 6443

// Managed datastore (Postgres) recipe constants. The HA spike (VERIFICATION G9, docs/ADR/0002)
// used exactly these. Image and memory are fixed defaults for now — tuning flags are a
// deliberate follow-up, not part of this cut.
const (
	defaultDatastoreImage       = "postgres:17-alpine"
	datastorePort               = 5432
	datastoreUser               = "kine"
	datastoreDB                 = "kine"
	datastoreDataMount          = "/var/lib/postgresql/data"
	datastorePGDataDir          = "/var/lib/postgresql/data/pgdata" // PGDATA subdir: an ext4 named volume ships a lost+found that blocks initdb at the mount root (G9)
	defaultDatastoreMemoryBytes = 1024 * 1024 * 1024                // 1 GiB
)

// kubeconfigMount is the in-node mount point for a HOST bind-mount of the cluster state
// dir on the server node. k3s writes its admin kubeconfig here (--write-kubeconfig
// kubeconfigMount/kubeconfigFileName) and Create reads it straight off the host
// filesystem. This REPLACES container cp for kubeconfig delivery: cp rides the guest agent
// (vminitd over vsock), which faults under k3s's cold-boot image-extraction I/O and gets
// SIGKILLed mid-transfer, cascading to a whole-daemon stop/rm hang (Apple containerization
// #678/#712, container #861; verified 2026-06-27 — even an un-killed cp dies in the boot
// window). A plain file write to a virtio-fs bind-mount + host-side read does not touch the
// guest agent and was spiked clean (write + chmod + host read all OK). Path avoids /run,
// /tmp (tmpfs) and /var/lib/rancher/k3s (datastore volume).
const (
	kubeconfigMount    = "/mnt/k3s-out"
	kubeconfigFileName = "k3s.yaml"
)

// sanitizeVolumeName lowercases s and replaces every character outside [a-z0-9-] with
// '-', yielding a stable, valid Apple `container` volume name from an arbitrary
// cluster/node identifier. Mirrors the Talos sibling.
func sanitizeVolumeName(s string) string {
	var b strings.Builder

	b.Grow(len(s))

	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}

	return b.String()
}

// nodeVolumeName returns the Apple `container` NAMED VOLUME name backing a node's k3s
// datastore (/var/lib/rancher/k3s). Scheme: <cluster>-<node>-k3s (sanitized). This is
// the single source of truth: buildRunArgs (mount), Create (existence guard + create),
// and Destroy (delete) all derive names here, so the destroy path can never target a
// different volume than the one create provisioned.
//
// Named volumes (not host-path bind-mounts) are mandatory: an Apple `--volume
// <hostpath>:<container>` is a virtio-fs share the guest cannot chmod (EPERM). Named
// volumes are block-backed ext4 owned by the guest root, so chmod succeeds — a
// prerequisite for k3s writing its datastore. See docs/VERIFICATION.md G3.
func nodeVolumeName(clusterName, nodeName string) string {
	return sanitizeVolumeName(clusterName + "-" + nodeName + "-k3s")
}

// nodeFQDN returns the container name used for --name and as the container ID.
// When domain is non-empty, it appends ".<domain>" to form a host-resolvable FQDN
// that Apple's container DNS forwarding registers. When domain is empty the bare
// nodeName is returned unchanged, preserving IP-only behaviour.
//
// Note: `container run` has no --hostname flag (verified 2026-06-26 against container
// 1.0.0); --name alone drives both the container ID and the DNS A-record. k3s derives
// the Kubernetes node name from the hostname/container name, dropping the domain
// suffix, which yields a clean node name (e.g. "aegis-server-1" from
// "aegis-server-1.aegis").
func nodeFQDN(nodeName, domain string) string {
	if domain == "" {
		return nodeName
	}

	return nodeName + "." + domain
}

// clusterAPIFQDN returns the shared HA API endpoint name <cluster>-api.<domain>. It is added
// to every server's --tls-san in HA mode (see buildRunArgs) so a single kubeconfig endpoint
// and a future API load balancer are cert-valid against any server. It is NOT a container
// name, so container DNS does not auto-register it — wiring it as the live endpoint is the
// load-balancer step deferred in docs/ADR/0002.
func clusterAPIFQDN(clusterName, domain string) string {
	return clusterName + "-api." + domain
}

// datastoreNodeName is the bare name of the managed datastore node: <cluster>-db. The FQDN
// (datastoreNodeName + "." + domain) is both the container --name and the host that the
// servers' --datastore-endpoint points at, so container DNS re-registers it to the new IP on
// a cold restart (the property that makes external-datastore HA survive the DHCP shift, G9).
func datastoreNodeName(clusterName string) string {
	return clusterName + "-db"
}

// datastoreVolumeName is the named volume backing the managed Postgres data dir. It does NOT
// follow the k3s nodeVolumeName scheme (that is <cluster>-<node>-k3s for /var/lib/rancher/k3s);
// destroyRecordedNodes special-cases RoleDatastore to delete this name.
func datastoreVolumeName(clusterName string) string {
	return sanitizeVolumeName(clusterName + "-db-pg")
}

// datastoreEndpointURL builds the k3s --datastore-endpoint for the managed Postgres node.
// sslmode=disable: the datastore is on the private vmnet and v0.2.0 does not provision TLS for
// it (a documented limit in docs/ADR/0002; k3s supports --datastore-cafile/certfile/keyfile for
// hardening later). The password is a generated hex string, URL-safe with no escaping needed.
func datastoreEndpointURL(clusterName, domain, password string) string {
	host := net.JoinHostPort(nodeFQDN(datastoreNodeName(clusterName), domain), strconv.Itoa(datastorePort))

	return fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=disable", datastoreUser, password, host, datastoreDB)
}

// buildDatastoreRunArgs assembles the `container run` vector for the managed Postgres datastore
// micro-VM. Pure function (unit-testable without launching a VM), mirroring buildRunArgs. It is
// deliberately NOT buildRunArgs: a Postgres node shares none of the k3s recipe (no --cap-add ALL,
// no tmpfs, no k3s subcommand, a different image, env, data mount, and the PGDATA-subdir guard).
// It carries the same cluster labels as k3s nodes so the destroy label sweep reclaims it.
func buildDatastoreRunArgs(cfg ClusterConfig, password, dnsDomain string) []string {
	args := []string{
		"run", "--detach",
		"--name", nodeFQDN(datastoreNodeName(cfg.Name), dnsDomain),
		"--memory", fmt.Sprintf("%dMB", defaultDatastoreMemoryBytes/(1024*1024)),
		"--volume", datastoreVolumeName(cfg.Name) + ":" + datastoreDataMount,
		"--env", "POSTGRES_USER=" + datastoreUser,
		"--env", "POSTGRES_PASSWORD=" + password,
		"--env", "POSTGRES_DB=" + datastoreDB,
		// PGDATA subdir: the ext4 named volume ships a lost+found, so initdb refuses the mount
		// root as the data dir (G9). Point it at a subdirectory.
		"--env", "PGDATA=" + datastorePGDataDir,
		"--label", labelOwned + "=true",
		"--label", labelClusterName + "=" + cfg.Name,
		"--label", "k3s.role=" + RoleDatastore.String(),
	}

	if cfg.Network != "" {
		args = append(args, "--network", cfg.Network)
	}

	args = append(args, defaultDatastoreImage)

	return args
}

// nodeTmpfsPaths returns the in-VM paths mounted as writable tmpfs for a k3s node.
//
// This is the central recipe DIVERGENCE from the Talos sibling. The Talos node needed
// /run,/system,/tmp,/var,/system/state plus CNI/k8s overlay dirs as tmpfs. For k3s:
//   - ONLY /run and /tmp. k3s repopulates these at boot and they must be real mount
//     points, same reasoning as Talos.
//   - DO NOT tmpfs /var. k3s's datastore and all server state live under
//     /var/lib/rancher/k3s; that path is a named volume mount (see nodeVolumeName),
//     not tmpfs. tmpfs-ing /var would shadow the named volume and wipe the datastore
//     on every restart — the exact opposite of what gate G3/G5 needs.
//   - ALL Talos-specific paths are dropped: /system, /system/state, /etc/cni,
//     /etc/kubernetes, /usr/libexec/kubernetes.
//   - CARRY OVER the Talos lesson: do NOT tmpfs /opt. tmpfs does not copy-up, so it
//     would shadow any shipped /opt content.
func nodeTmpfsPaths() []string {
	return []string{"/run", "/tmp"}
}

// buildRunArgs assembles the `container run` argument vector for one k3s node. It is a
// pure function so the recipe can be unit-tested (incl. BVA on node fields) without
// launching a VM — same discipline as the Talos sibling's buildRunArgs.
//
// dnsDomain, when non-empty, sets the container --name to an FQDN (<node>.<domain>)
// so Apple's container DNS forwarding resolves the node from the host by name, and
// bakes the FQDN into --tls-san so the API server cert covers it across IP changes.
// Volume names are derived from the bare node.Name regardless of the domain, so Create
// and Destroy always agree on the same volume identifiers.
//
// serverURL is "" for a server node and "https://<server-fqdn>:6443" for an agent.
// The caller (Create) computes the FQDN-based URL after the server boots and passes it
// when launching agents.
// kubeconfigHostDir, when non-empty (server node only), is bind-mounted at kubeconfigMount
// so k3s can write its admin kubeconfig straight to the host. The caller passes the
// absolute cluster state dir; agents pass "".
func buildRunArgs(cfg ClusterConfig, node NodeConfig, serverURL, dnsDomain, kubeconfigHostDir string) []string {
	args := []string{
		"run", "--detach",
		"--name", nodeFQDN(node.Name, dnsDomain),
		// G1 VERIFIED: k3s's embedded containerd needs CAP_SYS_ADMIN (mount/pivot_root/
		// cgroup). apple/container has no --privileged; --cap-add ALL is the equivalent.
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

	// DATASTORE PERSISTENCE: back /var/lib/rancher/k3s with a per-node Apple NAMED
	// VOLUME (block-backed ext4, guest-owned). Named volumes survive `container stop/
	// start` and `container rm`. Host-path bind-mounts are virtio-fs shares the guest
	// cannot chmod — k3s sqlite writes would fail. Volume names use the BARE node name
	// so Create and Destroy derive the same name regardless of the dns-domain setting.
	args = append(args, "--volume", nodeVolumeName(cfg.Name, node.Name)+":"+k3sDatastoreMount)

	// KUBECONFIG DELIVERY (server only): bind-mount the host cluster state dir so k3s writes
	// its admin kubeconfig where the host can read it directly — no container cp. See the
	// kubeconfigMount doc for why cp is avoided. A host bind-mount is a virtio-fs share; that
	// is fine for a plain kubeconfig file (unlike the sqlite datastore, which needs the
	// block-backed named volume above).
	if node.Role == RoleServer && kubeconfigHostDir != "" {
		args = append(args, "--volume", kubeconfigHostDir+":"+kubeconfigMount)
	}

	// Labels: k3s.* replacing the Talos sibling's talos.* scheme. Node IDs are also
	// tracked in state.json so teardown does not depend on label-listing (the CLI does
	// not support native label filters — Talos sibling finding). Labels on containers and
	// named volumes share the same key scheme, so the destroy label sweep covers both.
	args = append(args,
		"--label", labelOwned+"=true",
		"--label", labelClusterName+"="+cfg.Name,
		"--label", "k3s.role="+node.Role.String(),
	)

	// Environment. K3S_TOKEN is the shared cluster secret (server presets it, agents
	// reuse it). The Talos sibling's PLATFORM=container and TALOSSKU are REMOVED — they
	// are Talos-only and meaningless to k3s.
	args = append(args, "--env", "K3S_TOKEN="+cfg.Token)

	// Agents additionally need to know where the server is. The server node gets no
	// K3S_URL (it IS the server). When dns-domain is set, serverURL carries the FQDN
	// endpoint (stable across cold-restart IP changes); without it, the server's current
	// DHCP IP is used.
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
		// kernel dependency (the default flannel VXLAN backend needs br_netfilter).
		args = append(args, "--flannel-backend=host-gw")

		// HA DATASTORE: when an external datastore endpoint is configured, every server runs
		// stateless against it — no embedded etcd, no --cluster-init (etcd's IP-bound peer
		// membership cannot survive the vmnet DHCP IP shift; docs/ADR/0002). Single-server
		// clusters leave this empty and use the embedded sqlite default, byte-identical to
		// v0.1.x. validateClusterConfig guarantees an endpoint is present for >1 server.
		if cfg.DatastoreEndpoint != "" {
			args = append(args, "--datastore-endpoint="+cfg.DatastoreEndpoint)
		}

		// Write the admin kubeconfig to the bind-mounted host dir (mode 0644 so the host
		// user can read it without a chmod round-trip) instead of the default in-node
		// /etc/rancher/k3s/k3s.yaml that only container cp could reach. Create reads it
		// straight off the host. Only emitted when the mount is present.
		if kubeconfigHostDir != "" {
			args = append(args,
				"--write-kubeconfig", kubeconfigMount+"/"+kubeconfigFileName,
				"--write-kubeconfig-mode", "0644",
			)
		}

		// --tls-san: pin the server's FQDN (or a static name as fallback) into the API
		// server cert SANs so the cert stays valid across DHCP IP changes. When a DNS
		// domain is configured, the FQDN IS the stable name and survives cold-restart IP
		// changes on the host side; without a domain, skip the SAN (IP-only mode accepts
		// the IP-change limitation as documented in docs/VERIFICATION.md G5).
		if dnsDomain != "" {
			args = append(args, "--tls-san", nodeFQDN(node.Name, dnsDomain))

			// HA: also cover the shared cluster API name so ONE kubeconfig endpoint (and a
			// future API load balancer) is cert-valid against every server. Only added in HA
			// mode; harmless extra SAN. The name is not auto-registered in container DNS, so
			// until an LB/DNS record exists the kubeconfig still points at the bootstrap
			// server's own FQDN — this SAN is forward-compatible cover for that work.
			if cfg.DatastoreEndpoint != "" {
				args = append(args, "--tls-san", clusterAPIFQDN(cfg.Name, dnsDomain))
			}
		}
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
