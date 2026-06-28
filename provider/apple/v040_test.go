// SPDX-License-Identifier: MIT

package apple

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestBuildRunArgs_K3sArgPassthrough locks item 1: operator k3s args are appended VERBATIM and
// LAST (after every built-in flag), so they can override built-ins (k3s last-one-wins). BVA on
// the arg count: zero (nothing appended), one, many.
func TestBuildRunArgs_K3sArgPassthrough(t *testing.T) {
	cfg := recipeCfg()

	t.Run("zero extra args: tail is unchanged", func(t *testing.T) {
		node := NodeConfig{Name: "aegis-server-1", Role: RoleServer}
		args := buildRunArgs(cfg, node, "", "aegis", "/abs/state/aegis")

		// With no ExtraArgs the last arg is a built-in (the FQDN tls-san), never an operator arg.
		if last := args[len(args)-1]; strings.HasPrefix(last, "--disable") {
			t.Errorf("no extra args expected, got trailing %q", last)
		}
	})

	t.Run("server args appended verbatim, last, after built-ins", func(t *testing.T) {
		extra := []string{"--disable=traefik", "--cluster-cidr=10.42.0.0/16"}
		node := NodeConfig{Name: "aegis-server-1", Role: RoleServer, ExtraArgs: extra}
		args := buildRunArgs(cfg, node, "", "aegis", "/abs/state/aegis")

		// The final len(extra) elements must be exactly the operator args, in order.
		tail := args[len(args)-len(extra):]
		if !slices.Equal(tail, extra) {
			t.Errorf("extra args must be the verbatim tail; got tail %v want %v", tail, extra)
		}

		// And they must sit AFTER a known built-in server flag (so overrides win).
		builtinIdx := slices.Index(args, "--flannel-backend=host-gw")
		firstExtraIdx := slices.Index(args, extra[0])
		if builtinIdx < 0 || firstExtraIdx < builtinIdx {
			t.Errorf("extra args must come after built-in flags: builtin@%d extra@%d", builtinIdx, firstExtraIdx)
		}
	})

	t.Run("agent args appended verbatim", func(t *testing.T) {
		extra := []string{"--node-taint=role=worker:NoSchedule"}
		node := NodeConfig{Name: "aegis-agent-1", Role: RoleAgent, ExtraArgs: extra}
		args := buildRunArgs(cfg, node, "https://aegis-server-1.aegis:6443", "aegis", "")

		if last := args[len(args)-1]; last != extra[0] {
			t.Errorf("agent extra arg must be the verbatim tail, got %q", last)
		}
	})
}

// TestBuildRunArgs_NodeLabels locks item 3: each label becomes a --node-label KEY=VALUE on the
// node's subcommand, for both roles. BVA on the label count: zero / one / many.
func TestBuildRunArgs_NodeLabels(t *testing.T) {
	cfg := recipeCfg()

	t.Run("zero labels: no --node-label", func(t *testing.T) {
		node := NodeConfig{Name: "aegis-server-1", Role: RoleServer}
		args := buildRunArgs(cfg, node, "", "aegis", "/abs/state/aegis")

		if slices.Contains(args, "--node-label") {
			t.Errorf("no labels expected, found --node-label: %v", args)
		}
	})

	t.Run("many labels on a server", func(t *testing.T) {
		node := NodeConfig{Name: "aegis-server-1", Role: RoleServer, Labels: []string{"tier=db", "zone=a"}}
		args := buildRunArgs(cfg, node, "", "aegis", "/abs/state/aegis")

		if !hasPair(args, "--node-label", "tier=db") || !hasPair(args, "--node-label", "zone=a") {
			t.Errorf("both labels must be --node-label pairs: %v", args)
		}
	})

	t.Run("label on an agent too", func(t *testing.T) {
		node := NodeConfig{Name: "aegis-agent-1", Role: RoleAgent, Labels: []string{"role=worker"}}
		args := buildRunArgs(cfg, node, "https://aegis-server-1.aegis:6443", "aegis", "")

		if !hasPair(args, "--node-label", "role=worker") {
			t.Errorf("agent must also get --node-label: %v", args)
		}
	})
}

// TestBuildRunArgs_ManifestMounts locks item 5: the bootstrap server (kubeconfigHostDir set)
// bind-mounts each manifest at k3sManifestsDir/<basename>; agents and non-bootstrap servers
// (no kubeconfigHostDir) get none. BVA on the manifest count: zero / one / many.
func TestBuildRunArgs_ManifestMounts(t *testing.T) {
	t.Run("bootstrap server mounts every manifest individually", func(t *testing.T) {
		cfg := recipeCfg()
		cfg.Manifests = []string{"/abs/m/app.yaml", "/abs/m/ns.yaml"}
		node := NodeConfig{Name: "aegis-server-1", Role: RoleServer}

		args := buildRunArgs(cfg, node, "", "aegis", "/abs/state/aegis")

		for _, m := range cfg.Manifests {
			target := k3sManifestsDir + "/" + filepath.Base(m)
			if !hasPair(args, "--volume", m+":"+target) {
				t.Errorf("manifest %q must mount at %q: %v", m, target, args)
			}
		}
	})

	t.Run("zero manifests: no manifests-dir mount", func(t *testing.T) {
		cfg := recipeCfg() // no manifests
		node := NodeConfig{Name: "aegis-server-1", Role: RoleServer}

		args := buildRunArgs(cfg, node, "", "aegis", "/abs/state/aegis")

		if strings.Contains(strings.Join(args, " "), k3sManifestsDir) {
			t.Errorf("no manifests expected, found a manifests mount: %v", args)
		}
	})

	t.Run("agent never mounts manifests", func(t *testing.T) {
		cfg := recipeCfg()
		cfg.Manifests = []string{"/abs/m/app.yaml"}
		node := NodeConfig{Name: "aegis-agent-1", Role: RoleAgent}

		args := buildRunArgs(cfg, node, "https://aegis-server-1.aegis:6443", "aegis", "")

		if strings.Contains(strings.Join(args, " "), k3sManifestsDir) {
			t.Errorf("agent must not mount manifests: %v", args)
		}
	})

	t.Run("non-bootstrap server (no kubeconfig dir) mounts no manifests", func(t *testing.T) {
		cfg := recipeCfg()
		cfg.DatastoreEndpoint = "postgres://kine:pw@db.aegis:5432/kine"
		cfg.Manifests = []string{"/abs/m/app.yaml"}
		node := NodeConfig{Name: "aegis-server-2", Role: RoleServer}

		// kubeconfigHostDir "" == HA join server; only the bootstrap mounts manifests.
		args := buildRunArgs(cfg, node, "", "aegis", "")

		if strings.Contains(strings.Join(args, " "), k3sManifestsDir) {
			t.Errorf("non-bootstrap server must not mount manifests: %v", args)
		}
	})
}

// TestManifestMountArgs is the pure mount-path BVA (zero / one / many) backing item 5.
func TestManifestMountArgs(t *testing.T) {
	if got := manifestMountArgs(nil); got != nil {
		t.Errorf("zero manifests must yield nil, got %v", got)
	}

	one := manifestMountArgs([]string{"/abs/app.yaml"})
	wantTarget := k3sManifestsDir + "/app.yaml"
	if len(one) != 2 || one[0] != "--volume" || one[1] != "/abs/app.yaml:"+wantTarget {
		t.Errorf("one manifest: got %v, want --volume /abs/app.yaml:%s", one, wantTarget)
	}

	many := manifestMountArgs([]string{"/a/x.yaml", "/b/y.yaml"})
	if len(many) != 4 {
		t.Errorf("two manifests must yield 4 args (2 pairs), got %v", many)
	}
}

// TestResolveManifests covers item 5's create-time guard: absolute-path resolution, existence,
// regular-file (not directory), and the basename-collision rejection. BVA: one file (ok),
// many distinct (ok), duplicate basename (error), missing file (error), directory (error),
// empty path (error).
func TestResolveManifests(t *testing.T) {
	dir := t.TempDir()

	writeFile := func(name string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("apiVersion: v1\nkind: Namespace\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		return p
	}

	app := writeFile("app.yaml")
	ns := writeFile("ns.yaml")

	t.Run("one file resolves to an absolute path", func(t *testing.T) {
		got, err := resolveManifests([]string{app})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if len(got) != 1 || !filepath.IsAbs(got[0]) {
			t.Errorf("want one absolute path, got %v", got)
		}
	})

	t.Run("many distinct basenames pass", func(t *testing.T) {
		got, err := resolveManifests([]string{app, ns})
		if err != nil || len(got) != 2 {
			t.Fatalf("two distinct manifests must pass: got %v err %v", got, err)
		}
	})

	t.Run("duplicate basename rejected", func(t *testing.T) {
		// Same basename "app.yaml" in two different dirs collide at one in-node target.
		sub := filepath.Join(dir, "sub")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}

		dup := filepath.Join(sub, "app.yaml")
		if err := os.WriteFile(dup, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}

		if _, err := resolveManifests([]string{app, dup}); err == nil {
			t.Error("duplicate basename must be rejected (would collide at the same mount target)")
		}
	})

	t.Run("missing file rejected", func(t *testing.T) {
		if _, err := resolveManifests([]string{filepath.Join(dir, "nope.yaml")}); err == nil {
			t.Error("missing manifest file must error")
		}
	})

	t.Run("directory rejected", func(t *testing.T) {
		if _, err := resolveManifests([]string{dir}); err == nil {
			t.Error("a directory must be rejected (pass individual files)")
		}
	})

	t.Run("empty path rejected", func(t *testing.T) {
		if _, err := resolveManifests([]string{""}); err == nil {
			t.Error("empty manifest path must error")
		}
	})
}

// TestBuildRunArgs_CPUs locks item 6: NodeConfig.NanoCPUs maps to --cpus <whole vCPUs>. BVA:
// default 2 vCPU, an override (4), and the zero boundary (no --cpus emitted).
func TestBuildRunArgs_CPUs(t *testing.T) {
	cfg := recipeCfg()

	tests := []struct {
		name     string
		nanoCPUs int64
		wantCPUs string // "" means --cpus must be absent
	}{
		{"default 2 vCPU", 2e9, "2"},
		{"override 4 vCPU", 4e9, "4"},
		{"override 1 vCPU", 1e9, "1"},
		{"zero: no --cpus flag", 0, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := NodeConfig{Name: "aegis-server-1", Role: RoleServer, NanoCPUs: tt.nanoCPUs}
			args := buildRunArgs(cfg, node, "", "aegis", "/abs/state/aegis")

			if tt.wantCPUs == "" {
				if slices.Contains(args, "--cpus") {
					t.Errorf("zero NanoCPUs must emit no --cpus, got %v", args)
				}

				return
			}

			if !hasPair(args, "--cpus", tt.wantCPUs) {
				t.Errorf("want --cpus %s, got %v", tt.wantCPUs, args)
			}
		})
	}
}

// TestSummarizeState locks item 2's pure role-counting: servers and agents are counted, the
// managed datastore (RoleDatastore) is excluded from both. BVA: zero agents; a multi-server HA
// shape with a datastore present.
func TestSummarizeState(t *testing.T) {
	mk := func(name string, role Role) NodeInfo { return NodeInfo{Name: name, Role: role} }

	t.Run("single server, zero agents", func(t *testing.T) {
		s := summarizeState(ClusterState{
			ClusterName: "aegis",
			ServerURL:   "https://aegis-server-1.aegis:6443",
			Image:       "rancher/k3s:v1.32.5-k3s1",
			Nodes:       []NodeInfo{mk("aegis-server-1", RoleServer)},
		})

		if s.Servers != 1 || s.Agents != 0 {
			t.Errorf("got servers=%d agents=%d, want 1/0", s.Servers, s.Agents)
		}

		if s.Name != "aegis" || s.ServerURL == "" || s.Image == "" {
			t.Errorf("summary fields not carried through: %+v", s)
		}
	})

	t.Run("HA: 2 servers + 1 agent + datastore (datastore excluded)", func(t *testing.T) {
		s := summarizeState(ClusterState{
			Nodes: []NodeInfo{
				mk("aegis-db", RoleDatastore),
				mk("aegis-server-1", RoleServer),
				mk("aegis-server-2", RoleServer),
				mk("aegis-agent-1", RoleAgent),
			},
		})

		if s.Servers != 2 || s.Agents != 1 {
			t.Errorf("got servers=%d agents=%d, want 2/1 (datastore excluded)", s.Servers, s.Agents)
		}
	})
}

// TestListClusters covers item 2's file scan. BVA: zero clusters (empty dir AND missing dir),
// one cluster, many; a directory without state.json is skipped; results are sorted by name.
func TestListClusters(t *testing.T) {
	save := func(stateDir, name string, nodes []NodeInfo) {
		st := ClusterState{Provisioner: ProviderName, ClusterName: name, StateDir: stateDir, Nodes: nodes}
		if err := saveState(st); err != nil {
			t.Fatal(err)
		}
	}

	mk := func(name string, role Role) NodeInfo { return NodeInfo{Name: name, Role: role} }

	t.Run("missing state dir: no clusters, no error", func(t *testing.T) {
		got, err := ListClusters(filepath.Join(t.TempDir(), "does-not-exist"))
		if err != nil || got != nil {
			t.Errorf("missing dir must yield (nil, nil), got %v err %v", got, err)
		}
	})

	t.Run("empty state dir: no clusters", func(t *testing.T) {
		got, err := ListClusters(t.TempDir())
		if err != nil || len(got) != 0 {
			t.Errorf("empty dir must yield no clusters, got %v err %v", got, err)
		}
	})

	t.Run("one cluster", func(t *testing.T) {
		dir := t.TempDir()
		save(dir, "solo", []NodeInfo{mk("solo-server-1", RoleServer)})

		got, err := ListClusters(dir)
		if err != nil {
			t.Fatal(err)
		}

		if len(got) != 1 || got[0].Name != "solo" || got[0].Servers != 1 {
			t.Errorf("one cluster expected, got %+v", got)
		}
	})

	t.Run("many clusters sorted, stray dir skipped", func(t *testing.T) {
		dir := t.TempDir()
		save(dir, "bravo", []NodeInfo{mk("bravo-server-1", RoleServer), mk("bravo-agent-1", RoleAgent)})
		save(dir, "alpha", []NodeInfo{mk("alpha-server-1", RoleServer)})

		// A directory with no state.json must be skipped, not error.
		if err := os.MkdirAll(filepath.Join(dir, "stray"), 0o755); err != nil {
			t.Fatal(err)
		}

		got, err := ListClusters(dir)
		if err != nil {
			t.Fatal(err)
		}

		if len(got) != 2 {
			t.Fatalf("expected 2 clusters (stray skipped), got %d: %+v", len(got), got)
		}

		if got[0].Name != "alpha" || got[1].Name != "bravo" {
			t.Errorf("clusters must be sorted by name, got %q then %q", got[0].Name, got[1].Name)
		}
	})
}

// TestStopStartOrder locks item 4's pure ordering. STOP = agents -> servers -> datastore;
// START = the exact reverse. BVA: zero agents; the full HA shape (datastore + 2 servers +
// 2 agents). Stable order within a role is also checked (server-1 before server-2).
func TestStopStartOrder(t *testing.T) {
	mk := func(name string, role Role) NodeInfo { return NodeInfo{Name: name, ID: name + ".aegis", Role: role} }

	names := func(nodes []NodeInfo) []string {
		out := make([]string, len(nodes))
		for i, n := range nodes {
			out[i] = n.Name
		}

		return out
	}

	t.Run("full HA shape", func(t *testing.T) {
		// Deliberately unsorted input to prove the ordering does the work.
		nodes := []NodeInfo{
			mk("aegis-agent-1", RoleAgent),
			mk("aegis-db", RoleDatastore),
			mk("aegis-server-1", RoleServer),
			mk("aegis-agent-2", RoleAgent),
			mk("aegis-server-2", RoleServer),
		}

		stop := names(stopOrder(nodes))
		wantStop := []string{"aegis-agent-1", "aegis-agent-2", "aegis-server-1", "aegis-server-2", "aegis-db"}
		if !slices.Equal(stop, wantStop) {
			t.Errorf("stop order:\n got %v\nwant %v", stop, wantStop)
		}

		start := names(startOrder(nodes))
		wantStart := []string{"aegis-db", "aegis-server-1", "aegis-server-2", "aegis-agent-1", "aegis-agent-2"}
		if !slices.Equal(start, wantStart) {
			t.Errorf("start order:\n got %v\nwant %v", start, wantStart)
		}
	})

	t.Run("full HA shape with API LB", func(t *testing.T) {
		// RoleLB (v0.3.0) must slot AFTER servers and BEFORE agents on start — agents join through
		// the LB FQDN, so it must be up before them; stop is the reverse (agents, LB, servers,
		// datastore). REGRESSION: the v0.4.0 lifecycle predates RoleLB, so a catch-all default rank
		// put the LB first on start, and Start then ran ip_forward on the haproxy node and aborted
		// with EPERM (caught in v0.4.0 hardware bring-up).
		nodes := []NodeInfo{
			mk("aegis-agent-1", RoleAgent),
			mk("aegis-api", RoleLB),
			mk("aegis-etcd-1", RoleDatastore),
			mk("aegis-server-1", RoleServer),
			mk("aegis-server-2", RoleServer),
		}

		stop := names(stopOrder(nodes))
		wantStop := []string{"aegis-agent-1", "aegis-api", "aegis-server-1", "aegis-server-2", "aegis-etcd-1"}
		if !slices.Equal(stop, wantStop) {
			t.Errorf("stop order:\n got %v\nwant %v", stop, wantStop)
		}

		start := names(startOrder(nodes))
		wantStart := []string{"aegis-etcd-1", "aegis-server-1", "aegis-server-2", "aegis-api", "aegis-agent-1"}
		if !slices.Equal(start, wantStart) {
			t.Errorf("start order:\n got %v\nwant %v", start, wantStart)
		}
	})

	t.Run("zero agents (server + datastore only)", func(t *testing.T) {
		nodes := []NodeInfo{
			mk("aegis-server-1", RoleServer),
			mk("aegis-db", RoleDatastore),
		}

		stop := names(stopOrder(nodes))
		if !slices.Equal(stop, []string{"aegis-server-1", "aegis-db"}) {
			t.Errorf("stop (no agents): got %v", stop)
		}

		start := names(startOrder(nodes))
		if !slices.Equal(start, []string{"aegis-db", "aegis-server-1"}) {
			t.Errorf("start (no agents): got %v", start)
		}
	})

	t.Run("single server only", func(t *testing.T) {
		nodes := []NodeInfo{mk("aegis-server-1", RoleServer)}

		if got := names(stopOrder(nodes)); !slices.Equal(got, []string{"aegis-server-1"}) {
			t.Errorf("stop single: got %v", got)
		}

		if got := names(startOrder(nodes)); !slices.Equal(got, []string{"aegis-server-1"}) {
			t.Errorf("start single: got %v", got)
		}
	})

	t.Run("start is the exact reverse of stop", func(t *testing.T) {
		nodes := []NodeInfo{
			mk("aegis-agent-1", RoleAgent),
			mk("aegis-server-1", RoleServer),
			mk("aegis-db", RoleDatastore),
		}

		stop := stopOrder(nodes)
		start := startOrder(nodes)

		for i := range stop {
			if stop[i].Name != start[len(start)-1-i].Name {
				t.Errorf("start must be reverse of stop: stop=%v start=%v", names(stop), names(start))

				break
			}
		}
	})
}
