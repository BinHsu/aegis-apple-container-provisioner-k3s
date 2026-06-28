// SPDX-License-Identifier: MIT

package apple

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// node_ops.go implements post-create membership changes — adding agent nodes and
// removing a node — on an already-running cluster. Create owns first bring-up; these
// two operations reuse its building blocks (launchNode, enableIPForward,
// prepareNodeVolumes, assertDistinctIPs) and Destroy's per-node teardown (stop /
// remove / volumeDelete) so the add/remove paths can never drift from create/destroy.

// AddAgents launches `count` new agent nodes and joins them to an existing cluster.
//
// Agents auto-join: buildRunArgs bakes K3S_URL (the saved server endpoint) and
// K3S_TOKEN into each agent's `container run`, so there is no separate join step — the
// k3s agent process contacts the server on boot. The flow mirrors Create's agent loop:
//
//	LoadState -> nextAgentIndex -> prepareNodeVolumes (stale-state guard)
//	  -> for each agent: launchNode(serverURL) -> enableIPForward -> append
//	  -> assertDistinctIPs -> saveState
//
// agentMemBytes is in bytes and agentNanoCPUs in nano-CPUs, matching NodeConfig and
// the units the create path passes. labels (-node-label) and extraArgs (-k3s-agent-arg) are
// threaded onto each new agent (v0.5.0) so a post-create agent is configured exactly like a
// create-time one; both were create-only before. Empty slices = none.
func (p *provisioner) AddAgents(ctx context.Context, stateDir, clusterName string, count int, agentMemBytes, agentNanoCPUs int64, labels, extraArgs []string, logw io.Writer) (ClusterState, error) {
	if logw == nil {
		logw = io.Discard
	}

	// You cannot add to a cluster that was never created. A missing state.json is the
	// authoritative "no such cluster" signal (Destroy tolerates it for orphan cleanup,
	// but adding agents has nothing to join), so surface a clear error.
	state, err := LoadState(stateDir, clusterName)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ClusterState{}, fmt.Errorf("cluster %q not found; create it first", clusterName)
		}

		return ClusterState{}, err
	}

	// Reuse the exact image the cluster was created with so new agents match the
	// existing nodes. Pre-v0.2.0 states predate ClusterState.Image and unmarshal to "";
	// fall back to the pinned default in that case.
	image := state.Image
	if image == "" {
		image = defaultK3sImage
	}

	// Synthesize the ClusterConfig launchNode/buildRunArgs need from the saved state.
	// Nodes is left empty — AddAgents drives the per-node loop itself.
	cfg := ClusterConfig{
		Name:     clusterName,
		Image:    image,
		Network:  state.Network,
		StateDir: stateDir,
		Token:    state.Token,
	}

	// Number the new agents after the highest existing <cluster>-agent-<N>, so re-running
	// AddAgents never collides with current nodes or with names freed by a prior remove.
	idx := nextAgentIndex(state.Nodes, clusterName)

	agents := make([]NodeConfig, count)
	for i := range agents {
		agents[i] = NodeConfig{
			Name:      fmt.Sprintf("%s-agent-%d", clusterName, idx+i),
			Role:      RoleAgent,
			Memory:    agentMemBytes,
			NanoCPUs:  agentNanoCPUs,
			Labels:    labels,
			ExtraArgs: extraArgs,
		}
	}

	// Create each new agent's datastore named volume (stamped with the cluster labels)
	// and refuse to launch onto stale state — the same guard Create uses. Reusing
	// prepareNodeVolumes keeps the stale-state refusal identical to the create path.
	createVolume := func(ctx context.Context, name string) error {
		return p.volumeCreate(ctx, name, volumeLabels(clusterName)...)
	}

	if err := prepareNodeVolumes(ctx, clusterName, agents, p.volumeExists, createVolume); err != nil {
		return ClusterState{}, err
	}

	for _, agent := range agents {
		fmt.Fprintln(logw, "adding k3s agent", agent.Name, "->", state.ServerURL)

		info, err := p.launchNode(ctx, cfg, agent, state.ServerURL, "")
		if err != nil {
			return ClusterState{}, err
		}

		// ip_forward is mandatory for k3s pod networking; Create sets it per node and so
		// must we (there is no systemd to do it).
		if err := p.enableIPForward(ctx, info.ID); err != nil {
			return ClusterState{}, fmt.Errorf("agent %q: %w", agent.Name, err)
		}

		state.Nodes = append(state.Nodes, info)
	}

	// Same everyday-correctness guard as Create: every node must get a distinct vmnet IP.
	if err := assertDistinctIPs(state.Nodes); err != nil {
		return ClusterState{}, err
	}

	if err := saveState(state); err != nil {
		return ClusterState{}, err
	}

	return state, nil
}

// AddServer launches ANOTHER control-plane server, joins it to the cluster's existing external
// datastore, and re-points the API load balancer at the larger server set (v0.5.0). It mirrors
// AddAgents' structure, with two HA-specific extras: the new server runs stateless against
// state.DatastoreEndpoint (no --cluster-init, like Create's HA join servers), and the LB's static
// backend list is regenerated + reloaded so traffic actually reaches the new server.
//
//	LoadState -> ensureAddServerable (HA + LB guard) -> nextServerIndex
//	  -> prepareNodeVolumes -> launchNode(join) -> enableIPForward -> waitForAPIServer
//	  -> append to state -> refreshAPILB (regenerate haproxy.cfg + restart LB) -> saveState
//
// serverMemBytes is in bytes, serverNanoCPUs in nano-CPUs (matching NodeConfig); labels
// (-node-label) and extraArgs (-k3s-server-arg) configure the new server like a create-time one.
func (p *provisioner) AddServer(ctx context.Context, stateDir, clusterName string, serverMemBytes, serverNanoCPUs int64, labels, extraArgs []string, logw io.Writer) (ClusterState, error) {
	if logw == nil {
		logw = io.Discard
	}

	state, err := LoadState(stateDir, clusterName)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ClusterState{}, fmt.Errorf("cluster %q not found; create it first", clusterName)
		}

		return ClusterState{}, err
	}

	// Guard (pure, unit-testable): -add-server needs an external datastore to join AND an existing
	// API LB to update. Refuse otherwise — there is nothing coherent to add a server to.
	if err := ensureAddServerable(state); err != nil {
		return ClusterState{}, err
	}

	image := state.Image
	if image == "" {
		image = defaultK3sImage
	}

	clusterDir, err := ensureClusterDir(stateDir, clusterName)
	if err != nil {
		return ClusterState{}, err
	}

	// Synthesize the join server's ClusterConfig from saved state. DatastoreTLSDir is re-pointed at
	// the client bundle Create wrote on disk (managed-etcd-TLS clusters), so the new server reaches
	// etcd with the SAME client cert the existing servers use; a bring-your-own cluster gets "".
	cfg := ClusterConfig{
		Name:              clusterName,
		Image:             image,
		Network:           state.Network,
		StateDir:          stateDir,
		Token:             state.Token,
		DatastoreEndpoint: state.DatastoreEndpoint,
		DatastoreTLSDir:   existingDatastoreTLSDir(clusterDir),
	}

	idx := nextServerIndex(state.Nodes, clusterName)
	server := NodeConfig{
		Name:      fmt.Sprintf("%s-server-%d", clusterName, idx),
		Role:      RoleServer,
		Memory:    serverMemBytes,
		NanoCPUs:  serverNanoCPUs,
		Labels:    labels,
		ExtraArgs: extraArgs,
	}

	createVolume := func(ctx context.Context, name string) error {
		return p.volumeCreate(ctx, name, volumeLabels(clusterName)...)
	}

	if err := prepareNodeVolumes(ctx, clusterName, []NodeConfig{server}, p.volumeExists, createVolume); err != nil {
		return ClusterState{}, err
	}

	fmt.Fprintln(logw, "adding k3s server", server.Name, "(HA, shared datastore)")

	// Join server: no kubeconfig mount (the bootstrap server already delivered it), so readiness is
	// the host-side TLS dial only — same as Create's launchJoinServers.
	info, err := p.launchNode(ctx, cfg, server, "", "")
	if err != nil {
		return ClusterState{}, err
	}

	if err := p.enableIPForward(ctx, info.ID); err != nil {
		return ClusterState{}, fmt.Errorf("server %q: %w", server.Name, err)
	}

	if err := p.waitForAPIServer(ctx, info.IPs[0]); err != nil {
		return ClusterState{}, fmt.Errorf("server %q readiness: %w", server.Name, err)
	}

	state.Nodes = append(state.Nodes, info)

	// Re-point the API LB at the now-larger server set. ensureAddServerable guaranteed an LB exists.
	if err := p.refreshAPILB(ctx, clusterDir, &state, logw); err != nil {
		return ClusterState{}, err
	}

	if err := assertDistinctIPs(state.Nodes); err != nil {
		return ClusterState{}, err
	}

	if err := saveState(state); err != nil {
		return ClusterState{}, err
	}

	return state, nil
}

// refreshAPILB regenerates the haproxy.cfg from the cluster's CURRENT server set (read from state)
// and restarts the LB container so haproxy reloads it. The backend list is static (one `server`
// line per FQDN), so a new server is invisible to the LB until the config is regenerated — this is
// the load-bearing half of AddServer. A cold restart shifts the LB's DHCP IP; clusterAPIFQDN
// re-registers, so the kubeconfig endpoint is unaffected. The restarted LB's new IP is written back
// into state. Mutates state in place (updates the LB node's recorded IP).
func (p *provisioner) refreshAPILB(ctx context.Context, clusterDir string, state *ClusterState, logw io.Writer) error {
	lbIdx := -1

	for i, n := range state.Nodes {
		if n.Role == RoleLB {
			lbIdx = i

			break
		}
	}

	if lbIdx < 0 {
		return fmt.Errorf("cluster %q: no API load balancer node to update", state.ClusterName)
	}

	servers := serverConfigsFromState(state.Nodes)

	fmt.Fprintf(logw, "regenerating API load balancer config for %d servers\n", len(servers))

	if _, err := writeAPILBConfig(clusterDir, buildAPILBConfig(state.ClusterName, servers, p.dnsDomain)); err != nil {
		return err
	}

	lb := state.Nodes[lbIdx]

	fmt.Fprintln(logw, "restarting API load balancer", lb.ID, "to reload config")

	if err := p.stop(ctx, lb.ID); err != nil {
		return err
	}

	if err := p.start(ctx, lb.ID); err != nil {
		return err
	}

	addr, err := p.waitForIPv4(ctx, lb.ID)
	if err != nil {
		return err
	}

	// A completed TLS handshake through the LB proves it forwards to a live server end to end (mode
	// tcp passthrough), i.e. the reloaded backend list works.
	if err := p.waitForAPIServer(ctx, addr); err != nil {
		return fmt.Errorf("API load balancer %q readiness after reload: %w", lb.ID, err)
	}

	state.Nodes[lbIdx].IPs = []netip.Addr{addr}

	return nil
}

// RemoveNode tears down a single node and removes it from the cluster's saved state.
//
//	LoadState -> find node -> server guard -> best-effort kubectl drain
//	  -> stop + remove container -> delete datastore volume -> drop from state -> saveState
//
// The container/volume teardown reuses Destroy's per-node helpers (stop / remove /
// volumeDelete), so a removed node is cleaned up exactly as Destroy would clean it.
func (p *provisioner) RemoveNode(ctx context.Context, stateDir, clusterName, nodeName string, logw io.Writer) error {
	if logw == nil {
		logw = io.Discard
	}

	state, err := LoadState(stateDir, clusterName)
	if err != nil {
		return err
	}

	idx := -1

	for i, n := range state.Nodes {
		if n.Name == nodeName {
			idx = i

			break
		}
	}

	if idx < 0 {
		return fmt.Errorf("node %q not found in cluster %q; available nodes: %s",
			nodeName, clusterName, strings.Join(nodeNames(state.Nodes), ", "))
	}

	node := state.Nodes[idx]

	// GUARD (load-bearing): the single server owns the datastore and the API — removing it
	// destroys the cluster, which is a -destroy operation, not a node remove. Refuse and
	// point the operator at the right tool. Extracted as a pure helper so the decision is
	// unit-testable without any container calls.
	if err := ensureRemovable(node); err != nil {
		return err
	}

	// Best-effort drain from Kubernetes BEFORE the container is gone, so the node does not
	// linger as NotReady. Non-fatal by design (see drainNode).
	p.drainNode(ctx, stateDir, clusterName, nodeName, logw)

	fmt.Fprintln(logw, "removing k3s node", node.Name)

	// Stop + remove the container (by node.ID, the FQDN when dns-domain was set at create
	// time) and delete its datastore named volume — the same idempotent helpers Destroy's
	// recorded-node pass uses, so removal can never target a different resource than create
	// provisioned.
	if err := p.stop(ctx, node.ID); err != nil {
		return err
	}

	if err := p.remove(ctx, node.ID); err != nil {
		return err
	}

	if err := p.volumeDelete(ctx, nodeVolumeName(clusterName, node.Name)); err != nil {
		return err
	}

	// Drop the node from recorded state and persist.
	state.Nodes = append(state.Nodes[:idx], state.Nodes[idx+1:]...)

	return saveState(state)
}

// drainNode best-effort removes nodeName from Kubernetes via `kubectl delete node` so it
// does not linger as a NotReady object after its VM is destroyed. It is intentionally
// non-fatal: kubectl may be absent from PATH or the API server may be unreachable; in
// either case we log a warning and let the container/volume teardown proceed (the node
// object is cosmetic once the backing VM is gone). Uses the kubeconfig Create wrote to
// <stateDir>/<clusterName>/kubeconfig (server URL already rewritten off loopback).
func (p *provisioner) drainNode(ctx context.Context, stateDir, clusterName, nodeName string, logw io.Writer) {
	kubeconfig := filepath.Join(stateDir, clusterName, "kubeconfig")

	cmd := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig, "delete", "node", nodeName)

	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(logw, "warning: kubectl delete node %q failed (continuing teardown): %v: %s\n",
			nodeName, err, strings.TrimSpace(string(out)))
	}
}

// nextAgentIndex returns the index to assign the next agent: max existing
// <clusterName>-agent-<N> suffix + 1, or 1 when there are no agents. Names that do not
// match the <clusterName>-agent-<N> pattern (the server, or a non-numeric suffix) are
// ignored. Pure so the numbering is unit-testable (BVA on the agent set) without the
// container CLI.
//
// Using max+1 (not count+1) means a gap left by a removed agent is NOT backfilled — new
// agents always get a fresh, unused index, so a recreated agent never reuses a name whose
// stale datastore volume might still linger.
func nextAgentIndex(nodes []NodeInfo, clusterName string) int {
	return nextIndexForPrefix(nodes, clusterName+"-agent-")
}

// nextServerIndex is the server equivalent of nextAgentIndex: max existing
// <clusterName>-server-<N> + 1 (or 1 when none). Drives AddServer's new-server naming so a
// re-added server never collides with a current one or reuses a name whose stale k3s datastore
// volume might linger. Pure (BVA on the server set), like nextAgentIndex.
func nextServerIndex(nodes []NodeInfo, clusterName string) int {
	return nextIndexForPrefix(nodes, clusterName+"-server-")
}

// nextIndexForPrefix returns max existing <prefix><N> suffix + 1 (or 1 when none match). Names
// that do not carry the prefix, or whose suffix is non-numeric, are ignored. The shared core of
// nextAgentIndex / nextServerIndex so the "fresh index, never backfill a gap" rule is identical
// for both roles.
func nextIndexForPrefix(nodes []NodeInfo, prefix string) int {
	highest := 0

	for _, n := range nodes {
		suffix, ok := strings.CutPrefix(n.Name, prefix)
		if !ok {
			continue
		}

		idx, err := strconv.Atoi(suffix)
		if err != nil {
			continue // non-numeric suffix: not one of our generated node names
		}

		if idx > highest {
			highest = idx
		}
	}

	return highest + 1
}

// ensureRemovable is the node-removal guard, extracted from RemoveNode so the decision is
// unit-testable without any container calls. Three roles may not be removed via -remove-node:
//   - a server owns the API (and, in single-server mode, the datastore);
//   - the managed datastore backs the whole control plane;
//   - the API load balancer is the cluster's single API endpoint (the kubeconfig + agents
//     target its FQDN), so removing it cuts every client off from the control plane.
//
// Removing any of them is a cluster-destroying act, so it belongs to -destroy. Agents are the
// only removable role. (Removing one of several HA servers is conservatively refused too: there
// is no per-server drain/rebalance path yet — docs/ADR/0002.)
func ensureRemovable(node NodeInfo) error {
	switch node.Role {
	case RoleServer:
		return fmt.Errorf("node %q is a cluster server; removing it would break the control plane — use -destroy instead", node.Name)
	case RoleDatastore:
		return fmt.Errorf("node %q is the cluster datastore; removing it would destroy the cluster — use -destroy instead", node.Name)
	case RoleLB:
		return fmt.Errorf("node %q is the cluster API load balancer; removing it would cut every client off from the API — use -destroy instead", node.Name)
	}

	return nil
}

// ensureAddServerable guards -add-server (pure, unit-testable without container calls). It requires
// (1) an external datastore endpoint — a single-server sqlite cluster has no shared store for a
// second server to join — and (2) an existing API load balancer node to re-point at the larger
// server set. Both are "the cluster has no HA/LB context"; without them -add-server has nothing
// coherent to do. An IP-only multi-server cluster (no DNS domain, so setupAPILB skipped the LB)
// hits guard (2): recreate it with -dns-domain to get an LB.
func ensureAddServerable(state ClusterState) error {
	if state.DatastoreEndpoint == "" {
		return fmt.Errorf("cluster %q is single-server (no external datastore); -add-server needs an HA "+
			"cluster — recreate with -servers>=2 (or a bring-your-own -datastore-endpoint)", state.ClusterName)
	}

	if !hasLBNode(state.Nodes) {
		return fmt.Errorf("cluster %q has no API load balancer to update; -add-server requires an HA "+
			"cluster created with a DNS domain (the LB is FQDN-addressed)", state.ClusterName)
	}

	return nil
}

// hasLBNode reports whether the recorded nodes include the API load balancer (RoleLB).
func hasLBNode(nodes []NodeInfo) bool {
	for _, n := range nodes {
		if n.Role == RoleLB {
			return true
		}
	}

	return false
}

// serverConfigsFromState extracts the RoleServer nodes from recorded state as NodeConfigs (Name +
// Role — all buildAPILBConfig needs), preserving order. The single source of truth for the LB
// backend set on AddServer, so the regenerated haproxy.cfg lists exactly the servers in state.
func serverConfigsFromState(nodes []NodeInfo) []NodeConfig {
	var servers []NodeConfig

	for _, n := range nodes {
		if n.Role == RoleServer {
			servers = append(servers, NodeConfig{Name: n.Name, Role: RoleServer})
		}
	}

	return servers
}

// existingDatastoreTLSDir returns the absolute client-TLS dir Create wrote for a managed-etcd
// cluster (<clusterDir>/etcd-tls/client) when it exists on disk, else "". AddServer threads it so a
// new server reaches the TLS datastore with the SAME client bundle the original servers use; a
// bring-your-own datastore cluster has no such dir and gets "" (plain connection).
func existingDatastoreTLSDir(clusterDir string) string {
	dir := filepath.Join(clusterDir, etcdTLSSubdir, etcdClientSubdir)
	if info, err := os.Stat(dir); err == nil && info.IsDir() {
		return dir
	}

	return ""
}

// nodeNames returns the bare names of nodes, for the "available nodes" hint in
// RemoveNode's not-found error.
func nodeNames(nodes []NodeInfo) []string {
	names := make([]string, len(nodes))
	for i, n := range nodes {
		names[i] = n.Name
	}

	return names
}
