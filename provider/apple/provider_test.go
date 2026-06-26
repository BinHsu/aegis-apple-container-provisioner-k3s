// SPDX-License-Identifier: MIT

package apple

import (
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
// /run and /tmp; /var must NOT be tmpfs (it is the persistent datastore bind-mount);
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

// cfg is a shared fixture for the recipe-lock tests.
func recipeCfg() ClusterConfig {
	return ClusterConfig{
		Name:       "aegis",
		Image:      "rancher/k3s:v1.32.5-k3s1",
		Network:    "default",
		StateDir:   "/tmp/aegis-state",
		Token:      "deadbeef",
		ClusterDNS: "aegis-k3s.local",
	}
}

// TestBuildRunArgs_ServerRecipe locks the server-role launch recipe.
func TestBuildRunArgs_ServerRecipe(t *testing.T) {
	cfg := recipeCfg()
	node := NodeConfig{Name: "aegis-server-1", Role: RoleServer, Memory: 2048 * 1024 * 1024, NanoCPUs: 2e9}

	args := buildRunArgs(cfg, node, "")
	joined := strings.Join(args, " ")

	checks := []struct {
		ok   bool
		desc string
	}{
		{hasPair(args, "--cap-add", "ALL"), "--cap-add ALL (G1: embedded containerd needs CAP_SYS_ADMIN)"},
		{memoryUsesMB(args), "--memory uses MB suffix (G4: bare M rejected)"},
		{!tmpfsContains(args, "/var"), "/var NOT tmpfs (it is the persistent datastore bind-mount)"},
		{hasDatastoreVolume(args), "datastore --volume ...:/var/lib/rancher/k3s present (G3)"},
		{slices.Contains(args, "--flannel-backend=host-gw"), "--flannel-backend=host-gw on server (G2)"},
		{slices.Contains(args, "--tls-san"), "--tls-san present on server (IP-change cert stability)"},
		{hasPair(args, "--tls-san", "aegis-k3s.local"), "--tls-san value is the stable cluster DNS"},
		{subcommandIs(args, cfg.Image, "server"), "server role uses `server` subcommand after image"},
		{hasPair(args, "--env", "K3S_TOKEN=deadbeef"), "K3S_TOKEN env present"},
		{!strings.Contains(joined, "K3S_URL"), "server has NO K3S_URL (it IS the server)"},
		{!strings.Contains(joined, "PLATFORM"), "no PLATFORM env (Talos-only, removed)"},
		{!strings.Contains(joined, "TALOSSKU"), "no TALOSSKU env (Talos-only, removed)"},
		{slices.Contains(args, "--detach"), "--detach"},
	}

	for _, c := range checks {
		if !c.ok {
			t.Errorf("server recipe check failed: %s\nargs: %s", c.desc, joined)
		}
	}
}

// TestBuildRunArgs_AgentRecipe locks the agent-role launch recipe.
func TestBuildRunArgs_AgentRecipe(t *testing.T) {
	cfg := recipeCfg()
	node := NodeConfig{Name: "aegis-agent-1", Role: RoleAgent, Memory: 2048 * 1024 * 1024, NanoCPUs: 2e9}
	serverURL := "https://192.168.64.20:6443"

	args := buildRunArgs(cfg, node, serverURL)
	joined := strings.Join(args, " ")

	checks := []struct {
		ok   bool
		desc string
	}{
		{subcommandIs(args, cfg.Image, "agent"), "agent role uses `agent` subcommand after image"},
		{hasPair(args, "--env", "K3S_URL="+serverURL), "agent has K3S_URL pointing at the server"},
		{hasPair(args, "--env", "K3S_TOKEN=deadbeef"), "agent shares the K3S_TOKEN"},
		{hasDatastoreVolume(args), "agent also gets a datastore bind-mount"},
		{hasPair(args, "--cap-add", "ALL"), "--cap-add ALL on agent too"},
		{!slices.Contains(args, "--flannel-backend=host-gw"), "agent has NO server-only flannel flag"},
		{!slices.Contains(args, "--tls-san"), "agent has NO server-only --tls-san"},
	}

	for _, c := range checks {
		if !c.ok {
			t.Errorf("agent recipe check failed: %s\nargs: %s", c.desc, joined)
		}
	}
}

// TestNodeDatastoreHostPath_Derivation locks the host-path scheme:
// <stateDir>/<clusterName>/<nodeName>/k3s. The exact string is load-bearing — Create
// mkdir's it, buildRunArgs bind-mounts it, and Destroy removes it, so a drift here would
// silently break either the mount or the cleanup.
func TestNodeDatastoreHostPath_Derivation(t *testing.T) {
	cfg := ClusterConfig{Name: "aegis", StateDir: filepath.Join("_out", "clusters")}

	got := nodeDatastoreHostPath(cfg, NodeConfig{Name: "aegis-server-1"})
	want := filepath.Join("_out", "clusters", "aegis", "aegis-server-1", "k3s")

	if got != want {
		t.Errorf("datastore host dir: got %q, want %q", got, want)
	}
}

// TestDatastorePath_CreateDestroySymmetry proves the dir buildRunArgs bind-mounts is
// exactly the dir Destroy would remove — both derive from nodeDatastoreHostPath, so cleanup
// can never target a different (or empty) directory than the one Create populated.
func TestDatastorePath_CreateDestroySymmetry(t *testing.T) {
	cfg := ClusterConfig{Name: "aegis", StateDir: filepath.Join("_out", "clusters")}
	node := NodeConfig{Name: "aegis-agent-1", Role: RoleAgent}

	args := buildRunArgs(cfg, node, "https://192.168.64.20:6443")

	var mountedHost string

	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--volume" {
			host, target, _ := strings.Cut(args[i+1], ":")
			if target == k3sDatastoreMount {
				mountedHost = host
			}
		}
	}

	want := nodeDatastoreHostPath(cfg, node)
	if mountedHost != want {
		t.Errorf("datastore: mounted %q but destroy targets %q", mountedHost, want)
	}
}

// TestPrepareNodeDatastores_StaleStateGuard is the BVA on the "is the datastore dir empty?"
// boundary (CLAUDE.md k). B = 0 entries. B-1 (dir absent) and B (present, empty) must pass
// and create the dir; B+1 (>= 1 entry, stale state) must be rejected so we never boot onto
// an old sqlite datastore.
func TestPrepareNodeDatastores_StaleStateGuard(t *testing.T) {
	cfgFor := func(stateDir string) ClusterConfig {
		return ClusterConfig{
			Name:     "aegis",
			StateDir: stateDir,
			Nodes:    []NodeConfig{{Name: "aegis-server-1", Role: RoleServer}},
		}
	}

	t.Run("absent dir (B-1): allowed, dir created", func(t *testing.T) {
		cfg := cfgFor(t.TempDir()) // node dir does not exist yet

		if err := prepareNodeDatastores(cfg); err != nil {
			t.Fatalf("absent datastore dir must be allowed: %v", err)
		}

		dir := nodeDatastoreHostPath(cfg, cfg.Nodes[0])
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("expected %q created: %v", dir, err)
		}
	})

	t.Run("present empty dir (B): allowed", func(t *testing.T) {
		cfg := cfgFor(t.TempDir())
		dir := nodeDatastoreHostPath(cfg, cfg.Nodes[0])

		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}

		if err := prepareNodeDatastores(cfg); err != nil {
			t.Fatalf("present-but-empty datastore dir must be allowed: %v", err)
		}
	})

	t.Run("non-empty dir (B+1): rejected", func(t *testing.T) {
		cfg := cfgFor(t.TempDir())
		dir := nodeDatastoreHostPath(cfg, cfg.Nodes[0])

		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(filepath.Join(dir, "state.db"), []byte("stale"), 0o644); err != nil {
			t.Fatal(err)
		}

		if err := prepareNodeDatastores(cfg); err == nil {
			t.Error("non-empty (stale) datastore dir must be rejected, telling the operator to destroy first")
		}
	})
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

// hasDatastoreVolume reports whether a --volume binds something to the k3s datastore path.
func hasDatastoreVolume(args []string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--volume" && strings.HasSuffix(args[i+1], ":"+k3sDatastoreMount) {
			return true
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
