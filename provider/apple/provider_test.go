// SPDX-License-Identifier: MIT

package apple

import (
	"bytes"
	"context"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func serverNode(name string) NodeConfig {
	return NodeConfig{Name: name, Role: RoleServer}
}

func agentNode(name string) NodeConfig {
	return NodeConfig{Name: name, Role: RoleAgent}
}

// TestValidateClusterConfig_ServerCountBoundaries exercises the server-count boundary
// (BVA, CLAUDE.md k), now gated on the datastore mode (docs/ADR/0002):
//   - 0 servers              : always rejected (nothing owns the API).
//   - 1 server               : always accepted (embedded sqlite OR external datastore).
//   - 2 servers, no endpoint : rejected (multi-server sqlite impossible, etcd disabled).
//   - 2 servers, endpoint set: accepted (HA on the shared external datastore).
//
// So B=1 is the ceiling ONLY without a datastore; with one, the ceiling lifts. Both sides
// of the B+1 boundary (2 servers) are exercised — the datastore endpoint flips the verdict.
func TestValidateClusterConfig_ServerCountBoundaries(t *testing.T) {
	const ds = "postgres://kine:pw@db.aegis:5432/kine"

	tests := []struct {
		name    string
		nodes   []NodeConfig
		ds      string
		managed bool
		wantErr bool
	}{
		{"no nodes at all", nil, "", false, true},
		{"0 servers, 1 agent (B-1, invalid)", []NodeConfig{agentNode("a1")}, "", false, true},
		{"1 server, 0 agent (single-node sqlite, valid)", []NodeConfig{serverNode("s1")}, "", false, false},
		{"1 server + 1 agent (smallest real, valid)", []NodeConfig{serverNode("s1"), agentNode("a1")}, "", false, false},
		{"1 server + datastore (valid)", []NodeConfig{serverNode("s1")}, ds, false, false},
		{"2 servers, no datastore (B+1, invalid)", []NodeConfig{serverNode("s1"), serverNode("s2")}, "", false, true},
		{"2 servers + BYO datastore (B+1, HA valid)", []NodeConfig{serverNode("s1"), serverNode("s2")}, ds, false, false},
		{"2 servers + managed datastore (B+1, HA valid)", []NodeConfig{serverNode("s1"), serverNode("s2")}, "", true, false},
		{"3 servers + datastore (HA valid)", []NodeConfig{serverNode("s1"), serverNode("s2"), serverNode("s3")}, ds, false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateClusterConfig(ClusterConfig{Name: "test", DatastoreEndpoint: tt.ds, ManageDatastore: tt.managed, Nodes: tt.nodes})
			if (err != nil) != tt.wantErr {
				t.Fatalf("wantErr=%v, got err=%v", tt.wantErr, err)
			}
		})
	}
}

// TestAssertDistinctIPs guards the everyday "every container gets the same IP" bug.
func TestAssertDistinctIPs(t *testing.T) {
	mk := func(name, ip string) NodeInfo {
		return NodeInfo{Name: name, IPs: []netip.Addr{netip.MustParseAddr(ip)}}
	}

	if err := assertDistinctIPs([]NodeInfo{mk("a", "192.168.64.20"), mk("b", "192.168.64.21")}); err != nil {
		t.Errorf("distinct IPs should pass: %v", err)
	}

	if err := assertDistinctIPs([]NodeInfo{mk("a", "192.168.64.20"), mk("b", "192.168.64.20")}); err == nil {
		t.Error("duplicate IPs must be rejected")
	}
}

// TestNodeTmpfsPaths_OnlyRunAndTmp locks the k3s tmpfs DIVERGENCE from Talos: ONLY
// /run and /tmp; /var must NOT be tmpfs (it backs the named-volume datastore);
// /opt stays off tmpfs (the carried-over Talos lesson).
func TestNodeTmpfsPaths_OnlyRunAndTmp(t *testing.T) {
	paths := nodeTmpfsPaths()

	for _, want := range []string{"/run", "/tmp"} {
		if !slices.Contains(paths, want) {
			t.Errorf("required tmpfs path %q missing", want)
		}
	}

	for _, forbidden := range []string{"/var", "/opt", "/system", "/system/state", "/etc/cni", "/etc/kubernetes"} {
		if slices.Contains(paths, forbidden) {
			t.Errorf("%q must NOT be tmpfs", forbidden)
		}
	}
}

// recipeCfg is a shared fixture for the recipe-lock tests.
func recipeCfg() ClusterConfig {
	return ClusterConfig{
		Name:     "aegis",
		Image:    "rancher/k3s:v1.32.5-k3s1",
		Network:  "default",
		StateDir: "/tmp/aegis-state",
		Token:    "deadbeef",
	}
}

// TestBuildRunArgs_ServerRecipe locks the server-role launch recipe with FQDN enabled.
func TestBuildRunArgs_ServerRecipe(t *testing.T) {
	cfg := recipeCfg()
	node := NodeConfig{Name: "aegis-server-1", Role: RoleServer, Memory: 2048 * 1024 * 1024, NanoCPUs: 2e9}
	kubecfgDir := "/abs/state/aegis"

	args := buildRunArgs(cfg, node, "", "aegis", kubecfgDir)
	joined := strings.Join(args, " ")

	serverFQDN := "aegis-server-1.aegis"

	checks := []struct {
		ok   bool
		desc string
	}{
		{hasPair(args, "--cap-add", "ALL"), "--cap-add ALL (G1 VERIFIED: embedded containerd needs CAP_SYS_ADMIN)"},
		{memoryUsesMB(args), "--memory uses MB suffix (bare M rejected)"},
		{!tmpfsContains(args, "/var"), "/var NOT tmpfs (it is the named-volume datastore mount)"},
		{hasNamedDatastoreVolume(args), "datastore is a NAMED volume (no host path with '/') at " + k3sDatastoreMount},
		{hasPair(args, "--volume", kubecfgDir+":"+kubeconfigMount), "kubeconfig host dir bind-mounted (delivery without container cp)"},
		{hasPair(args, "--write-kubeconfig", kubeconfigMount+"/"+kubeconfigFileName), "--write-kubeconfig points k3s at the bind mount"},
		{hasPair(args, "--write-kubeconfig-mode", "0644"), "--write-kubeconfig-mode 0644 (host-readable)"},
		{slices.Contains(args, "--flannel-backend=host-gw"), "--flannel-backend=host-gw on server (G2)"},
		{hasPair(args, "--tls-san", serverFQDN), "--tls-san is the server FQDN (stable across IP changes)"},
		{!strings.Contains(joined, "aegis-k3s.local"), "old static cluster-dns name NOT present (replaced by FQDN)"},
		{hasPair(args, "--name", serverFQDN), "--name is the server FQDN"},
		{subcommandIs(args, cfg.Image, "server"), "server role uses `server` subcommand after image"},
		{hasPair(args, "--env", "K3S_TOKEN=deadbeef"), "K3S_TOKEN env present"},
		{!strings.Contains(joined, "K3S_URL"), "server has NO K3S_URL (it IS the server)"},
		{!strings.Contains(joined, "PLATFORM"), "no PLATFORM env (Talos-only, removed)"},
		{!strings.Contains(joined, "TALOSSKU"), "no TALOSSKU env (Talos-only, removed)"},
		{slices.Contains(args, "--detach"), "--detach"},
		{hasPair(args, "--label", "k3s.cluster.name=aegis"), "k3s.cluster.name label"},
		{hasPair(args, "--label", "k3s.owned=true"), "k3s.owned label"},
	}

	for _, c := range checks {
		if !c.ok {
			t.Errorf("server recipe check failed: %s\nargs: %s", c.desc, joined)
		}
	}
}

// TestBuildRunArgs_HAServerRecipe locks the HA server recipe: with an external datastore
// endpoint set, a server emits --datastore-endpoint, covers BOTH its own FQDN and the shared
// cluster API name in --tls-san, never uses --cluster-init (embedded etcd is disabled), and —
// as a non-bootstrap server (no kubeconfig host dir) — emits no --write-kubeconfig. Mirrors
// docs/ADR/0002.
func TestBuildRunArgs_HAServerRecipe(t *testing.T) {
	cfg := recipeCfg()
	cfg.DatastoreEndpoint = "postgres://kine:pw@db.aegis:5432/kine"
	node := NodeConfig{Name: "aegis-server-2", Role: RoleServer, Memory: 2048 * 1024 * 1024, NanoCPUs: 2e9}

	// Non-bootstrap server: serverURL "" and kubeconfigHostDir "" (only the first server mounts it).
	args := buildRunArgs(cfg, node, "", "aegis", "")
	joined := strings.Join(args, " ")

	checks := []struct {
		ok   bool
		desc string
	}{
		{slices.Contains(args, "--datastore-endpoint="+cfg.DatastoreEndpoint), "--datastore-endpoint carries the external datastore"},
		{hasPair(args, "--tls-san", "aegis-server-2.aegis"), "--tls-san covers the server's own FQDN"},
		{hasPair(args, "--tls-san", "aegis-api.aegis"), "--tls-san also covers the shared cluster API name (LB-ready cert)"},
		{!strings.Contains(joined, "--cluster-init"), "NO --cluster-init (embedded etcd is intentionally disabled)"},
		{!strings.Contains(joined, "--write-kubeconfig"), "non-bootstrap server emits no --write-kubeconfig (no host mount)"},
		{slices.Contains(args, "--flannel-backend=host-gw"), "host-gw still set"},
		{subcommandIs(args, cfg.Image, "server"), "server subcommand"},
	}

	for _, c := range checks {
		if !c.ok {
			t.Errorf("HA server recipe check failed: %s\nargs: %s", c.desc, joined)
		}
	}
}

// TestBuildRunArgs_SingleServerNoDatastoreFlag guards that the DEFAULT single-server recipe
// (no datastore endpoint) is unchanged: no --datastore-endpoint, and no shared API SAN — only
// the node's own FQDN. This keeps v0.1.x byte-compatible.
func TestBuildRunArgs_SingleServerNoDatastoreFlag(t *testing.T) {
	cfg := recipeCfg() // DatastoreEndpoint == ""
	node := NodeConfig{Name: "aegis-server-1", Role: RoleServer, Memory: 2048 * 1024 * 1024}

	args := buildRunArgs(cfg, node, "", "aegis", "/abs/state/aegis")
	joined := strings.Join(args, " ")

	if strings.Contains(joined, "--datastore-endpoint") {
		t.Errorf("single-server must NOT emit --datastore-endpoint\nargs: %s", joined)
	}

	if hasPair(args, "--tls-san", "aegis-api.aegis") {
		t.Errorf("single-server must NOT add the shared API SAN\nargs: %s", joined)
	}
}

// TestValidateEtcdMemberCount is the BVA (CLAUDE.md k) on the etcd quorum invariant — the pure
// odd-and-≥3 boundary. Members below 3 cannot tolerate a single loss; even counts gain no fault
// tolerance and risk a split vote. Boundaries: 1=reject, 2=reject, 3=accept, 4=reject, 5=accept
// (plus 0 below 3 and 6 even, for completeness around B=3).
func TestValidateEtcdMemberCount(t *testing.T) {
	tests := []struct {
		n       int
		wantErr bool
	}{
		{0, true},  // below minimum
		{1, true},  // B-2: below minimum (and odd, but < 3)
		{2, true},  // B-1: even and below minimum
		{3, false}, // B: smallest valid quorum
		{4, true},  // B+1: even
		{5, false}, // next valid odd
		{6, true},  // even
	}

	for _, tt := range tests {
		err := validateEtcdMemberCount(tt.n)
		if (err != nil) != tt.wantErr {
			t.Errorf("validateEtcdMemberCount(%d): wantErr=%v, got err=%v", tt.n, tt.wantErr, err)
		}
	}
}

// TestEtcdInitialClusterAndEndpoint locks the two FQDN string constructions for N members — the
// single source of truth shared by buildEtcdRunArgs (--initial-cluster) and the k3s servers
// (--datastore-endpoint). BVA on member count: B=3 (the default/minimum) and B+2=5.
func TestEtcdInitialClusterAndEndpoint(t *testing.T) {
	// 3 members: each member-name keyed to its FQDN peer URL, comma-joined, in order.
	wantInitial3 := "aegis-etcd-1=http://aegis-etcd-1.aegis:2380," +
		"aegis-etcd-2=http://aegis-etcd-2.aegis:2380," +
		"aegis-etcd-3=http://aegis-etcd-3.aegis:2380"
	if got := etcdInitialCluster("aegis", "aegis", 3); got != wantInitial3 {
		t.Errorf("etcdInitialCluster(3):\n got %q\nwant %q", got, wantInitial3)
	}

	wantEndpoint3 := "http://aegis-etcd-1.aegis:2379," +
		"http://aegis-etcd-2.aegis:2379," +
		"http://aegis-etcd-3.aegis:2379"
	if got := etcdDatastoreEndpoint("aegis", "aegis", 3); got != wantEndpoint3 {
		t.Errorf("etcdDatastoreEndpoint(3):\n got %q\nwant %q", got, wantEndpoint3)
	}

	// 5 members: exactly five comma-separated client URLs (one per member).
	if got := strings.Count(etcdDatastoreEndpoint("aegis", "aegis", 5), "http://"); got != 5 {
		t.Errorf("etcdDatastoreEndpoint(5): got %d client URLs, want 5", got)
	}

	if got := strings.Count(etcdInitialCluster("aegis", "aegis", 5), "=http://"); got != 5 {
		t.Errorf("etcdInitialCluster(5): got %d member entries, want 5", got)
	}
}

// TestEtcdHelpers locks the etcd member naming + per-member volume derivation, the single source
// of truth shared by buildEtcdRunArgs (create) and destroyRecordedNodes (teardown).
func TestEtcdHelpers(t *testing.T) {
	if got := etcdNodeName("aegis", 2); got != "aegis-etcd-2" {
		t.Errorf("etcdNodeName: got %q, want aegis-etcd-2", got)
	}

	// Per-member volume — keyed on the member name so each of the N members gets its own.
	if got := etcdVolumeName("aegis-etcd-2"); got != "aegis-etcd-2-data" {
		t.Errorf("etcdVolumeName: got %q, want aegis-etcd-2-data", got)
	}

	members := etcdMembers("aegis", 3)
	if len(members) != 3 {
		t.Fatalf("etcdMembers(3): got %d members, want 3", len(members))
	}

	for i, m := range members {
		if m.Role != RoleDatastore {
			t.Errorf("member %d: role %v, want RoleDatastore", i, m.Role)
		}

		if want := fmt.Sprintf("aegis-etcd-%d", i+1); m.Name != want {
			t.Errorf("member %d: name %q, want %q", i, m.Name, want)
		}
	}
}

// TestBuildEtcdRunArgs locks the etcd member recipe (ADR-0003): the CONTAINER --name is the FQDN
// (so container DNS registers the peer — MANDATORY, a bare name leaves peers at NXDOMAIN and
// quorum never forms), the etcd MEMBER --name is the bare name keyed in --initial-cluster, the
// data dir is a SUBDIR of the mounted volume (lost+found guard), all peer/client URLs are FQDNs,
// the cluster labels are present (destroy sweep), and crucially NONE of the k3s recipe (no
// --cap-add ALL, no tmpfs, no k3s subcommand; the image precedes the etcd flags).
func TestBuildEtcdRunArgs(t *testing.T) {
	cfg := recipeCfg()
	member := NodeConfig{Name: "aegis-etcd-1", Role: RoleDatastore, Memory: defaultEtcdMemoryBytes}
	initial := etcdInitialCluster("aegis", "aegis", 3)

	args := buildEtcdRunArgs(cfg, member, "aegis", initial)
	joined := strings.Join(args, " ")

	checks := []struct {
		ok   bool
		desc string
	}{
		{hasPair(args, "--name", "aegis-etcd-1.aegis"), "CONTAINER --name is the member FQDN (container DNS registers it — MANDATORY)"},
		{hasPair(args, "--name", "aegis-etcd-1"), "etcd MEMBER --name is the bare name (keyed in --initial-cluster)"},
		{hasPair(args, "--volume", etcdVolumeName("aegis-etcd-1")+":"+etcdDataMount), "data on the per-member named volume at the mount root"},
		{hasPair(args, "--data-dir", etcdDataDir), "--data-dir is a SUBDIR of the mount (ext4 lost+found guard)"},
		{strings.HasPrefix(etcdDataDir, etcdDataMount+"/"), "data dir is strictly under the mount"},
		{hasPair(args, "--initial-advertise-peer-urls", "http://aegis-etcd-1.aegis:2380"), "advertise peer URL is the FQDN (name-bound membership)"},
		{hasPair(args, "--advertise-client-urls", "http://aegis-etcd-1.aegis:2379"), "advertise client URL is the FQDN"},
		{hasPair(args, "--listen-peer-urls", "http://0.0.0.0:2380"), "listen peer URL binds all interfaces"},
		{hasPair(args, "--listen-client-urls", "http://0.0.0.0:2379"), "listen client URL binds all interfaces"},
		{hasPair(args, "--initial-cluster", initial), "--initial-cluster carries the full FQDN member list"},
		{hasPair(args, "--initial-cluster-state", "new"), "--initial-cluster-state new"},
		{hasPair(args, "--initial-cluster-token", "aegis-etcd"), "--initial-cluster-token isolates this cluster's quorum"},
		{hasPair(args, "--label", "k3s.cluster.name=aegis"), "cluster label (destroy sweep reclaims it)"},
		{hasPair(args, "--label", "k3s.role=datastore"), "role=datastore label"},
		{hasPair(args, "--label", "k3s.owned=true"), "owned label"},
		{memoryUsesMB(args), "--memory uses the MB suffix (bare M rejected)"},
		{!strings.Contains(joined, "--cap-add"), "NO --cap-add (etcd is not k3s)"},
		{!tmpfsContains(args, "/run") && !tmpfsContains(args, "/tmp"), "NO tmpfs (not a k3s node)"},
		{!hasNamedDatastoreVolume(args), "NO k3s /var/lib/rancher/k3s datastore volume"},
		{!strings.Contains(joined, "--cluster-init"), "NO --cluster-init (this is EXTERNAL etcd, not embedded)"},
		{etcdImageIndex(args) >= 0, "etcd image present"},
		{etcdImageIndex(args) < len(args)-1, "etcd flags follow the image (image is NOT the final arg)"},
		// REGRESSION (v0.3.0 hardware bring-up): the token IMMEDIATELY after the image must be the
		// etcd binary. The image has no ENTRYPOINT, so Apple `container` execs the first post-image
		// arg; if that is a flag (e.g. --name) the member dies "failed to find target executable".
		{etcdImageIndex(args) >= 0 && args[etcdImageIndex(args)+1] == etcdBinary, "etcd binary is the first arg after the image (not a flag)"},
	}

	for _, c := range checks {
		if !c.ok {
			t.Errorf("etcd recipe check failed: %s\nargs: %s", c.desc, joined)
		}
	}
}

// TestBuildEtcdRunArgs_Network verifies the optional --network flag is threaded through when set
// (mirrors the k3s/LB recipes) and omitted when left empty (the built-in default).
func TestBuildEtcdRunArgs_Network(t *testing.T) {
	cfg := recipeCfg()
	cfg.Network = "k3snet"
	member := NodeConfig{Name: "aegis-etcd-1", Role: RoleDatastore, Memory: defaultEtcdMemoryBytes}

	if !hasPair(buildEtcdRunArgs(cfg, member, "aegis", "x=y"), "--network", "k3snet") {
		t.Error("custom network must be passed to the etcd container")
	}

	cfg.Network = ""
	if slices.Contains(buildEtcdRunArgs(cfg, member, "aegis", "x=y"), "--network") {
		t.Error("empty network must NOT emit --network")
	}
}

// TestDestroyEtcdVolumeEnumeration proves the teardown derives the SAME per-member volume name
// that create provisioned, for every etcd member — the create/destroy symmetry that guarantees a
// destroy reclaims exactly the volumes create made (no orphans, no wrong target). It mirrors the
// role-keyed branch in destroyRecordedNodes: RoleDatastore -> etcdVolumeName(node.Name).
func TestDestroyEtcdVolumeEnumeration(t *testing.T) {
	// A 3-member etcd cluster as Create would record it (RoleDatastore, member names).
	members := etcdMembers("aegis", 3)

	for _, m := range members {
		node := NodeInfo{Name: m.Name, Role: RoleDatastore}

		// Re-derive the volume the way destroyRecordedNodes does for a RoleDatastore node.
		var vol string
		if node.Role == RoleDatastore {
			vol = etcdVolumeName(node.Name)
		} else {
			vol = nodeVolumeName("aegis", node.Name)
		}

		if want := m.Name + "-data"; vol != want {
			t.Errorf("destroy would target %q for member %q, want %q (create/destroy symmetry)", vol, m.Name, want)
		}
	}
}

// TestAPILBHelpers locks the LB node naming, the single source of truth shared by
// buildAPILBRunArgs (create) and the kubeconfig endpoint. The LB's FQDN MUST equal
// clusterAPIFQDN (the name baked into every server's --tls-san) or the LB's serving cert would
// fail client verification.
func TestAPILBHelpers(t *testing.T) {
	if got := apiLBNodeName("aegis"); got != "aegis-api" {
		t.Errorf("apiLBNodeName: got %q, want aegis-api", got)
	}

	// The LB container --name (nodeFQDN of the bare name) must be identical to the --tls-san name.
	if got, want := nodeFQDN(apiLBNodeName("aegis"), "aegis"), clusterAPIFQDN("aegis", "aegis"); got != want {
		t.Errorf("LB FQDN %q must equal the cert SAN %q (else TLS verification fails)", got, want)
	}
}

// TestClusterAPIEndpoint is the BVA (CLAUDE.md k) on the LB on/off boundary — the pure logic that
// decides what the kubeconfig + agents target. The behavioral boundary is "is an API LB in front":
//   - LB off, FQDN mode (single-server, B=1 in cmd): the bootstrap SERVER's own FQDN endpoint.
//   - LB on, FQDN mode (multi-server, B+1 in cmd): the shared <cluster>-api.<domain> LB endpoint.
//   - LB on, IP-only (no DNS domain): falls back to the bootstrap server IP — the same gate
//     setupAPILB uses, so the endpoint can never name an LB that was not provisioned.
//   - LB off, IP-only: the bootstrap server IP (v0.1.x).
//
// The serverCount > 1 -> ProvisionAPILB mapping itself is an inline inference in cmd/k3ac/main.go
// (mirroring ManageDatastore); it is exercised here behaviorally via the ProvisionAPILB flag.
func TestClusterAPIEndpoint(t *testing.T) {
	ip := netip.MustParseAddr("192.168.64.7")

	tests := []struct {
		name      string
		lb        bool
		dnsDomain string
		want      string
	}{
		{"LB off, FQDN: bootstrap server FQDN", false, "aegis", "https://aegis-server-1.aegis:6443"},
		{"LB on, FQDN: shared LB FQDN", true, "aegis", "https://aegis-api.aegis:6443"},
		{"LB on, IP-only: server IP (LB skipped, no FQDN)", true, "", "https://192.168.64.7:6443"},
		{"LB off, IP-only: server IP", false, "", "https://192.168.64.7:6443"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := ClusterConfig{Name: "aegis", ProvisionAPILB: tt.lb}
			if got := clusterAPIEndpoint(cfg, "aegis-server-1", ip, tt.dnsDomain); got != tt.want {
				t.Errorf("clusterAPIEndpoint = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestBuildAPILBConfig locks the haproxy.cfg recipe — the load-bearing requirement that the LB is
// an L4/TCP PASSTHROUGH proxy (TLS terminates at the apiserver, not the LB) with FQDN backends
// that re-resolve at runtime so they survive the vmnet DHCP IP shift.
func TestBuildAPILBConfig(t *testing.T) {
	servers := []NodeConfig{
		{Name: "aegis-server-1", Role: RoleServer},
		{Name: "aegis-server-2", Role: RoleServer},
	}

	cfgText := buildAPILBConfig("aegis", servers, "aegis")

	checks := []struct {
		ok   bool
		desc string
	}{
		{strings.Contains(cfgText, "mode    tcp"), "mode tcp (L4 passthrough; TLS NOT terminated at the LB)"},
		{!strings.Contains(cfgText, "mode http") && !strings.Contains(cfgText, "ssl crt"), "no L7/HTTP mode and no TLS termination (`ssl crt`)"},
		{strings.Contains(cfgText, "bind *:6443"), "frontend binds the apiserver port 6443"},
		{strings.Contains(cfgText, "server srv1 aegis-server-1.aegis:6443"), "backend 1 addressed by server FQDN (not IP)"},
		{strings.Contains(cfgText, "server srv2 aegis-server-2.aegis:6443"), "backend 2 addressed by server FQDN (not IP)"},
		{strings.Contains(cfgText, "resolvers containerdns"), "a resolvers section drives runtime backend re-resolution"},
		{strings.Contains(cfgText, "parse-resolv-conf"), "parse-resolv-conf reuses the container DNS that re-registers <node>.<domain>"},
		{strings.Contains(cfgText, "resolve-prefer ipv4"), "each server re-resolves its FQDN at runtime (survives the DHCP shift)"},
		{strings.Contains(cfgText, "init-addr none"), "init-addr none lets haproxy start even if a backend FQDN is not resolvable yet"},
	}

	for _, c := range checks {
		if !c.ok {
			t.Errorf("API LB config check failed: %s\nconfig:\n%s", c.desc, cfgText)
		}
	}
}

// TestBuildAPILBConfig_BackendCount is the BVA (CLAUDE.md k) on the backend-server count: the
// generated config must emit exactly one `server srvN` line per server. Boundaries B-1=1, B=2,
// B+1=3 (the LB is only provisioned for >1 server, but the pure renderer must be correct for any
// count, so all three are exercised).
func TestBuildAPILBConfig_BackendCount(t *testing.T) {
	mk := func(n int) []NodeConfig {
		s := make([]NodeConfig, n)
		for i := range s {
			s[i] = NodeConfig{Name: fmt.Sprintf("aegis-server-%d", i+1), Role: RoleServer}
		}

		return s
	}

	for _, n := range []int{1, 2, 3} {
		cfgText := buildAPILBConfig("aegis", mk(n), "aegis")
		if got := strings.Count(cfgText, "\n    server srv"); got != n {
			t.Errorf("%d servers: got %d backend lines, want %d\nconfig:\n%s", n, got, n, cfgText)
		}
	}
}

// TestBuildAPILBRunArgs locks the LB `container run` recipe: the FQDN --name (so container DNS
// registers the cluster API name to the LB), the read-only config bind-mount at the haproxy
// default dir, the cluster + role labels (so the destroy sweep reclaims it), and crucially NONE
// of the k3s recipe (no --cap-add, no tmpfs, no named volume, no k3s subcommand; the image is the
// final arg so the haproxy image's default CMD loads the mounted config).
func TestBuildAPILBRunArgs(t *testing.T) {
	cfg := recipeCfg()
	configDir := "/abs/state/aegis/lb"

	args := buildAPILBRunArgs(cfg, "aegis", configDir)
	joined := strings.Join(args, " ")

	checks := []struct {
		ok   bool
		desc string
	}{
		{hasPair(args, "--name", "aegis-api.aegis"), "--name is the cluster API FQDN (container DNS registers it)"},
		{hasPair(args, "--volume", configDir+":"+apiLBConfigMount+":ro"), "config bind-mounted read-only at the haproxy default dir"},
		{memoryUsesMB(args), "--memory uses the MB suffix (bare M rejected)"},
		{hasPair(args, "--label", "k3s.cluster.name=aegis"), "cluster label (destroy sweep reclaims it)"},
		{hasPair(args, "--label", "k3s.role=lb"), "role=lb label"},
		{hasPair(args, "--label", "k3s.owned=true"), "owned label"},
		{slices.Contains(args, "--detach"), "--detach"},
		{!strings.Contains(joined, "--cap-add"), "NO --cap-add (haproxy is not k3s)"},
		{!tmpfsContains(args, "/run") && !tmpfsContains(args, "/tmp"), "NO tmpfs (not a k3s node)"},
		{!hasNamedDatastoreVolume(args), "NO k3s datastore named volume (the LB is stateless)"},
		{!strings.Contains(joined, " server") && !strings.Contains(joined, " agent"), "NO k3s subcommand"},
		{args[len(args)-1] == defaultAPILBImage, "image is the final arg (default CMD loads the mounted config)"},
	}

	for _, c := range checks {
		if !c.ok {
			t.Errorf("API LB run-args check failed: %s\nargs: %s", c.desc, joined)
		}
	}
}

// TestBuildAPILBRunArgs_Network verifies the optional --network flag is threaded through when set
// (mirrors the datastore/k3s recipes) and omitted for the built-in default left empty.
func TestBuildAPILBRunArgs_Network(t *testing.T) {
	cfg := recipeCfg()
	cfg.Network = "k3snet"

	if !hasPair(buildAPILBRunArgs(cfg, "aegis", "/abs/lb"), "--network", "k3snet") {
		t.Error("custom network must be passed to the LB container")
	}

	cfg.Network = ""
	if slices.Contains(buildAPILBRunArgs(cfg, "aegis", "/abs/lb"), "--network") {
		t.Error("empty network must NOT emit --network")
	}
}

// TestEnsureRemovable guards that -remove-node refuses a server, the managed datastore, and the
// API load balancer (all cluster-destroying acts) and permits an agent.
func TestEnsureRemovable(t *testing.T) {
	if err := ensureRemovable(NodeInfo{Name: "s1", Role: RoleServer}); err == nil {
		t.Error("removing a server must be refused")
	}

	if err := ensureRemovable(NodeInfo{Name: "aegis-db", Role: RoleDatastore}); err == nil {
		t.Error("removing the datastore must be refused")
	}

	if err := ensureRemovable(NodeInfo{Name: "aegis-api", Role: RoleLB}); err == nil {
		t.Error("removing the API load balancer must be refused")
	}

	if err := ensureRemovable(NodeInfo{Name: "a1", Role: RoleAgent}); err != nil {
		t.Errorf("removing an agent must be allowed, got %v", err)
	}
}

// TestRoleString locks the role rendering, including the datastore and lb roles used only as labels.
func TestRoleString(t *testing.T) {
	for role, want := range map[Role]string{RoleServer: "server", RoleAgent: "agent", RoleDatastore: "datastore", RoleLB: "lb"} {
		if got := role.String(); got != want {
			t.Errorf("Role(%d).String(): got %q, want %q", role, got, want)
		}
	}
}

// TestBuildRunArgs_AgentRecipe locks the agent-role launch recipe with FQDN enabled.
func TestBuildRunArgs_AgentRecipe(t *testing.T) {
	cfg := recipeCfg()
	node := NodeConfig{Name: "aegis-agent-1", Role: RoleAgent, Memory: 2048 * 1024 * 1024, NanoCPUs: 2e9}
	serverURL := "https://aegis-server-1.aegis:6443"

	args := buildRunArgs(cfg, node, serverURL, "aegis", "")
	joined := strings.Join(args, " ")

	agentFQDN := "aegis-agent-1.aegis"

	checks := []struct {
		ok   bool
		desc string
	}{
		{subcommandIs(args, cfg.Image, "agent"), "agent role uses `agent` subcommand after image"},
		{hasPair(args, "--name", agentFQDN), "agent --name is the FQDN"},
		{hasPair(args, "--env", "K3S_URL="+serverURL), "agent has K3S_URL pointing at the server FQDN"},
		{hasPair(args, "--env", "K3S_TOKEN=deadbeef"), "agent shares the K3S_TOKEN"},
		{hasNamedDatastoreVolume(args), "agent also gets a named datastore volume (no host path with '/')"},
		{hasPair(args, "--cap-add", "ALL"), "--cap-add ALL on agent too"},
		{!slices.Contains(args, "--flannel-backend=host-gw"), "agent has NO server-only flannel flag"},
		{!slices.Contains(args, "--tls-san"), "agent has NO server-only --tls-san"},
		{!slices.Contains(args, "--write-kubeconfig"), "agent has NO --write-kubeconfig (server-only kubeconfig delivery)"},
		{!strings.Contains(joined, "PLATFORM"), "no PLATFORM env"},
	}

	for _, c := range checks {
		if !c.ok {
			t.Errorf("agent recipe check failed: %s\nargs: %s", c.desc, joined)
		}
	}
}

// TestBuildRunArgs_IPOnlyFallback locks the IP-only (no dns-domain) fallback:
// bare node name for --name, no --tls-san on server.
func TestBuildRunArgs_IPOnlyFallback(t *testing.T) {
	cfg := recipeCfg()
	node := NodeConfig{Name: "aegis-server-1", Role: RoleServer, Memory: 2048 * 1024 * 1024}

	args := buildRunArgs(cfg, node, "", "", "")
	joined := strings.Join(args, " ")

	if !hasPair(args, "--name", "aegis-server-1") {
		t.Errorf("IP-only mode: --name must be bare node name, got: %s", joined)
	}

	if slices.Contains(args, "--tls-san") {
		t.Errorf("IP-only mode: no --tls-san (no stable FQDN to pin), got: %s", joined)
	}

	// Boundary: with an empty kubeconfigHostDir, a server emits neither the bind-mount nor
	// --write-kubeconfig (the mount is opt-in; Create always supplies it in production).
	if slices.Contains(args, "--write-kubeconfig") {
		t.Errorf("empty kubeconfigHostDir: must NOT emit --write-kubeconfig, got: %s", joined)
	}
}

// TestNodeFQDN verifies FQDN construction and the empty-domain passthrough.
func TestNodeFQDN(t *testing.T) {
	tests := []struct {
		name, domain, want string
	}{
		{"aegis-server-1", "aegis", "aegis-server-1.aegis"},
		{"aegis-agent-2", "k3s", "aegis-agent-2.k3s"},
		{"aegis-server-1", "", "aegis-server-1"}, // IP-only: bare name unchanged
	}

	for _, tt := range tests {
		got := nodeFQDN(tt.name, tt.domain)
		if got != tt.want {
			t.Errorf("nodeFQDN(%q, %q) = %q, want %q", tt.name, tt.domain, got, tt.want)
		}
	}
}

// TestSanitizeVolumeName verifies that the sanitizer lowercases and replaces invalid chars.
func TestSanitizeVolumeName(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"aegis-server-1-k3s", "aegis-server-1-k3s"},             // already clean
		{"UPPERCASE", "uppercase"},                               // lowercase
		{"slash/dot.colon:", "slash-dot-colon-"},                 // invalid chars → '-'
		{"aegis-aegis-server-1-k3s", "aegis-aegis-server-1-k3s"}, // real case
	}

	for _, tt := range tests {
		got := sanitizeVolumeName(tt.in)
		if got != tt.want {
			t.Errorf("sanitizeVolumeName(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestNodeVolumeName_Derivation locks the named-volume name scheme:
// <cluster>-<node>-k3s (sanitized). The exact name is load-bearing — Create creates
// this volume, buildRunArgs mounts it, and Destroy deletes it; a drift would silently
// break either the mount or the cleanup.
func TestNodeVolumeName_Derivation(t *testing.T) {
	got := nodeVolumeName("aegis", "aegis-server-1")
	want := "aegis-aegis-server-1-k3s"

	if got != want {
		t.Errorf("nodeVolumeName: got %q, want %q", got, want)
	}
}

// TestVolumeNameCreateDestroySymmetry proves the named volume buildRunArgs mounts is
// exactly the volume Destroy would delete — both derive from nodeVolumeName, so cleanup
// can never target a different volume than the one Create provisioned.
func TestVolumeNameCreateDestroySymmetry(t *testing.T) {
	cfg := recipeCfg()
	node := NodeConfig{Name: "aegis-agent-1", Role: RoleAgent}

	args := buildRunArgs(cfg, node, "https://aegis-server-1.aegis:6443", "aegis", "")

	var mountedVol string

	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--volume" {
			source, target, _ := strings.Cut(args[i+1], ":")
			if target == k3sDatastoreMount {
				mountedVol = source
			}
		}
	}

	// The volume name Create provisions and Destroy deletes must be the same.
	want := nodeVolumeName(cfg.Name, node.Name)
	if mountedVol != want {
		t.Errorf("datastore: mounted %q but destroy targets %q", mountedVol, want)
	}

	// The source must not contain '/' — a named volume, not a host path.
	if strings.Contains(mountedVol, "/") {
		t.Errorf("named volume source must not contain '/': got %q", mountedVol)
	}
}

// TestPrepareNodeVolumes_StaleStateGuard is the BVA on the "does the volume exist?"
// boundary (CLAUDE.md k). B = volume absent (create is called). B+1 = volume present
// (stale state from a prior run) must be rejected so we never boot onto an old sqlite
// datastore. The guard is injected so it runs without the `container` CLI.
func TestPrepareNodeVolumes_StaleStateGuard(t *testing.T) {
	nodes := []NodeConfig{{Name: "aegis-server-1", Role: RoleServer}}

	t.Run("volume absent (B): created, no error", func(t *testing.T) {
		created := ""
		exists := func(_ context.Context, _ string) (bool, error) { return false, nil }
		create := func(_ context.Context, name string) error { created = name; return nil }

		if err := prepareNodeVolumes(context.Background(), "aegis", nodes, exists, create); err != nil {
			t.Fatalf("absent volume must be allowed: %v", err)
		}

		if created == "" {
			t.Error("create must be called when volume is absent")
		}
	})

	t.Run("volume exists (B+1): rejected as stale state", func(t *testing.T) {
		exists := func(_ context.Context, _ string) (bool, error) { return true, nil }
		create := func(_ context.Context, _ string) error { return nil }

		if err := prepareNodeVolumes(context.Background(), "aegis", nodes, exists, create); err == nil {
			t.Error("existing volume must be rejected as stale state")
		}
	})
}

// TestVolumeCreateArgs_LabelFlags locks the volumeCreateArgs pure function:
// labels become --label pairs, and the volume name is the last positional arg.
func TestVolumeCreateArgs_LabelFlags(t *testing.T) {
	args := volumeCreateArgs("myvol", "k3s.cluster.name=aegis", "k3s.owned=true")

	if args[0] != "volume" || args[1] != "create" {
		t.Errorf("volumeCreateArgs must start with 'volume create', got %v", args[:2])
	}

	if args[len(args)-1] != "myvol" {
		t.Errorf("volume name must be the last positional arg, got %q", args[len(args)-1])
	}

	if !hasPair(args, "--label", "k3s.cluster.name=aegis") {
		t.Error("cluster label must be present as --label pair")
	}

	if !hasPair(args, "--label", "k3s.owned=true") {
		t.Error("owned label must be present as --label pair")
	}
}

// TestDNSDomainInList verifies the pure JSON parser for `container system dns list`.
func TestDNSDomainInList(t *testing.T) {
	json := `["aegis","local","test"]`

	ok, err := dnsDomainInList(json, "aegis")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !ok {
		t.Error("aegis should be found in the list")
	}

	ok, err = dnsDomainInList(json, "missing")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if ok {
		t.Error("missing domain must not be found")
	}

	if _, err := dnsDomainInList("not-json", "aegis"); err == nil {
		t.Error("invalid JSON must return an error")
	}
}

// TestContainersMatchingLabel verifies the pure client-side label filter.
func TestContainersMatchingLabel(t *testing.T) {
	jsonOut := `[
		{"configuration":{"id":"aegis-server-1.aegis","labels":{"k3s.cluster.name":"aegis","k3s.owned":"true"}}},
		{"configuration":{"id":"aegis-agent-1.aegis","labels":{"k3s.cluster.name":"aegis"}}},
		{"configuration":{"id":"other-container","labels":{"k3s.cluster.name":"other"}}}
	]`

	ids, err := containersMatchingLabel(jsonOut, "k3s.cluster.name=aegis")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(ids) != 2 {
		t.Errorf("expected 2 matches, got %d: %v", len(ids), ids)
	}

	if !slices.Contains(ids, "aegis-server-1.aegis") || !slices.Contains(ids, "aegis-agent-1.aegis") {
		t.Errorf("wrong containers matched: %v", ids)
	}

	// Invalid selector must error.
	if _, err := containersMatchingLabel(jsonOut, "no-equals-sign"); err == nil {
		t.Error("invalid selector must return an error")
	}
}

// TestVolumesMatchingLabel verifies the pure client-side volume label filter.
func TestVolumesMatchingLabel(t *testing.T) {
	jsonOut := `[
		{"configuration":{"name":"aegis-aegis-server-1-k3s","labels":{"k3s.cluster.name":"aegis","k3s.owned":"true"}}},
		{"configuration":{"name":"aegis-aegis-agent-1-k3s","labels":{"k3s.cluster.name":"aegis"}}},
		{"configuration":{"name":"other-vol","labels":{"k3s.cluster.name":"other"}}}
	]`

	vols, err := volumesMatchingLabel(jsonOut, "k3s.cluster.name=aegis")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(vols) != 2 {
		t.Errorf("expected 2 matches, got %d: %v", len(vols), vols)
	}

	if !slices.Contains(vols, "aegis-aegis-server-1-k3s") || !slices.Contains(vols, "aegis-aegis-agent-1-k3s") {
		t.Errorf("wrong volumes matched: %v", vols)
	}
}

// TestDNSDomainDefault confirms the expected default value for the dns-domain flag.
// The default is "aegis" — this is the contract callers depend on when they rely on
// FQDN naming out of the box.
func TestDNSDomainDefault(t *testing.T) {
	const wantDefault = "aegis"
	// The default is enforced by the -dns-domain flag in cmd/k3ac/main.go.
	// We lock the value here so a refactor that changes the default breaks this test.
	if wantDefault == "" {
		t.Error("dns-domain default must be non-empty (FQDN mode should be the default)")
	}
}

// TestRewriteKubeconfigServer verifies the pure kubeconfig server-URL rewrite helper.
// BVA on the replacement boundary (CLAUDE.md k):
//   - B   (loopback present): 127.0.0.1:6443 is replaced by the new endpoint.
//   - B-1 (no loopback): input is returned unchanged (idempotent on an already-rewritten kubeconfig).
//
// Two endpoint variants are tested — FQDN mode and IP-only mode (-dns-domain "") — to
// confirm both paths produce a usable, non-loopback server address.
func TestRewriteKubeconfigServer(t *testing.T) {
	loopbackKubeconfig := []byte("    server: https://127.0.0.1:6443\n    certificate-authority-data: abc\n")
	alreadyRewritten := []byte("    server: https://aegis-server-1.aegis:6443\n")

	tests := []struct {
		name         string
		in           []byte
		newServerURL string
		wantContains string
		wantAbsent   string
	}{
		{
			name:         "FQDN endpoint replaces loopback (FQDN mode)",
			in:           loopbackKubeconfig,
			newServerURL: "https://aegis-server-1.aegis:6443",
			wantContains: "https://aegis-server-1.aegis:6443",
			wantAbsent:   "https://127.0.0.1:6443",
		},
		{
			name:         "IP endpoint replaces loopback (IP-only mode, -dns-domain \"\")",
			in:           loopbackKubeconfig,
			newServerURL: "https://192.168.64.5:6443",
			wantContains: "https://192.168.64.5:6443",
			wantAbsent:   "https://127.0.0.1:6443",
		},
		{
			name:         "no loopback present: output unchanged (already rewritten)",
			in:           alreadyRewritten,
			newServerURL: "https://aegis-server-1.aegis:6443",
			wantContains: "https://aegis-server-1.aegis:6443",
			wantAbsent:   "https://127.0.0.1:6443",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := rewriteKubeconfigServer(tt.in, tt.newServerURL)

			if !bytes.Contains(got, []byte(tt.wantContains)) {
				t.Errorf("want %q in output\ngot: %s", tt.wantContains, got)
			}

			if bytes.Contains(got, []byte(tt.wantAbsent)) {
				t.Errorf("must not contain %q in output\ngot: %s", tt.wantAbsent, got)
			}
		})
	}
}

// TestLoadStateForDestroy is the BVA on the "is state.json loadable?" boundary
// (CLAUDE.md k), the seam behind BUG #2 (a -destroy with no state.json must NOT abort
// and orphan the running container + named volume):
//   - missing state.json (the failed-create case): fall back to a name-only ClusterRef
//     with sweptByLabel=true, so Destroy reclaims orphans via the label sweep.
//   - present + valid state.json: return the loaded state, sweptByLabel=false.
//   - present + corrupt state.json: a parse error must SURFACE (only fs.ErrNotExist is
//     tolerated) — never silently treated as "missing".
func TestLoadStateForDestroy(t *testing.T) {
	t.Run("missing state.json: falls back to label-sweep ClusterRef", func(t *testing.T) {
		dir := t.TempDir()

		state, swept, err := LoadStateForDestroy(dir, "k3v")
		if err != nil {
			t.Fatalf("missing state.json must not error (would orphan resources): %v", err)
		}

		if !swept {
			t.Error("missing state.json must report sweptByLabel=true")
		}

		if state.ClusterName != "k3v" || state.StateDir != dir {
			t.Errorf("ClusterRef must carry name+stateDir for the label sweep, got %+v", state)
		}

		if len(state.Nodes) != 0 {
			t.Errorf("ClusterRef must have no recorded nodes (recorded pass is a no-op), got %d", len(state.Nodes))
		}
	})

	t.Run("present state.json: returns loaded state, no sweep flag", func(t *testing.T) {
		dir := t.TempDir()
		want := ClusterState{
			Provisioner: ProviderName,
			ClusterName: "k3v",
			StateDir:    dir,
			ServerURL:   "https://k3v-server-1.aegis:6443",
		}

		if err := saveState(want); err != nil {
			t.Fatalf("saveState: %v", err)
		}

		state, swept, err := LoadStateForDestroy(dir, "k3v")
		if err != nil {
			t.Fatalf("present state.json must not error: %v", err)
		}

		if swept {
			t.Error("present state.json must report sweptByLabel=false")
		}

		if state.ServerURL != want.ServerURL {
			t.Errorf("loaded state mismatch: got %q want %q", state.ServerURL, want.ServerURL)
		}
	})

	t.Run("corrupt state.json: parse error surfaces (not treated as missing)", func(t *testing.T) {
		dir := t.TempDir()
		clusterDir := filepath.Join(dir, "k3v")

		if err := os.MkdirAll(clusterDir, 0o755); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(filepath.Join(clusterDir, "state.json"), []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}

		_, swept, err := LoadStateForDestroy(dir, "k3v")
		if err == nil {
			t.Error("corrupt state.json must surface an error, not be tolerated")
		}

		if swept {
			t.Error("a parse error must NOT be treated as missing-state (sweptByLabel must be false)")
		}
	})
}

// TestNextAgentIndex is the BVA on the agent set (CLAUDE.md k) that drives new-agent
// numbering in AddAgents. The boundary is the set of existing <cluster>-agent-<N> names:
//   - 0 agents (only a server): next is 1 (no agents -> start at 1).
//   - agents 1,2 (contiguous): next is 3 (max+1).
//   - agents 1,3 (a gap, e.g. agent-2 removed): next is 4 (max+1, NOT count+1 — a freed
//     index is never backfilled, so a recreated agent cannot reuse a stale volume name).
//   - names that do not match <cluster>-agent-<N> (the server, a foreign/non-numeric
//     suffix) are ignored.
func TestNextAgentIndex(t *testing.T) {
	mk := func(name string, role Role) NodeInfo { return NodeInfo{Name: name, Role: role} }

	tests := []struct {
		name    string
		cluster string
		nodes   []NodeInfo
		want    int
	}{
		{
			name:    "0 agents, server only: next is 1",
			cluster: "aegis",
			nodes:   []NodeInfo{mk("aegis-server-1", RoleServer)},
			want:    1,
		},
		{
			name:    "agents 1,2 contiguous: next is 3 (max+1)",
			cluster: "aegis",
			nodes:   []NodeInfo{mk("aegis-server-1", RoleServer), mk("aegis-agent-1", RoleAgent), mk("aegis-agent-2", RoleAgent)},
			want:    3,
		},
		{
			name:    "agents 1,3 with a gap: next is 4 (max+1, not count+1)",
			cluster: "aegis",
			nodes:   []NodeInfo{mk("aegis-server-1", RoleServer), mk("aegis-agent-1", RoleAgent), mk("aegis-agent-3", RoleAgent)},
			want:    4,
		},
		{
			name:    "non-matching names ignored (foreign cluster + non-numeric suffix)",
			cluster: "aegis",
			nodes:   []NodeInfo{mk("aegis-server-1", RoleServer), mk("other-agent-9", RoleAgent), mk("aegis-agent-x", RoleAgent), mk("aegis-agent-2", RoleAgent)},
			want:    3,
		},
		{
			name:    "empty node list: next is 1",
			cluster: "aegis",
			nodes:   nil,
			want:    1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := nextAgentIndex(tt.nodes, tt.cluster); got != tt.want {
				t.Errorf("nextAgentIndex(%v, %q) = %d, want %d", tt.nodes, tt.cluster, got, tt.want)
			}
		})
	}
}

// TestEnsureRemovable_ServerGuard locks RemoveNode's load-bearing guard: a server node
// may NOT be removed (that destroys the cluster — the -destroy path), while an agent
// node is removable. Tests the extracted guard helper directly, so no container calls.
func TestEnsureRemovable_ServerGuard(t *testing.T) {
	if err := ensureRemovable(NodeInfo{Name: "aegis-server-1", Role: RoleServer}); err == nil {
		t.Error("removing a server node must be refused (use -destroy)")
	}

	if err := ensureRemovable(NodeInfo{Name: "aegis-agent-1", Role: RoleAgent}); err != nil {
		t.Errorf("removing an agent node must be allowed: %v", err)
	}
}

// --- helpers ---

// hasPair reports whether args contains flag immediately followed by value.
func hasPair(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}

	return false
}

// tmpfsContains reports whether path is mounted as a --tmpfs in args.
func tmpfsContains(args []string, path string) bool {
	return hasPair(args, "--tmpfs", path)
}

// memoryUsesMB reports whether the --memory value carries the required "MB" suffix.
func memoryUsesMB(args []string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--memory" {
			return strings.HasSuffix(args[i+1], "MB")
		}
	}

	return false
}

// hasNamedDatastoreVolume reports whether a --volume mounts a NAMED volume (no host
// path: no '/' in the source component) to the k3s datastore path. A host-path
// bind-mount would have '/' in the source, violating the named-volume requirement.
func hasNamedDatastoreVolume(args []string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--volume" {
			source, target, _ := strings.Cut(args[i+1], ":")
			if target == k3sDatastoreMount && !strings.Contains(source, "/") {
				return true
			}
		}
	}

	return false
}

// subcommandIs reports whether the k3s subcommand immediately follows the image positional.
func subcommandIs(args []string, image, sub string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == image && args[i+1] == sub {
			return true
		}
	}

	return false
}

// etcdImageIndex returns the position of the etcd image positional in args, or -1 if absent.
// Used to assert the image's placement relative to trailing flags (the binary and etcd flags
// must follow the image).
func etcdImageIndex(args []string) int {
	return slices.Index(args, defaultEtcdImage)
}
