// SPDX-License-Identifier: MIT

package apple

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// snapshot.go implements the v0.6.0 etcd backup/restore for the managed datastore.
//
//   - Snapshot (safe, read-only): an ephemeral etcdctl container connects to a live member over the
//     existing client mutual-TLS and streams `snapshot save` to a HOST path under the cluster state
//     dir. Delivery is the host BIND-MOUNT (ADR-0001) — the snapshot lands directly on the host via a
//     writable /backup mount, never `container cp`. Mirrors how the kubeconfig arrives.
//   - Restore (DESTRUCTIVE, needs -force): stops the cluster, rebuilds a FRESH 3-node etcd cluster from
//     the snapshot (one `etcdctl snapshot restore` per member into its own fresh data volume, all with
//     the SAME FQDN --initial-cluster + --initial-cluster-token so the restored quorum keeps the
//     IP-shift-survivable FQDN peer mesh), relaunches the members, then restarts the servers/agents
//     against the restored etcd (the datastore TLS is unchanged by a restore).
//
// ETCD RESTORE SEMANTICS (the load-bearing detail — get this wrong and the cluster never re-forms):
// `etcdctl snapshot restore` is a LOCAL, offline operation. It rewrites a snapshot into a data dir and
// stamps the member with a NEW member ID and a NEW cluster ID derived from --initial-cluster +
// --initial-cluster-token. So EVERY member must restore from the SAME snapshot file with an IDENTICAL
// --initial-cluster and --initial-cluster-token, but each with its OWN --name and
// --initial-advertise-peer-urls. The members then boot normally (buildEtcdRunArgs) and form one new
// quorum that already contains the snapshot's data. We reuse etcdInitialCluster + etcdClusterToken so
// the restored cluster is byte-for-byte the same FQDN mesh the create recipe builds.

const (
	// etcdctlBinary is the etcd image's client binary. Like etcdBinary it must be named explicitly:
	// the image sets Cmd=["/usr/local/bin/etcd"] with NO ENTRYPOINT, so a bare `snapshot` arg would be
	// taken as the executable. The snapshot/restore/verify helpers all run this instead of `etcd`.
	etcdctlBinary = "/usr/local/bin/etcdctl"
	// etcdBackupMount is the in-container path the host snapshot dir is bind-mounted at. The snapshot
	// helper WRITES here (writable mount, the snapshot lands on the host); the restore helper READS
	// here (read-only) to unpack the snapshot into the member's data volume.
	etcdBackupMount = "/backup"
	// snapshotsSubdir is the host subdir (under the cluster state dir) snapshots are written to.
	snapshotsSubdir = "snapshots"
	// restoreMarkerKey is the etcd key the restore step reads back to PROVE the snapshot's data made it
	// into the restored cluster. /registry/namespaces/kube-system is always present in a live k3s
	// cluster (k3s stores every Kubernetes object under /registry), so its presence after a restore is
	// a sound "the data came back" signal without the snapshot step having to mutate anything.
	restoreMarkerKey = "/registry/namespaces/kube-system"
)

// snapshotsDir returns the host dir snapshots are written to: <clusterDir>/snapshots. Pure.
func snapshotsDir(clusterDir string) string {
	return filepath.Join(clusterDir, snapshotsSubdir)
}

// snapshotFileName builds a snapshot's file name: <cluster>-<UTC-timestamp>.db. The UTC timestamp
// (sortable, colon-free so it is a valid filename on any host) makes repeated snapshots unique and
// orderable. Pure so the construction is unit-testable (BVA: name shape, .db suffix) without a clock.
func snapshotFileName(clusterName string, t time.Time) string {
	return fmt.Sprintf("%s-%s.db", clusterName, t.UTC().Format("20060102T150405Z"))
}

// etcdClientURL / etcdPeerURL render a member's FQDN client / peer URL (https, mutual TLS). Shared by
// the snapshot endpoint, the restore --initial-advertise-peer-urls, and the verify endpoint so they
// all address the member by the SAME FQDN the create recipe uses (name-bound, IP-shift-survivable).
func etcdClientURL(memberFQDN string) string {
	return "https://" + net.JoinHostPort(memberFQDN, strconv.Itoa(etcdClientPort))
}

func etcdPeerURL(memberFQDN string) string {
	return "https://" + net.JoinHostPort(memberFQDN, strconv.Itoa(etcdPeerPort))
}

// etcdctlTLSFlags returns the etcdctl client mutual-TLS flags pointing at the bind-mounted client
// bundle (ca.crt/client.crt/client.key under etcdTLSMount). Shared by the snapshot and verify helpers
// (both connect to a live member as a client). Restore needs none — it is a local, offline unpack.
func etcdctlTLSFlags() []string {
	return []string{
		"--cacert", etcdTLSMount + "/" + etcdCACertFile,
		"--cert", etcdTLSMount + "/" + etcdClientCertFile,
		"--key", etcdTLSMount + "/" + etcdClientKeyFile,
	}
}

// etcdImageOf resolves the etcd member image for a helper container: the cluster's recorded
// DatastoreImage, else the pinned default. Mirrors buildEtcdRunArgs' own fallback so a snapshot /
// restore / verify helper runs the SAME etcd build as the members.
func etcdImageOf(cfg ClusterConfig) string {
	if cfg.DatastoreImage != "" {
		return cfg.DatastoreImage
	}

	return defaultEtcdImage
}

// buildEtcdSnapshotArgs assembles the `container run` vector for the snapshot helper: an ephemeral
// etcdctl container that bind-mounts the host snapshot dir (writable) and the client TLS bundle
// (read-only), connects to memberFQDN over mutual TLS, and streams `snapshot save` to the host. Pure
// (unit-testable without a VM), mirroring buildEtcdRunArgs. Foreground (no --detach): the helper is a
// one-shot, so p.run blocks until etcdctl exits and the snapshot is complete on the host.
func buildEtcdSnapshotArgs(cfg ClusterConfig, helperName, memberFQDN, hostSnapshotDir, hostClientTLSDir, snapshotFile string) []string {
	args := []string{
		"run",
		"--name", helperName,
		"--volume", hostSnapshotDir + ":" + etcdBackupMount, // writable: the snapshot lands on the host here
		"--volume", hostClientTLSDir + ":" + etcdTLSMount + ":ro",
		"--env", "ETCDCTL_API=3",
	}

	if cfg.Network != "" {
		args = append(args, "--network", cfg.Network)
	}

	args = append(args, etcdImageOf(cfg), etcdctlBinary,
		"--endpoints="+etcdClientURL(memberFQDN))
	args = append(args, etcdctlTLSFlags()...)
	args = append(args, "snapshot", "save", etcdBackupMount+"/"+snapshotFile)

	return args
}

// buildEtcdRestoreArgs assembles the `container run` vector for one member's restore helper: an
// ephemeral etcdctl container that mounts the member's FRESH data volume (writable) and the host
// snapshot dir (read-only) and runs `snapshot restore` into the member's data dir. Pure
// (unit-testable). Foreground one-shot. initialCluster is the SHARED etcdInitialCluster value
// (identical for every member); only --name and --initial-advertise-peer-urls differ per member.
//
// No TLS flags: restore is a LOCAL unpack of the snapshot into a data dir, not a client connection.
// --initial-cluster-token is etcdClusterToken (== the create recipe) so the restored quorum is the
// same FQDN-addressed cluster. The data dir is etcdDataDir (a subdir of the mount, dodging the ext4
// lost+found the same way buildEtcdRunArgs does).
func buildEtcdRestoreArgs(cfg ClusterConfig, helperName string, member NodeConfig, dnsDomain, initialCluster, hostSnapshotDir, snapshotFile string) []string {
	fqdn := nodeFQDN(member.Name, dnsDomain)

	args := []string{
		"run",
		"--name", helperName,
		"--volume", etcdVolumeName(member.Name) + ":" + etcdDataMount, // the FRESH data volume, writable
		"--volume", hostSnapshotDir + ":" + etcdBackupMount + ":ro", // the snapshot, read-only
		"--env", "ETCDCTL_API=3",
	}

	args = append(args, etcdImageOf(cfg), etcdctlBinary,
		"snapshot", "restore", etcdBackupMount+"/"+snapshotFile,
		"--name", member.Name, // each member restores under its OWN etcd member name
		"--initial-cluster", initialCluster, // IDENTICAL across members (the FQDN mesh)
		"--initial-advertise-peer-urls", etcdPeerURL(fqdn), // this member's OWN peer URL
		"--initial-cluster-token", etcdClusterToken(cfg.Name), // == create recipe
		"--data-dir", etcdDataDir,
	)

	return args
}

// buildEtcdVerifyArgs assembles the `container run` vector for the post-restore verification helper:
// an ephemeral etcdctl container that connects to memberFQDN over mutual TLS and `get`s the marker
// key. Pure. Foreground one-shot; p.run returns the key listing, which the caller checks is
// non-empty (the marker survived the restore).
func buildEtcdVerifyArgs(cfg ClusterConfig, helperName, memberFQDN, hostClientTLSDir, key string) []string {
	args := []string{
		"run",
		"--name", helperName,
		"--volume", hostClientTLSDir + ":" + etcdTLSMount + ":ro",
		"--env", "ETCDCTL_API=3",
	}

	if cfg.Network != "" {
		args = append(args, "--network", cfg.Network)
	}

	args = append(args, etcdImageOf(cfg), etcdctlBinary,
		"--endpoints="+etcdClientURL(memberFQDN))
	args = append(args, etcdctlTLSFlags()...)
	args = append(args, "get", key, "--keys-only")

	return args
}

// existingEtcdMemberTLSDir returns the absolute on-disk TLS dir for one member
// (<clusterDir>/etcd-tls/<member>), the same path writeEtcdTLS produced at create. Restore reuses
// the member's existing server cert (a restore does not rotate TLS), so the relaunched member mounts
// this dir exactly as Create did.
func existingEtcdMemberTLSDir(clusterDir, memberName string) string {
	return filepath.Join(clusterDir, etcdTLSSubdir, memberName)
}

// Snapshot saves an etcd snapshot of the managed datastore to a host path under the cluster state
// dir and returns that path. SAFE / read-only — no -force needed: it never mutates the cluster.
// Requires a managed etcd datastore (a bring-your-own endpoint or single-server sqlite has no
// k3ac-managed quorum to snapshot).
func (p *provisioner) Snapshot(ctx context.Context, stateDir, clusterName string, logw io.Writer) (string, error) {
	if logw == nil {
		logw = io.Discard
	}

	state, err := LoadState(stateDir, clusterName)
	if err != nil {
		return "", err
	}

	clusterDir, err := ensureClusterDir(stateDir, clusterName)
	if err != nil {
		return "", err
	}

	members := datastoreNodes(state.Nodes)
	if len(members) == 0 {
		return "", fmt.Errorf("cluster %q has no managed etcd datastore to snapshot "+
			"(bring-your-own-endpoint and single-server sqlite clusters are out of scope)", clusterName)
	}

	clientTLSDir := existingDatastoreTLSDir(clusterDir)
	if clientTLSDir == "" {
		return "", fmt.Errorf("cluster %q: no datastore client TLS bundle on disk (%s); cannot reach etcd to snapshot",
			clusterName, filepath.Join(clusterDir, etcdTLSSubdir, etcdClientSubdir))
	}

	snapDir := snapshotsDir(clusterDir)
	if err := os.MkdirAll(snapDir, 0o755); err != nil {
		return "", fmt.Errorf("creating snapshot dir %q: %w", snapDir, err)
	}

	file := snapshotFileName(clusterName, time.Now())
	helper := clusterName + "-etcd-snapshot"
	cfg := clusterConfigFromState(state, stateDir, clusterDir)

	// Clear any stale helper from a prior interrupted snapshot, run the save, then clean up the
	// one-shot. etcdctl writes <file>.part and renames to <file> on success, so the final path
	// existing means the snapshot is complete.
	_ = p.remove(ctx, helper)

	fmt.Fprintf(logw, "saving etcd snapshot from %s\n", members[0].ID)

	if _, err := p.run(ctx, buildEtcdSnapshotArgs(cfg, helper, members[0].ID, snapDir, clientTLSDir, file)...); err != nil {
		return "", fmt.Errorf("etcd snapshot save: %w", err)
	}

	_ = p.remove(ctx, helper)

	hostPath := filepath.Join(snapDir, file)
	if _, err := os.Stat(hostPath); err != nil {
		return "", fmt.Errorf("snapshot did not land on the host at %q: %w", hostPath, err)
	}

	fmt.Fprintf(logw, "etcd snapshot saved to %s\n", hostPath)

	return hostPath, nil
}

// Restore rebuilds the managed etcd datastore from a snapshot and brings the cluster back on the
// restored data. DESTRUCTIVE — needs -force: it stops every node and REPLACES every etcd member's
// data volume. Preserves the k3s server/agent state volumes (they are only stopped/started) and the
// datastore TLS (unchanged by a restore).
func (p *provisioner) Restore(ctx context.Context, stateDir, clusterName, snapshotPath string, force bool, logw io.Writer) error {
	if logw == nil {
		logw = io.Discard
	}

	state, err := LoadState(stateDir, clusterName)
	if err != nil {
		return err
	}

	clusterDir, err := ensureClusterDir(stateDir, clusterName)
	if err != nil {
		return err
	}

	members := datastoreNodes(state.Nodes)
	if len(members) == 0 {
		return fmt.Errorf("cluster %q has no managed etcd datastore to restore", clusterName)
	}

	snapAbs, err := filepath.Abs(snapshotPath)
	if err != nil {
		return fmt.Errorf("resolving snapshot path %q: %w", snapshotPath, err)
	}

	if info, err := os.Stat(snapAbs); err != nil || info.IsDir() {
		return fmt.Errorf("snapshot file %q is not a readable file", snapshotPath)
	}

	if err := ensureForced(force, logw, "-restore", []string{
		fmt.Sprintf("stop every node of cluster %q", clusterName),
		fmt.Sprintf("DELETE and rebuild all %d etcd data volumes from %s", len(members), snapAbs),
		"restart the servers and agents against the restored datastore",
	}); err != nil {
		return err
	}

	return p.runRestore(ctx, &state, clusterName, clusterDir, snapAbs, members, logw)
}

// runRestore is the orchestration body of Restore (extracted so Restore stays within the funlen /
// gocognit gates). It stops the cluster, rebuilds the etcd quorum from the snapshot, verifies the
// marker key, restarts the k3s nodes, and persists the refreshed state.
func (p *provisioner) runRestore(ctx context.Context, state *ClusterState, clusterName, clusterDir, snapAbs string, members []NodeInfo, logw io.Writer) error {
	cfg := clusterConfigFromState(*state, state.StateDir, clusterDir)
	snapHostDir, snapFile := filepath.Split(snapAbs)
	initialCluster := etcdInitialCluster(clusterName, p.dnsDomain, len(members))

	// 1) Stop every node in dependency-safe order so nothing writes to etcd while it is rebuilt.
	for _, node := range stopOrder(state.Nodes) {
		fmt.Fprintln(logw, "stopping node", node.Name)

		if err := p.stop(ctx, node.ID); err != nil {
			return fmt.Errorf("stopping node %q: %w", node.Name, err)
		}
	}

	// 2) Rebuild the etcd members from the snapshot and relaunch them.
	memberInfos, err := p.restoreEtcdMembers(ctx, cfg, members, clusterDir, initialCluster, snapHostDir, snapFile, logw)
	if err != nil {
		return err
	}

	for _, info := range memberInfos {
		replaceNodeInState(state, info)
	}

	// 3) Prove the snapshot's data is actually in the restored quorum before restarting k3s.
	if err := p.verifyRestoreMarker(ctx, cfg, memberInfos[0].ID, clusterDir, logw); err != nil {
		return err
	}

	// 4) Restart the servers/agents/LB against the restored datastore.
	if err := p.startNonDatastore(ctx, state, logw); err != nil {
		return err
	}

	fmt.Fprintf(logw, "restored cluster %q from %s\n", clusterName, snapAbs)

	return saveState(*state)
}

// restoreEtcdMembers replaces each etcd member's data volume with a FRESH one, restores the snapshot
// into it, then relaunches the member from the restored volume. It returns the relaunched members'
// NodeInfo (with their new DHCP IPs). Two passes: restore-into-fresh-volume for ALL members first,
// then launch ALL members, because etcd forms its quorum only once every peer in --initial-cluster
// is running (same ordering rule as provisionEtcdCluster).
func (p *provisioner) restoreEtcdMembers(ctx context.Context, cfg ClusterConfig, members []NodeInfo, clusterDir, initialCluster, snapHostDir, snapFile string, logw io.Writer) ([]NodeInfo, error) {
	for _, m := range members {
		memberCfg := nodeConfigFromInfo(m)
		helper := m.Name + "-restore"

		fmt.Fprintln(logw, "restoring etcd member", m.Name, "from snapshot")

		// Remove the old member container, then DELETE + recreate its data volume so the restore
		// unpacks into an empty dir (etcdctl snapshot restore refuses a non-empty data dir). The
		// volume keeps the SAME name, so buildEtcdRunArgs still finds it.
		if err := p.remove(ctx, m.ID); err != nil {
			return nil, fmt.Errorf("removing etcd member %q: %w", m.Name, err)
		}

		if err := p.volumeDelete(ctx, etcdVolumeName(m.Name)); err != nil {
			return nil, fmt.Errorf("deleting etcd volume for %q: %w", m.Name, err)
		}

		if err := p.volumeCreate(ctx, etcdVolumeName(m.Name), volumeLabels(cfg.Name)...); err != nil {
			return nil, fmt.Errorf("recreating etcd volume for %q: %w", m.Name, err)
		}

		_ = p.remove(ctx, helper)

		if _, err := p.run(ctx, buildEtcdRestoreArgs(cfg, helper, memberCfg, p.dnsDomain, initialCluster, snapHostDir, snapFile)...); err != nil {
			return nil, fmt.Errorf("etcd snapshot restore for member %q: %w", m.Name, err)
		}

		_ = p.remove(ctx, helper)
	}

	return p.launchRestoredMembers(ctx, cfg, members, clusterDir, initialCluster, logw)
}

// launchRestoredMembers relaunches every etcd member from its restored data volume and waits for the
// quorum's client ports to come up. Each member reuses its existing on-disk server cert (restore does
// not rotate TLS) and boots from etcdDataDir, which now holds the restored data.
func (p *provisioner) launchRestoredMembers(ctx context.Context, cfg ClusterConfig, members []NodeInfo, clusterDir, initialCluster string, logw io.Writer) ([]NodeInfo, error) {
	infos := make([]NodeInfo, 0, len(members))

	for _, m := range members {
		memberCfg := nodeConfigFromInfo(m)
		tlsDir := existingEtcdMemberTLSDir(clusterDir, m.Name)

		fmt.Fprintln(logw, "relaunching etcd member", m.ID, "on restored data")

		if _, err := p.run(ctx, buildEtcdRunArgs(cfg, memberCfg, p.dnsDomain, initialCluster, tlsDir)...); err != nil {
			return nil, fmt.Errorf("relaunching etcd member %q: %w", m.ID, err)
		}

		addr, err := p.waitForIPv4(ctx, m.ID)
		if err != nil {
			return nil, err
		}

		infos = append(infos, NodeInfo{ID: m.ID, Name: m.Name, Role: RoleDatastore, IPs: []netip.Addr{addr}, Memory: m.Memory})
	}

	for _, info := range infos {
		if err := p.waitForEtcdMember(ctx, info.IPs[0]); err != nil {
			return nil, fmt.Errorf("restored etcd member %q readiness: %w", info.ID, err)
		}
	}

	return infos, nil
}

// verifyRestoreMarker reads the marker key back from the restored quorum and fails if it is absent —
// the restore did not actually recover the snapshot's data. Runs an ephemeral etcdctl `get` over the
// existing client TLS against the first restored member.
func (p *provisioner) verifyRestoreMarker(ctx context.Context, cfg ClusterConfig, memberFQDN, clusterDir string, logw io.Writer) error {
	clientTLSDir := existingDatastoreTLSDir(clusterDir)
	helper := cfg.Name + "-etcd-verify"

	_ = p.remove(ctx, helper)

	fmt.Fprintf(logw, "verifying restored datastore has the marker key %s\n", restoreMarkerKey)

	out, err := p.run(ctx, buildEtcdVerifyArgs(cfg, helper, memberFQDN, clientTLSDir, restoreMarkerKey)...)

	_ = p.remove(ctx, helper)

	if err != nil {
		return fmt.Errorf("verifying restored datastore: %w", err)
	}

	if out == "" {
		return fmt.Errorf("restore verification failed: marker key %q absent from the restored datastore "+
			"(the snapshot data did not come back)", restoreMarkerKey)
	}

	return nil
}

// startNonDatastore starts every non-datastore node (servers, then LB, then agents — start order) and
// re-arms ip_forward on the k3s nodes, exactly as lifecycle.Start does. The etcd members are already
// running (relaunched fresh by the restore), so they are skipped here.
func (p *provisioner) startNonDatastore(ctx context.Context, state *ClusterState, logw io.Writer) error {
	for _, node := range startOrder(state.Nodes) {
		if node.Role == RoleDatastore {
			continue
		}

		fmt.Fprintln(logw, "starting node", node.Name)

		if err := p.start(ctx, node.ID); err != nil {
			return fmt.Errorf("starting node %q: %w", node.Name, err)
		}

		if node.Role != RoleServer && node.Role != RoleAgent {
			continue // LB is not a k3s node: no ip_forward
		}

		addr, err := p.waitForIPv4(ctx, node.ID)
		if err != nil {
			return fmt.Errorf("starting node %q: %w", node.Name, err)
		}

		if err := p.enableIPForward(ctx, node.ID); err != nil {
			return fmt.Errorf("starting node %q: %w", node.Name, err)
		}

		replaceNodeInState(state, NodeInfo{
			ID: node.ID, Name: node.Name, Role: node.Role, IPs: []netip.Addr{addr},
			Memory: node.Memory, NanoCPUs: node.NanoCPUs, Labels: node.Labels, ExtraArgs: node.ExtraArgs,
		})
	}

	return nil
}
