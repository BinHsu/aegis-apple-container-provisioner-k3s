// SPDX-License-Identifier: MIT

package apple

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// v060_test.go is the BVA (CLAUDE.md k) recipe-lock + pure-logic coverage for the v0.6.0 day-2
// operations. Everything tested here is hardware-FREE: the container orchestration (run/stop/start/
// exec, kubectl against a live cluster) is exercised only on real hardware — those boundaries are
// named in each feature's HARDWARE-VERIFICATION CHECKLIST, not stubbed with return-true.

// --- Shared: the -force guard ---

// TestEnsureForced is the BVA on the destructive-op guard: force=false refuses (errForceRequired) and
// prints the plan; force=true proceeds (nil) and prints nothing. The boolean is the contract.
func TestEnsureForced(t *testing.T) {
	plan := []string{"delete the datastore", "restart everything"}

	t.Run("false refuses and prints the plan", func(t *testing.T) {
		var buf bytes.Buffer

		err := ensureForced(false, &buf, "-restore", plan)
		if !errors.Is(err, errForceRequired) {
			t.Errorf("force=false must return errForceRequired, got %v", err)
		}

		for _, line := range plan {
			if !strings.Contains(buf.String(), line) {
				t.Errorf("plan line %q must be printed before refusing:\n%s", line, buf.String())
			}
		}
	})

	t.Run("true proceeds silently", func(t *testing.T) {
		var buf bytes.Buffer

		if err := ensureForced(true, &buf, "-restore", plan); err != nil {
			t.Errorf("force=true must proceed (nil), got %v", err)
		}

		if buf.Len() != 0 {
			t.Errorf("force=true must print nothing, got %q", buf.String())
		}
	})
}

// --- Feature 1: snapshot / restore ---

// TestSnapshotPathConstruction locks the host snapshot path: <clusterDir>/snapshots/<cluster>-<UTC>.db.
func TestSnapshotPathConstruction(t *testing.T) {
	dir := snapshotsDir("/abs/state/aegis")
	if dir != "/abs/state/aegis/snapshots" {
		t.Errorf("snapshotsDir: got %q", dir)
	}

	ts := time.Date(2026, 6, 28, 14, 30, 5, 0, time.UTC)

	name := snapshotFileName("aegis", ts)
	if name != "aegis-20260628T143005Z.db" {
		t.Errorf("snapshotFileName: got %q, want aegis-20260628T143005Z.db", name)
	}

	// Different cluster name + different instant must change the file name (uniqueness/ordering).
	if snapshotFileName("k3v", ts) == name {
		t.Error("snapshot file name must include the cluster name")
	}

	if snapshotFileName("aegis", ts.Add(time.Second)) == name {
		t.Error("snapshot file name must include the timestamp (so repeats are unique)")
	}
}

// TestBuildEtcdSnapshotArgs locks the snapshot helper recipe: a WRITABLE host backup mount (so the
// snapshot lands on the host), a read-only client TLS mount, the etcdctl client TLS flags, the FQDN
// client endpoint, and `snapshot save /backup/<file>`. Image falls back to the etcd default.
// TestEtcdctlHealthArgs locks the quorum-gate command (the provision-flakiness fix): it must carry
// the member's mutual-TLS bundle (ca + server cert/key under etcdTLSMount), the target endpoint,
// and the `endpoint health` verb — the invocation whose success proves a leader exists before the
// bootstrap k3s server launches (a bare TCP dial did not, which fatal-exited the first server).
func TestEtcdctlHealthArgs(t *testing.T) {
	args := etcdctlHealthArgs("https://aegis-etcd-1.aegis:2379")
	joined := strings.Join(args, " ")

	checks := []struct {
		ok   bool
		desc string
	}{
		{len(args) > 0 && args[0] == "etcdctl", "etcdctl is the executable"},
		{slices.Contains(args, "endpoint") && slices.Contains(args, "health"), "the 'endpoint health' verb is present"},
		{hasPair(args, "--cacert", etcdTLSMount+"/"+etcdCACertFile), "CA from the member's TLS mount"},
		{hasPair(args, "--cert", etcdTLSMount+"/"+etcdServerCertFile), "member server cert (mutual-TLS client)"},
		{hasPair(args, "--key", etcdTLSMount+"/"+etcdServerKeyFile), "member server key"},
		{hasPair(args, "--endpoints", "https://aegis-etcd-1.aegis:2379"), "the target endpoint"},
		{!strings.Contains(joined, "http://"), "TLS only — no plaintext endpoint"},
	}

	for _, c := range checks {
		if !c.ok {
			t.Errorf("etcdctl health args check failed: %s\nargs: %s", c.desc, joined)
		}
	}
}

func TestBuildEtcdSnapshotArgs(t *testing.T) {
	cfg := recipeCfg()
	args := buildEtcdSnapshotArgs(cfg, "aegis-etcd-snapshot", "aegis-etcd-1.aegis",
		"/abs/state/aegis/snapshots", "/abs/state/aegis/etcd-tls/client", "aegis-20260628T143005Z.db")
	joined := strings.Join(args, " ")

	checks := []struct {
		ok   bool
		desc string
	}{
		{hasPair(args, "--volume", "/abs/state/aegis/snapshots:"+etcdBackupMount), "backup dir mounted WRITABLE (no :ro) so the snapshot reaches the host"},
		{hasPair(args, "--volume", "/abs/state/aegis/etcd-tls/client:"+etcdTLSMount+":ro"), "client TLS mounted read-only"},
		{slices.Contains(args, "--endpoints=https://aegis-etcd-1.aegis:2379"), "FQDN client endpoint"},
		{slices.Contains(args, "--cacert"), "etcdctl client TLS --cacert present"},
		{slices.Contains(args, defaultEtcdImage), "etcd image falls back to the pinned default"},
		{slices.Contains(args, etcdctlBinary), "etcdctl binary named explicitly (image has no entrypoint)"},
		{strings.Contains(joined, "snapshot save "+etcdBackupMount+"/aegis-20260628T143005Z.db"), "snapshot save targets the host-backed /backup path"},
	}

	for _, c := range checks {
		if !c.ok {
			t.Errorf("snapshot args check failed: %s\nargs: %s", c.desc, joined)
		}
	}

	// The backup mount must NOT be read-only (etcdctl writes the snapshot there).
	if hasPair(args, "--volume", "/abs/state/aegis/snapshots:"+etcdBackupMount+":ro") {
		t.Error("the backup mount must be writable, not :ro")
	}
}

// TestBuildEtcdRestoreArgs is the load-bearing restore string construction (the most likely place for
// a data-destroying bug). It locks: the SHARED initial-cluster (every member identical), THIS member's
// own peer URL + name, the cluster token == the create recipe, the fresh data-volume mount + data-dir,
// and that restore carries NO client TLS (it is a local unpack). BVA: a non-first member (member-2).
func TestBuildEtcdRestoreArgs(t *testing.T) {
	cfg := recipeCfg()
	initialCluster := etcdInitialCluster("aegis", "aegis", 3)
	member := NodeConfig{Name: "aegis-etcd-2", Role: RoleDatastore, Memory: defaultEtcdMemoryBytes}

	args := buildEtcdRestoreArgs(cfg, "aegis-etcd-2-restore", member, "aegis", initialCluster, "/host/snap", "snap.db")
	joined := strings.Join(args, " ")

	checks := []struct {
		ok   bool
		desc string
	}{
		{hasPair(args, "--initial-cluster", initialCluster), "initial-cluster is the SHARED FQDN mesh (must match every member)"},
		{hasPair(args, "--initial-advertise-peer-urls", "https://aegis-etcd-2.aegis:2380"), "this member's OWN peer URL (member-2, not member-1)"},
		{hasPair(args, "--initial-cluster-token", "aegis-etcd"), "cluster token == create recipe (etcdClusterToken)"},
		{hasPair(args, "--name", "aegis-etcd-2"), "etcd member name is this member"},
		{hasPair(args, "--data-dir", etcdDataDir), "restore into the lost+found-safe data subdir"},
		{hasPair(args, "--volume", etcdVolumeName("aegis-etcd-2")+":"+etcdDataMount), "the FRESH data volume is mounted writable"},
		{hasPair(args, "--volume", "/host/snap:"+etcdBackupMount+":ro"), "the snapshot is mounted read-only"},
		{strings.Contains(joined, "snapshot restore "+etcdBackupMount+"/snap.db"), "restore reads the bind-mounted snapshot"},
	}

	for _, c := range checks {
		if !c.ok {
			t.Errorf("restore args check failed: %s\nargs: %s", c.desc, joined)
		}
	}

	// Restore is a LOCAL unpack — no client TLS flags, no endpoints.
	if slices.Contains(args, "--cacert") || strings.Contains(joined, "--endpoints") {
		t.Errorf("restore must NOT carry client TLS/endpoints (it is a local unpack): %s", joined)
	}

	// The token MUST equal etcdClusterToken — a different token would form an unrelated cluster.
	if !hasPair(args, "--initial-cluster-token", etcdClusterToken("aegis")) {
		t.Error("restore token must equal etcdClusterToken(cluster) so the restored quorum is the same cluster")
	}
}

// TestBuildEtcdVerifyArgs locks the post-restore marker read: client TLS, FQDN endpoint, and a
// keys-only get of the marker key.
func TestBuildEtcdVerifyArgs(t *testing.T) {
	cfg := recipeCfg()
	args := buildEtcdVerifyArgs(cfg, "aegis-etcd-verify", "aegis-etcd-1.aegis", "/abs/client", restoreMarkerKey)
	joined := strings.Join(args, " ")

	if !slices.Contains(args, "--endpoints=https://aegis-etcd-1.aegis:2379") {
		t.Errorf("verify must target the member FQDN endpoint: %s", joined)
	}

	if !strings.Contains(joined, "get "+restoreMarkerKey+" --keys-only") {
		t.Errorf("verify must read the marker key keys-only: %s", joined)
	}

	if !slices.Contains(args, "--cacert") {
		t.Errorf("verify is a client op and must carry TLS: %s", joined)
	}
}

// --- Feature 2: upgrade / rollback ---

// TestK3sNodesInUpgradeOrder is the BVA on the rolling-replace ordering: servers BEFORE agents, both
// in recorded order, and the datastore + LB EXCLUDED (a k3s image roll never recreates them).
func TestK3sNodesInUpgradeOrder(t *testing.T) {
	mk := func(name string, role Role) NodeInfo { return NodeInfo{ID: name, Name: name, Role: role} }

	nodes := []NodeInfo{
		mk("aegis-etcd-1", RoleDatastore),
		mk("aegis-agent-1", RoleAgent),
		mk("aegis-server-1", RoleServer),
		mk("aegis-api", RoleLB),
		mk("aegis-server-2", RoleServer),
		mk("aegis-agent-2", RoleAgent),
	}

	got := k3sNodesInUpgradeOrder(nodes)

	want := []string{"aegis-server-1", "aegis-server-2", "aegis-agent-1", "aegis-agent-2"}
	if len(got) != len(want) {
		t.Fatalf("got %d k3s nodes, want %d (datastore/LB excluded): %v", len(got), len(want), got)
	}

	for i, n := range got {
		if n.Name != want[i] {
			t.Errorf("upgrade order[%d] = %q, want %q", i, n.Name, want[i])
		}

		if n.Role == RoleDatastore || n.Role == RoleLB {
			t.Errorf("datastore/LB must be excluded from the upgrade order, found %q", n.Name)
		}
	}
}

// TestUpgradePins locks the -upgrade image bookkeeping: the new image moves in and the CURRENT image
// (resolved) is pinned for rollback. BVA: a normal current image, and an empty one (pre-v0.2.0 state).
func TestUpgradePins(t *testing.T) {
	prev, next := upgradePins("rancher/k3s:v1.32.5-k3s1", "rancher/k3s:v1.33.0-k3s1")
	if prev != "rancher/k3s:v1.32.5-k3s1" || next != "rancher/k3s:v1.33.0-k3s1" {
		t.Errorf("upgradePins = (%q,%q)", prev, next)
	}

	// Empty current image (old state) must pin the resolved default, never "".
	prev, _ = upgradePins("", "rancher/k3s:v1.33.0-k3s1")
	if prev != defaultK3sImage {
		t.Errorf("empty current image must pin defaultK3sImage, got %q", prev)
	}
}

// TestRollbackPins is the BVA on the rollback target: no previous image -> error (nothing to roll
// back to); a previous image -> (target=previous, recordedPrevious=current-resolved) so the rollback
// is itself reversible.
func TestRollbackPins(t *testing.T) {
	if _, _, err := rollbackPins(ClusterState{ClusterName: "aegis"}); err == nil {
		t.Error("no PreviousImage must error (nothing to roll back to)")
	}

	target, prev, err := rollbackPins(ClusterState{
		ClusterName: "aegis", Image: "rancher/k3s:v1.33.0-k3s1", PreviousImage: "rancher/k3s:v1.32.5-k3s1",
	})
	if err != nil {
		t.Fatalf("rollbackPins: %v", err)
	}

	if target != "rancher/k3s:v1.32.5-k3s1" {
		t.Errorf("rollback target must be the previous image, got %q", target)
	}

	if prev != "rancher/k3s:v1.33.0-k3s1" {
		t.Errorf("rollback must pin the image it rolled away from (reversible), got %q", prev)
	}
}

// TestKubectlArgs locks the host-kubectl vectors the rolling replace runs. The load-bearing one is
// drain: WITHOUT --ignore-daemonsets and --delete-emptydir-data a drain stalls and the upgrade hangs.
func TestKubectlArgs(t *testing.T) {
	kc := "/state/aegis/kubeconfig"

	if got := strings.Join(kubectlCordonArgs(kc, "aegis-agent-1"), " "); got != "--kubeconfig "+kc+" cordon aegis-agent-1" {
		t.Errorf("cordon args: %q", got)
	}

	drain := strings.Join(kubectlDrainArgs(kc, "aegis-agent-1"), " ")
	for _, want := range []string{"drain aegis-agent-1", "--ignore-daemonsets", "--delete-emptydir-data"} {
		if !strings.Contains(drain, want) {
			t.Errorf("drain args missing %q: %q", want, drain)
		}
	}

	wait := strings.Join(kubectlWaitReadyArgs(kc, "aegis-agent-1", 5*time.Minute), " ")
	for _, want := range []string{"wait", "--for=condition=Ready", "node/aegis-agent-1", "--timeout=300s"} {
		if !strings.Contains(wait, want) {
			t.Errorf("wait args missing %q: %q", want, wait)
		}
	}

	// delete-node clears the stale Node object before recreate so the node rejoins on its new DHCP
	// IP; --ignore-not-found keeps it idempotent across retries.
	del := strings.Join(kubectlDeleteNodeArgs(kc, "aegis-agent-1"), " ")
	for _, want := range []string{"delete node aegis-agent-1", "--ignore-not-found"} {
		if !strings.Contains(del, want) {
			t.Errorf("delete-node args missing %q: %q", want, del)
		}
	}
}

// TestNodeConfigFromInfo locks the recreate-faithfulness: a recreated node reproduces the recorded
// size, labels, and verbatim k3s args (so -upgrade/-rotate-token do not silently reset them).
func TestNodeConfigFromInfo(t *testing.T) {
	info := NodeInfo{
		Name: "aegis-agent-1", Role: RoleAgent, Memory: 4096 * 1024 * 1024, NanoCPUs: 4e9,
		Labels: []string{"tier=edge"}, ExtraArgs: []string{"--kubelet-arg=foo"},
	}

	cfg := nodeConfigFromInfo(info)
	if cfg.Name != info.Name || cfg.Role != info.Role || cfg.Memory != info.Memory || cfg.NanoCPUs != info.NanoCPUs {
		t.Errorf("nodeConfigFromInfo did not carry size/role: %+v", cfg)
	}

	if !slices.Equal(cfg.Labels, info.Labels) || !slices.Equal(cfg.ExtraArgs, info.ExtraArgs) {
		t.Errorf("nodeConfigFromInfo dropped labels/args: %+v", cfg)
	}
}

// --- Feature 3: cert / token rotation ---

// TestCertRegenWiring locks that cert rotation regenerates a bundle covering EXACTLY the recorded etcd
// members (the wiring from saved state -> generateEtcdTLS). A member added/removed since create must
// be reflected, so the regenerated certs always match the live quorum.
func TestCertRegenWiring(t *testing.T) {
	state := ClusterState{ClusterName: "aegis", Nodes: []NodeInfo{
		{Name: "aegis-etcd-1", Role: RoleDatastore},
		{Name: "aegis-etcd-2", Role: RoleDatastore},
		{Name: "aegis-etcd-3", Role: RoleDatastore},
		{Name: "aegis-server-1", Role: RoleServer},
	}}

	members := datastoreNodes(state.Nodes)
	if len(members) != 3 {
		t.Fatalf("datastoreNodes must select the 3 etcd members, got %d", len(members))
	}

	names := make([]string, len(members))
	for i, m := range members {
		names[i] = m.Name
	}

	bundle, err := generateEtcdTLS(state.ClusterName, "aegis", names)
	if err != nil {
		t.Fatalf("generateEtcdTLS: %v", err)
	}

	if len(bundle.MemberCerts) != 3 {
		t.Errorf("regenerated bundle must cover all 3 live members, got %d", len(bundle.MemberCerts))
	}

	for _, name := range names {
		if _, ok := bundle.MemberCerts[name]; !ok {
			t.Errorf("regenerated bundle missing a cert for live member %q", name)
		}
	}
}

// TestBuildK3sCertRotateArgs locks the OFFLINE k3s certificate-rotate one-shot: the server's k3s
// volume mounted at the datastore path, the k3s image, and `certificate rotate --data-dir`. Crucially
// it must NOT carry the `server` subcommand (it is a cert-rotate run, not a server run).
func TestBuildK3sCertRotateArgs(t *testing.T) {
	cfg := recipeCfg()
	args := buildK3sCertRotateArgs(cfg, NodeConfig{Name: "aegis-server-1", Role: RoleServer})
	joined := strings.Join(args, " ")

	if !hasPair(args, "--volume", nodeVolumeName("aegis", "aegis-server-1")+":"+k3sDatastoreMount) {
		t.Errorf("cert-rotate must mount the server's k3s state volume: %s", joined)
	}

	if !strings.Contains(joined, "certificate rotate --data-dir "+k3sDatastoreMount) {
		t.Errorf("cert-rotate must run `certificate rotate --data-dir`: %s", joined)
	}

	if slices.Contains(args, "server") {
		t.Errorf("cert-rotate one-shot must NOT run the server subcommand: %s", joined)
	}
}

// TestK3sTokenRotateExecArgs locks the `k3s token rotate` vector (old + new token).
func TestK3sTokenRotateExecArgs(t *testing.T) {
	args := k3sTokenRotateExecArgs("oldtok", "newtok")

	want := []string{"k3s", "token", "rotate", "--token", "oldtok", "--new-token", "newtok"}
	if !slices.Equal(args, want) {
		t.Errorf("token rotate args = %v, want %v", args, want)
	}
}

// TestTokenRotationReRegisters is the token-rotation state-update + re-registration check: the new
// token replaces the old one in state, and a node recreated from the post-rotation cfg bakes the NEW
// K3S_TOKEN into its `container run` env (so agents re-register with it and survive a cold restart).
func TestTokenRotationReRegisters(t *testing.T) {
	state := ClusterState{
		ClusterName: "aegis", Image: defaultK3sImage, Network: "default", Token: "oldtok",
		DatastoreEndpoint: "https://aegis-etcd-1.aegis:2379",
		Nodes:             []NodeInfo{{ID: "aegis-agent-1.aegis", Name: "aegis-agent-1", Role: RoleAgent}},
	}

	cfg := clusterConfigFromState(state, "/state", "/state/aegis")
	cfg.Token = "newtok" // RotateToken sets this before recreating nodes

	// state-update: the helper that recreate uses must carry the new token, not the old one.
	if cfg.Token != "newtok" {
		t.Fatalf("recreate cfg must carry the new token, got %q", cfg.Token)
	}

	args := buildRunArgs(cfg, NodeConfig{Name: "aegis-agent-1", Role: RoleAgent}, "https://aegis-api.aegis:6443", "aegis", "")
	if !hasPair(args, "--env", "K3S_TOKEN=newtok") {
		t.Errorf("recreated agent must re-register with the NEW token: %v", args)
	}

	if hasPair(args, "--env", "K3S_TOKEN=oldtok") {
		t.Errorf("recreated agent must NOT keep the old token: %v", args)
	}
}

// TestReplaceNodeInState locks the in-place node refresh (used after every recreate/restart to record
// the node's new DHCP IP): the matching ID is replaced, a non-matching ID is a no-op.
func TestReplaceNodeInState(t *testing.T) {
	state := ClusterState{Nodes: []NodeInfo{
		{ID: "aegis-server-1.aegis", Name: "aegis-server-1", Role: RoleServer},
		{ID: "aegis-agent-1.aegis", Name: "aegis-agent-1", Role: RoleAgent},
	}}

	if !replaceNodeInState(&state, NodeInfo{ID: "aegis-agent-1.aegis", Name: "aegis-agent-1", Role: RoleAgent, Memory: 99}) {
		t.Error("a matching ID must be replaced")
	}

	if state.Nodes[1].Memory != 99 {
		t.Errorf("matching node not updated: %+v", state.Nodes[1])
	}

	if replaceNodeInState(&state, NodeInfo{ID: "nope.aegis"}) {
		t.Error("a non-matching ID must be a no-op")
	}
}

// TestEtcdClusterToken locks the single-source-of-truth token used by BOTH the create recipe and the
// restore recipe (a mismatch would make restore form an unrelated cluster).
func TestEtcdClusterToken(t *testing.T) {
	if got := etcdClusterToken("aegis"); got != "aegis-etcd" {
		t.Errorf("etcdClusterToken = %q, want aegis-etcd", got)
	}
}

// --- v0.6.0 recreate layer 2: stale IP-bound serving certs ---

// TestStaleServingCertPaths is the BVA on the role gate that decides WHICH serving certs a recreate
// clears. A recreate boots the node on a new DHCP IP, so the IP-bound SERVING certs on the preserved
// volume must be regenerated — but the cluster CA and client/identity certs must NOT. Boundaries: a
// SERVER (apiserver dynamiclistener cache + kubelet serving cert), an AGENT (kubelet serving cert
// only, no apiserver), and the two NON-k3s roles (datastore/LB → nothing to clear).
func TestStaleServingCertPaths(t *testing.T) {
	t.Run("server clears the dynamiclistener cache AND its kubelet serving cert", func(t *testing.T) {
		got := staleServingCertPaths(RoleServer)

		want := []string{
			"/var/lib/rancher/k3s/server/tls/dynamic-cert.json",
			"/var/lib/rancher/k3s/agent/serving-kubelet.crt",
			"/var/lib/rancher/k3s/agent/serving-kubelet.key",
		}
		if !slices.Equal(got, want) {
			t.Errorf("server serving certs:\n got %v\nwant %v", got, want)
		}
	})

	t.Run("agent clears only the kubelet serving cert (no apiserver, no dynamic-cert.json)", func(t *testing.T) {
		got := staleServingCertPaths(RoleAgent)

		want := []string{
			"/var/lib/rancher/k3s/agent/serving-kubelet.crt",
			"/var/lib/rancher/k3s/agent/serving-kubelet.key",
		}
		if !slices.Equal(got, want) {
			t.Errorf("agent serving certs:\n got %v\nwant %v", got, want)
		}

		for _, p := range got {
			if strings.Contains(p, "dynamic-cert.json") {
				t.Errorf("an agent has no apiserver dynamiclistener cache to clear: %v", got)
			}
		}
	})

	t.Run("non-k3s roles clear nothing", func(t *testing.T) {
		for _, role := range []Role{RoleDatastore, RoleLB} {
			if got := staleServingCertPaths(role); got != nil {
				t.Errorf("role %v is not a k3s node and must clear no serving certs, got %v", role, got)
			}
		}
	})

	t.Run("never clears the cluster CA or any client/identity cert", func(t *testing.T) {
		// The cluster's root of trust + identity certs live on the same volume; clearing any of them
		// would break trust cluster-wide. Every cleared path must be a SERVING leaf only.
		forbidden := []string{
			"server-ca", "client-ca", "request-header-ca", "peer-ca", "etcd/", // CAs
			"service.key",                                                                                // service-account signing key
			"client-admin", "client-kubelet.crt", "client-controller", "client-k3s", "client-kube-proxy", // client identity
		}

		for _, role := range []Role{RoleServer, RoleAgent} {
			for _, p := range staleServingCertPaths(role) {
				for _, bad := range forbidden {
					if strings.Contains(p, bad) {
						t.Errorf("role %v would clear a CA/identity cert %q (matched %q) — only serving certs are safe", role, p, bad)
					}
				}
			}
		}
	})
}

// TestRmStaleServingCertsArgs locks the `container exec` argv that clears the stale serving certs: it
// is `rm -f <path>...` (idempotent, and `rm` is not a k3s multi-call symlink so exec dispatches it
// cleanly), and it is nil for the non-k3s roles so recreateK3sNode skips the exec entirely.
func TestRmStaleServingCertsArgs(t *testing.T) {
	t.Run("k3s roles build rm -f with every serving-cert path", func(t *testing.T) {
		for _, role := range []Role{RoleServer, RoleAgent} {
			args := rmStaleServingCertsArgs(role)

			if len(args) < 3 || args[0] != "rm" || args[1] != "-f" {
				t.Fatalf("role %v: must start `rm -f`, got %v", role, args)
			}

			if !slices.Equal(args[2:], staleServingCertPaths(role)) {
				t.Errorf("role %v: argv tail must be exactly the cert paths: %v", role, args)
			}
		}
	})

	t.Run("non-k3s roles produce nil (no exec)", func(t *testing.T) {
		for _, role := range []Role{RoleDatastore, RoleLB} {
			if args := rmStaleServingCertsArgs(role); args != nil {
				t.Errorf("role %v must produce no argv (skip the exec), got %v", role, args)
			}
		}
	})
}
