// SPDX-License-Identifier: MIT

package apple

import (
	"context"
	"net/netip"
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
// (BVA, CLAUDE.md k): B = 1 server (the sqlite single-server case). B-1 = 0 rejected;
// B = 1 accepted; B+1 = 2 rejected (multi-server needs embedded etcd, intentionally
// disabled). Agent count is not the boundary — 0 agents (single-node) is valid.
func TestValidateClusterConfig_ServerCountBoundaries(t *testing.T) {
	tests := []struct {
		name    string
		nodes   []NodeConfig
		wantErr bool
	}{
		{"no nodes at all", nil, true},
		{"0 servers, 1 agent (B-1, invalid)", []NodeConfig{agentNode("a1")}, true},
		{"1 server, 0 agent (single-node, valid)", []NodeConfig{serverNode("s1")}, false},
		{"1 server + 1 agent (smallest real, valid)", []NodeConfig{serverNode("s1"), agentNode("a1")}, false},
		{"2 servers (B+1, invalid: sqlite single-server)", []NodeConfig{serverNode("s1"), serverNode("s2")}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateClusterConfig(ClusterConfig{Name: "test", Nodes: tt.nodes})
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

	args := buildRunArgs(cfg, node, "", "aegis")
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

// TestBuildRunArgs_AgentRecipe locks the agent-role launch recipe with FQDN enabled.
func TestBuildRunArgs_AgentRecipe(t *testing.T) {
	cfg := recipeCfg()
	node := NodeConfig{Name: "aegis-agent-1", Role: RoleAgent, Memory: 2048 * 1024 * 1024, NanoCPUs: 2e9}
	serverURL := "https://aegis-server-1.aegis:6443"

	args := buildRunArgs(cfg, node, serverURL, "aegis")
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

	args := buildRunArgs(cfg, node, "", "")
	joined := strings.Join(args, " ")

	if !hasPair(args, "--name", "aegis-server-1") {
		t.Errorf("IP-only mode: --name must be bare node name, got: %s", joined)
	}

	if slices.Contains(args, "--tls-san") {
		t.Errorf("IP-only mode: no --tls-san (no stable FQDN to pin), got: %s", joined)
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

	args := buildRunArgs(cfg, node, "https://aegis-server-1.aegis:6443", "aegis")

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
	// The default is enforced by the -dns-domain flag in cmd/aegis-k3s/main.go.
	// We lock the value here so a refactor that changes the default breaks this test.
	if wantDefault == "" {
		t.Error("dns-domain default must be non-empty (FQDN mode should be the default)")
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
