// SPDX-License-Identifier: MIT

package apple

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"path/filepath"
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

// k3sManifestsDir is k3s's auto-deploy directory inside a server node. k3s applies every
// manifest that lands here at startup (the "auto-deploying manifests" feature). It sits UNDER
// the datastore mount (/var/lib/rancher/k3s), so v0.4.0's -manifest support bind-mounts each
// host manifest file INDIVIDUALLY at <this>/<basename> rather than mounting the whole directory
// — a directory mount would shadow k3s's own generated manifests (traefik, coredns,
// local-storage, metrics-server) that live in the named volume. See manifestMountArgs.
const k3sManifestsDir = k3sDatastoreMount + "/server/manifests"

// Managed datastore (external etcd cluster) recipe constants. v0.3.0 supersedes ADR-0002's
// single-Postgres datastore with an auto-provisioned N-node etcd cluster as the default HA
// datastore (docs/ADR-0003, hardware-verified). Image and memory are the DEFAULTS; v0.5.0 exposes
// them as -datastore-image / -datastore-memory (ClusterConfig.DatastoreImage / DatastoreMemoryBytes).
const (
	// defaultEtcdImage is the etcd node image. ADR-0003 VERIFIED: quay.io/coreos/etcd:v3.5.16
	// forms a 3-member FQDN-addressed cluster under Apple container and survives the cold-restart
	// DHCP IP shift. Use this exact tag.
	defaultEtcdImage = "quay.io/coreos/etcd:v3.5.16"
	// etcdBinary is the in-image etcd executable. The image sets Cmd=["/usr/local/bin/etcd"] but
	// NO ENTRYPOINT, and Apple `container` execs the first post-image arg as the target binary, so
	// we must name it explicitly — otherwise a bare flag like --name is taken as the executable and
	// the member dies with `failed to find target executable --name` (caught in v0.3.0 hardware bring-up).
	etcdBinary     = "/usr/local/bin/etcd"
	etcdClientPort = 2379 // client/API port; k3s --datastore-endpoint targets this
	etcdPeerPort   = 2380 // peer port for member-to-member quorum traffic
	// etcdDataMount is the in-node mount point of the member's named volume. etcdDataDir is a
	// SUBDIR of it: an ext4 named volume ships a lost+found at the mount root, which etcd refuses
	// as a data dir (ADR-0003 gotcha, mirrors the Postgres PGDATA-subdir guard in ADR-0002).
	etcdDataMount = "/data"
	etcdDataDir   = "/data/etcd"
	// defaultEtcdMembers is the default etcd cluster size. MUST be odd for quorum (3 or 5);
	// validateEtcdMemberCount enforces that. 3 tolerates losing 1 member.
	defaultEtcdMembers     = 3
	minEtcdMembers         = 3
	defaultEtcdMemoryBytes = 512 * 1024 * 1024 // 512 MiB — etcd is light vs a full Postgres
)

// API load balancer (RoleLB) recipe constants. The LB is one stateless micro-VM running
// haproxy in TCP (L4) passthrough mode in front of the HA servers' apiserver port (6443).
//
// WHY haproxy mode tcp (not nginx stream, not an L7 proxy): the apiserver speaks mutual TLS
// and the kubeconfig must verify the apiserver cert END TO END, so the LB must NOT terminate
// TLS — it forwards raw TCP. Both haproxy `mode tcp` and nginx `stream {}` do L4 passthrough,
// but this substrate's defining constraint is the vmnet DHCP IP shift on cold restart, so the
// LB must RE-RESOLVE its backend FQDNs at runtime (not just once at startup). Open-source
// haproxy has mature runtime DNS re-resolution — a `resolvers` section with `parse-resolv-conf`
// (reuse the same container DNS that re-registers <node>.<domain> A-records) plus per-server
// `resolvers`/`resolve-prefer ipv4` — so a backend that moves to a new DHCP IP is picked up
// within the `hold` TTL. Open-source nginx re-resolves stream upstreams only via the commercial
// `resolve` parameter (or a single-backend `proxy_pass $var` hack that loses multi-backend
// balancing), so haproxy is the better fit for surviving the IP shift here.
const (
	defaultAPILBImage       = "haproxy:3.0-alpine"     // 3.0 is an LTS; >=2.1 needed for `parse-resolv-conf`
	apiLBConfigMount        = "/usr/local/etc/haproxy" // haproxy image's default config dir (default CMD reads haproxy.cfg here)
	apiLBConfigFileName     = "haproxy.cfg"            // the file the default CMD loads
	apiLBConfigSubdir       = "lb"                     // subdir under the cluster state dir holding the generated haproxy.cfg
	defaultAPILBMemoryBytes = 256 * 1024 * 1024        // 256 MiB — a TCP passthrough proxy is light
	apiLBDNSHoldSeconds     = 10                       // backend-FQDN resolution TTL; bounds how fast a shifted DHCP IP is picked up
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
// and the API load balancer are cert-valid against any server. As of v0.3.0 it IS a live
// container name: the RoleLB micro-VM is launched with --name == this FQDN, so container DNS
// auto-registers it to the LB's IP (and re-registers on the cold-restart shift, like any node).
// It is identical to nodeFQDN(apiLBNodeName(clusterName), domain) — the single source of truth
// is kept here because the --tls-san wiring predates the LB node.
func clusterAPIFQDN(clusterName, domain string) string {
	return clusterName + "-api." + domain
}

// apiLBNodeName is the bare name of the API load-balancer node: <cluster>-api. Its FQDN
// (apiLBNodeName + "." + domain) equals clusterAPIFQDN and is both the container --name and the
// host that the kubeconfig + agents target, so container DNS re-registers it to the new IP on a
// cold restart — the same FQDN mechanism that stabilizes every other endpoint (ADR-0001).
func apiLBNodeName(clusterName string) string {
	return clusterName + "-api"
}

// validateEtcdMemberCount enforces the etcd quorum invariant: an odd member count of at least
// minEtcdMembers (3). Even counts gain no fault tolerance over the next-lower odd count and risk
// a split vote; fewer than 3 cannot tolerate a single loss (the whole point of HA). Pure so the
// boundary is unit-testable (BVA, CLAUDE.md k): 1=reject, 2=reject, 3=accept, 4=reject, 5=accept.
func validateEtcdMemberCount(n int) error {
	if n < minEtcdMembers {
		return fmt.Errorf("etcd cluster needs at least %d members for quorum, got %d", minEtcdMembers, n)
	}

	if n%2 == 0 {
		return fmt.Errorf("etcd member count must be odd (3 or 5) for a stable quorum, got %d", n)
	}

	return nil
}

// etcdNodeName is the bare name of etcd member i (1-based): <cluster>-etcd-<i>. Cluster-scoped
// (like every other node: <cluster>-server-N, <cluster>-agent-N) so two HA clusters sharing one
// DNS domain never collide on the same etcd FQDN. It is both the etcd member name (--name after
// the image) and the basis of the container --name FQDN (nodeFQDN(etcdNodeName, domain)) that
// container DNS registers; the two must agree for --initial-cluster to resolve every peer.
func etcdNodeName(clusterName string, i int) string {
	return fmt.Sprintf("%s-etcd-%d", clusterName, i)
}

// etcdVolumeName is the named volume backing one etcd member's data dir. It is keyed on the bare
// member NAME (not a single shared name like the old Postgres datastore) so each of the N members
// gets its own volume; destroyRecordedNodes derives the same name per RoleDatastore node. It does
// NOT follow the k3s nodeVolumeName scheme (<cluster>-<node>-k3s for /var/lib/rancher/k3s).
func etcdVolumeName(nodeName string) string {
	return sanitizeVolumeName(nodeName + "-data")
}

// etcdMembers builds the NodeConfig list for an N-member etcd cluster. Each carries RoleDatastore
// (a label-only role — etcd members never reach the k3s subcommand path) and memBytes of memory
// (the resolved -datastore-memory, v0.5.0; callers pass defaultEtcdMemoryBytes for the default).
// The members are NOT part of ClusterConfig.Nodes; Create provisions them separately.
func etcdMembers(clusterName string, count int, memBytes int64) []NodeConfig {
	members := make([]NodeConfig, count)
	for i := range members {
		members[i] = NodeConfig{
			Name:   etcdNodeName(clusterName, i+1),
			Role:   RoleDatastore,
			Memory: memBytes,
		}
	}

	return members
}

// etcdInitialCluster renders etcd's --initial-cluster value: a comma-separated
// <member-name>=<peer-url> list covering every member, all FQDN-addressed so peer membership is
// name-bound (not IP-bound) and therefore survives the vmnet DHCP cold-restart shift — the exact
// failure that ruled out embedded etcd in ADR-0002 but does NOT apply here, because these peers
// reach each other by name (ADR-0003). The member name matches each member's etcd --name. Peer URLs
// are https:// (v0.5.0): the managed quorum now runs over mutual TLS (etcd_tls.go), and the member
// server cert's FQDN SAN keeps verification valid across the IP shift.
func etcdInitialCluster(clusterName, domain string, count int) string {
	parts := make([]string, count)
	for i := range parts {
		name := etcdNodeName(clusterName, i+1)
		peer := "https://" + net.JoinHostPort(nodeFQDN(name, domain), strconv.Itoa(etcdPeerPort))
		parts[i] = name + "=" + peer
	}

	return strings.Join(parts, ",")
}

// etcdDatastoreEndpoint builds the k3s --datastore-endpoint for the managed etcd cluster: a
// comma-separated list of every member's FQDN client URL. k3s tries them in order and fails over,
// so naming all members (not just one) keeps the control plane reachable when any single member is
// down. https:// (v0.5.0): the managed quorum runs over mutual TLS; the k3s servers present a
// client cert and verify each member's FQDN-SAN server cert (the FQDN SAN survives the IP shift).
// The servers are wired with --datastore-cafile/certfile/keyfile in buildRunArgs.
func etcdDatastoreEndpoint(clusterName, domain string, count int) string {
	parts := make([]string, count)
	for i := range parts {
		host := nodeFQDN(etcdNodeName(clusterName, i+1), domain)
		parts[i] = "https://" + net.JoinHostPort(host, strconv.Itoa(etcdClientPort))
	}

	return strings.Join(parts, ",")
}

// buildEtcdRunArgs assembles the `container run` vector for one etcd member micro-VM. Pure
// (unit-testable without launching a VM), mirroring buildRunArgs and buildAPILBRunArgs.
// It is deliberately NOT buildRunArgs: an etcd node shares none of the k3s recipe (no --cap-add ALL,
// no tmpfs, no k3s subcommand, a different image, data mount, and the lost+found-subdir guard). It
// carries the same cluster labels as k3s nodes so the destroy label sweep reclaims it.
//
// Two --name flags appear and that is intentional: the CONTAINER --name (before the image) is the
// FQDN that container DNS registers (MANDATORY — a bare name leaves every peer at NXDOMAIN and
// quorum never forms, ADR-0003); the etcd --name (after the image) is the bare member name that
// --initial-cluster keys on. All peer/client/advertise URLs are FQDNs so membership is name-bound.
// initialCluster is the shared etcdInitialCluster value (identical for every member).
//
// v0.5.0: the quorum runs over mutual TLS. tlsHostDir is the absolute host dir holding this member's
// bundle (ca.crt/server.crt/server.key, written by writeEtcdTLS); it is bind-mounted read-only at
// etcdTLSMount and wired into the peer + client TLS flags (etcdTLSFlags). All URLs are https://. The
// member server cert's FQDN SAN is what keeps TLS valid across the DHCP IP shift.
func buildEtcdRunArgs(cfg ClusterConfig, node NodeConfig, dnsDomain, initialCluster, tlsHostDir string) []string {
	fqdn := nodeFQDN(node.Name, dnsDomain)

	image := cfg.DatastoreImage
	if image == "" {
		image = defaultEtcdImage
	}

	args := []string{
		"run", "--detach",
		"--name", fqdn, // CONTAINER name == FQDN so container DNS registers the peer (MANDATORY)
		"--memory", fmt.Sprintf("%dMB", node.Memory/(1024*1024)),
		"--volume", etcdVolumeName(node.Name) + ":" + etcdDataMount,
		// etcd TLS bundle (CA + this member's server cert/key) bind-mounted read-only (v0.5.0). A
		// virtio-fs share is fine for plain cert files (same reasoning as the kubeconfig/haproxy
		// mounts); only the block-backed etcd data dir needs a named volume.
		"--volume", tlsHostDir + ":" + etcdTLSMount + ":ro",
		"--label", labelOwned + "=true",
		"--label", labelClusterName + "=" + cfg.Name,
		"--label", "k3s.role=" + RoleDatastore.String(),
	}

	if cfg.Network != "" {
		args = append(args, "--network", cfg.Network)
	}

	// Image, the etcd binary, then the etcd flags. Apple `container` execs the first post-image arg
	// as the target executable; the etcd image sets Cmd=["/usr/local/bin/etcd"] but NO ENTRYPOINT,
	// so the binary must be named explicitly (a bare --name would otherwise be taken as the
	// executable). The trailing args are then etcd flags — same shape as a k3s node's
	// `server`/`agent` subcommand after the image.
	args = append(args, image, etcdBinary,
		"--name", node.Name, // etcd MEMBER name (bare); must match this member's key in --initial-cluster
		"--data-dir", etcdDataDir, // subdir of the mount: the ext4 lost+found blocks etcd at the root
		"--listen-peer-urls", fmt.Sprintf("https://0.0.0.0:%d", etcdPeerPort),
		"--listen-client-urls", fmt.Sprintf("https://0.0.0.0:%d", etcdClientPort),
		"--initial-advertise-peer-urls", "https://"+net.JoinHostPort(fqdn, strconv.Itoa(etcdPeerPort)),
		"--advertise-client-urls", "https://"+net.JoinHostPort(fqdn, strconv.Itoa(etcdClientPort)),
		"--initial-cluster", initialCluster,
		"--initial-cluster-state", "new",
		"--initial-cluster-token", cfg.Name+"-etcd", // isolates this cluster's quorum from any other on the network
	)

	// Mutual-TLS flags for the peer and client ports (v0.5.0). Kept in a helper so this recipe stays
	// readable and the flag set is asserted in one place.
	args = append(args, etcdTLSFlags()...)

	return args
}

// etcdTLSFlags returns etcd's peer + client mutual-TLS flags (v0.5.0). Both the peer port (member
// quorum traffic) and the client port (the k3s servers) serve TLS from this member's server cert and
// trust the shared CA; the *-client-cert-auth flags force the other side to present a CA-signed cert
// too (mutual TLS), so an unauthenticated peer or client is rejected. The files resolve under the
// bind-mounted etcdTLSMount (buildEtcdRunArgs / writeEtcdTLS).
func etcdTLSFlags() []string {
	return []string{
		"--peer-cert-file", etcdTLSMount + "/" + etcdServerCertFile,
		"--peer-key-file", etcdTLSMount + "/" + etcdServerKeyFile,
		"--peer-trusted-ca-file", etcdTLSMount + "/" + etcdCACertFile,
		"--peer-client-cert-auth=true",
		"--cert-file", etcdTLSMount + "/" + etcdServerCertFile,
		"--key-file", etcdTLSMount + "/" + etcdServerKeyFile,
		"--trusted-ca-file", etcdTLSMount + "/" + etcdCACertFile,
		"--client-cert-auth=true",
	}
}

// clusterAPIEndpoint returns the stable https URL the kubeconfig and agents target.
//
// The selection is the BVA boundary (CLAUDE.md k) for the LB feature:
//   - LB enabled (more than one server + a DNS domain): the shared LB FQDN
//     https://<cluster>-api.<domain>:6443 — ONE endpoint that fans out across every server,
//     cert-valid because that name is in every server's --tls-san (buildRunArgs, HA mode).
//   - otherwise + a DNS domain: the bootstrap server's own FQDN endpoint (v0.2.0 behaviour).
//   - otherwise (IP-only): the bootstrap server's current DHCP IP (v0.1.x behaviour).
//
// Pure so the endpoint selection is unit-testable without launching anything. cfg.ProvisionAPILB
// is the same predicate setupAPILB uses, so the endpoint can never point at an LB that was not
// provisioned (and vice-versa).
func clusterAPIEndpoint(cfg ClusterConfig, serverName string, serverIP netip.Addr, dnsDomain string) string {
	switch {
	case cfg.ProvisionAPILB && dnsDomain != "":
		return "https://" + net.JoinHostPort(clusterAPIFQDN(cfg.Name, dnsDomain), strconv.Itoa(k3sAPIPort))
	case dnsDomain != "":
		return "https://" + net.JoinHostPort(nodeFQDN(serverName, dnsDomain), strconv.Itoa(k3sAPIPort))
	default:
		return "https://" + net.JoinHostPort(serverIP.String(), strconv.Itoa(k3sAPIPort))
	}
}

// buildAPILBConfig renders the haproxy.cfg for the API load balancer. Pure (config text in
// strings out) so the recipe is unit-testable without launching a VM — same discipline as
// buildRunArgs / buildEtcdRunArgs.
//
// The config is L4 (mode tcp) passthrough: haproxy never decrypts, so the client verifies the
// apiserver cert end to end. Backends are each server addressed by FQDN (nodeFQDN), never IP, so
// they survive the vmnet DHCP shift. Runtime DNS re-resolution makes that survival real:
//   - `resolvers containerdns parse-resolv-conf` reuses the container's own DNS (the Apple
//     container resolver that re-registers <node>.<domain> A-records to the new IP on restart);
//   - each `server` line carries `resolvers containerdns resolve-prefer ipv4` so haproxy re-resolves
//     the backend FQDN on the `hold valid` TTL (apiLBDNSHoldSeconds) and follows the moved IP;
//   - `init-addr none` lets haproxy START even if a backend FQDN does not resolve yet at boot
//     (servers may still be coming up), instead of a fatal startup error.
//
// servers must be the server NodeConfigs (RoleServer); callers pass the same slice splitRoles
// produced. dnsDomain MUST be non-empty (the LB is FQDN-only) — setupAPILB guarantees this.
func buildAPILBConfig(clusterName string, servers []NodeConfig, dnsDomain string) string {
	var b strings.Builder

	fmt.Fprintf(&b, "# Generated by k3ac for cluster %q — API load balancer (L4/TCP passthrough).\n", clusterName)
	b.WriteString("# DO NOT terminate TLS here: the apiserver cert is verified end to end by the client.\n\n")

	b.WriteString("global\n")
	b.WriteString("    log stdout format raw local0\n")
	b.WriteString("    maxconn 4096\n\n")

	b.WriteString("defaults\n")
	b.WriteString("    log     global\n")
	b.WriteString("    mode    tcp\n")
	b.WriteString("    option  tcplog\n")
	b.WriteString("    timeout connect 5s\n")
	b.WriteString("    timeout client  30s\n")
	b.WriteString("    timeout server  30s\n\n")

	// parse-resolv-conf: pick up the container's DNS server (Apple container DNS), the same
	// resolver that re-registers <node>.<domain> after the cold-restart IP shift.
	b.WriteString("resolvers containerdns\n")
	b.WriteString("    parse-resolv-conf\n")
	fmt.Fprintf(&b, "    hold valid %ds\n", apiLBDNSHoldSeconds)
	fmt.Fprintf(&b, "    hold nx    %ds\n\n", apiLBDNSHoldSeconds)

	fmt.Fprintf(&b, "frontend k3s-api\n    bind *:%d\n    default_backend k3s-servers\n\n", k3sAPIPort)

	b.WriteString("backend k3s-servers\n")
	b.WriteString("    balance roundrobin\n")
	b.WriteString("    option tcp-check\n")

	for i, s := range servers {
		fqdn := nodeFQDN(s.Name, dnsDomain)
		// resolvers + resolve-prefer ipv4 + init-addr none: re-resolve the backend FQDN at
		// runtime and tolerate it being unresolvable at boot. check: passive TCP health check.
		fmt.Fprintf(&b, "    server srv%d %s:%d check resolvers containerdns resolve-prefer ipv4 init-addr none\n",
			i+1, fqdn, k3sAPIPort)
	}

	return b.String()
}

// buildAPILBRunArgs assembles the `container run` vector for the API load-balancer micro-VM.
// Pure (unit-testable without launching a VM), mirroring buildEtcdRunArgs. It is NOT
// buildRunArgs: the LB shares none of the k3s recipe (no --cap-add, no tmpfs, no named volume,
// no k3s subcommand, a different image). It is STATELESS — its only state is the bind-mounted
// haproxy.cfg, so there is no named volume to create or reclaim. It carries the cluster labels
// so the destroy label sweep reclaims it even when state.json is absent.
//
// configHostDir is the absolute host dir holding the generated haproxy.cfg; it is bind-mounted
// (read-only) at the haproxy image's default config dir so the image's default CMD loads it. A
// bind-mount is a virtio-fs share — fine for a plain config file (the same reasoning that makes
// the kubeconfig bind-mount safe; only the block-backed datastore needs a named volume). dnsDomain
// MUST be non-empty (the --name is the FQDN container DNS registers); setupAPILB guarantees this.
func buildAPILBRunArgs(cfg ClusterConfig, dnsDomain, configHostDir string) []string {
	args := []string{
		"run", "--detach",
		"--name", nodeFQDN(apiLBNodeName(cfg.Name), dnsDomain),
		"--memory", fmt.Sprintf("%dMB", defaultAPILBMemoryBytes/(1024*1024)),
		"--volume", configHostDir + ":" + apiLBConfigMount + ":ro",
		"--label", labelOwned + "=true",
		"--label", labelClusterName + "=" + cfg.Name,
		"--label", "k3s.role=" + RoleLB.String(),
	}

	if cfg.Network != "" {
		args = append(args, "--network", cfg.Network)
	}

	// Image is the final arg: the haproxy image's default ENTRYPOINT/CMD runs
	// `haproxy -f <apiLBConfigMount>/haproxy.cfg`, so no trailing subcommand (same shape as
	// the Postgres datastore, which also relies on its image's default entrypoint).
	args = append(args, defaultAPILBImage)

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

		// AUTO-DEPLOY MANIFESTS (v0.4.0): bind-mount each host manifest file into the
		// bootstrap server's k3s auto-deploy dir so k3s applies it at startup. Same
		// virtio-fs bind-mount mechanism the kubeconfig uses (ADR-0001) — a plain file
		// share, no guest agent. Only the bootstrap server (kubeconfigHostDir != "")
		// mounts them: auto-deploy AddOn state is recorded in the shared datastore, so
		// applying once is enough even in HA. cfg.Manifests is absolute by here (Create
		// resolved + collision-checked it). agents and HA join servers get nothing.
		args = append(args, manifestMountArgs(cfg.Manifests)...)
	}

	// DATASTORE TLS (v0.5.0): EVERY server (bootstrap AND HA join, unlike the bootstrap-only
	// kubeconfig mount) bind-mounts the client TLS bundle (ca.crt/client.crt/client.key) read-only
	// so it reaches the managed etcd quorum over mutual TLS. Set only when Create generated a
	// managed-etcd bundle (cfg.DatastoreTLSDir); a bring-your-own datastore endpoint leaves it empty
	// (the operator owns that connection's TLS). The matching --datastore-*file flags are added in
	// the server-flags section below.
	if node.Role == RoleServer && cfg.DatastoreTLSDir != "" {
		args = append(args, "--volume", cfg.DatastoreTLSDir+":"+etcdTLSMount+":ro")
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

	// ENV INJECTION (v0.5.0): operator-supplied KEY=VALUE env vars (-env, repeatable) added as
	// --env flags on every k3s node, alongside the built-in K3S_TOKEN/K3S_URL. Empty = none.
	for _, env := range cfg.EnvVars {
		args = append(args, "--env", env)
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

		// k3s datastore client TLS (v0.5.0): point k3s at the bind-mounted client bundle so it
		// presents a client cert and verifies each etcd member's FQDN-SAN server cert. Paired with the
		// https:// --datastore-endpoint (etcdDatastoreEndpoint). Managed-etcd path only — empty for a
		// bring-your-own endpoint, which carries its own TLS params in the endpoint string.
		if cfg.DatastoreTLSDir != "" {
			args = append(args,
				"--datastore-cafile="+etcdTLSMount+"/"+etcdCACertFile,
				"--datastore-certfile="+etcdTLSMount+"/"+etcdClientCertFile,
				"--datastore-keyfile="+etcdTLSMount+"/"+etcdClientKeyFile,
			)
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

	// NODE LABELS (v0.4.0): both server and agent accept --node-label KEY=VALUE. Emitted
	// after the role-specific built-in flags and before the verbatim passthrough so an
	// operator-supplied --k3s-*-arg can still override anything here (k3s is last-one-wins).
	for _, label := range node.Labels {
		args = append(args, "--node-label", label)
	}

	// K3S-ARG PASSTHROUGH (v0.4.0): operator-supplied k3s flags appended VERBATIM, LAST, so
	// they sit after every built-in flag and win on conflicts (k3s last-one-wins). This is the
	// "open the closed box" escape hatch: --disable=traefik, --flannel-backend=vxlan,
	// --cluster-cidr=..., enable ServiceLB, etc. The caller resolves the per-role list (server
	// args onto servers, agent args onto agents); a nil ExtraArgs appends nothing.
	args = append(args, node.ExtraArgs...)

	return args
}

// manifestMountArgs returns the `--volume <hostpath>:<target>` pairs that bind-mount each
// host manifest file into the bootstrap server's k3s auto-deploy dir (k3sManifestsDir). Pure
// so the mount-path construction is unit-testable (BVA: zero / one / many manifests) without
// launching a VM. Each manifest mounts INDIVIDUALLY at k3sManifestsDir/<basename>, leaving
// k3s's own generated manifests in the named volume intact. hostPaths are expected absolute
// (Create resolves + collision-checks them via resolveManifests); a nil/empty slice yields nil.
func manifestMountArgs(hostPaths []string) []string {
	if len(hostPaths) == 0 {
		return nil
	}

	args := make([]string, 0, len(hostPaths)*2)

	for _, hostPath := range hostPaths {
		target := k3sManifestsDir + "/" + filepath.Base(hostPath)
		args = append(args, "--volume", hostPath+":"+target)
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
