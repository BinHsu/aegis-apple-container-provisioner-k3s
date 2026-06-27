// SPDX-License-Identifier: MIT

package apple

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
// the units the create path passes.
func (p *provisioner) AddAgents(ctx context.Context, stateDir, clusterName string, count int, agentMemBytes, agentNanoCPUs int64, logw io.Writer) (ClusterState, error) {
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
			Name:     fmt.Sprintf("%s-agent-%d", clusterName, idx+i),
			Role:     RoleAgent,
			Memory:   agentMemBytes,
			NanoCPUs: agentNanoCPUs,
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
	prefix := clusterName + "-agent-"

	highest := 0

	for _, n := range nodes {
		suffix, ok := strings.CutPrefix(n.Name, prefix)
		if !ok {
			continue
		}

		idx, err := strconv.Atoi(suffix)
		if err != nil {
			continue // non-numeric suffix: not one of our generated agent names
		}

		if idx > highest {
			highest = idx
		}
	}

	return highest + 1
}

// ensureRemovable is the server-removal guard, extracted from RemoveNode so the decision
// is unit-testable without any container calls. A server node may not be removed: it owns
// the datastore and API, so removing it destroys the cluster — that is the -destroy path.
func ensureRemovable(node NodeInfo) error {
	if node.Role == RoleServer {
		return fmt.Errorf("node %q is the cluster server; removing it would destroy the cluster — use -destroy instead", node.Name)
	}

	return nil
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
