// SPDX-License-Identifier: MIT

package apple

import (
	"context"
	"fmt"
	"io"
	"sort"
)

// lifecycle.go implements whole-cluster start/stop (v0.4.0). Unlike Create/Destroy it does
// not provision or reclaim anything — it stops or starts the nodes a cluster already has,
// in a dependency-safe order read from the cluster's saved state.json.
//
// Ordering is the load-bearing part and is computed by two PURE functions (stopOrder /
// startOrder) so it is unit-testable without the `container` CLI:
//
//   - STOP order:  agents first, then servers, then the datastore/LB last. Workers drain
//     before the control plane; the datastore outlives every server that writes to it.
//   - START order: the exact reverse — datastore first (the servers need it to be accepting
//     connections), then servers, then agents.
//
// The vmnet substrate forces a per-node re-arm on start: a cold `container start` boots each
// node onto a NEW DHCP IP, and there is no systemd to set net.ipv4.ip_forward=1, so Start
// re-arms it per k3s node exactly as Create does (the datastore VM is not a k3s node and is
// skipped). See docs/ADR/0002 for the IP-shift behaviour and the one-id-per-start constraint.

// roleStopRank ranks a node's role for STOP ordering: lower stops first. Agents (workers)
// drain before servers; the datastore stops last so it outlives every server still writing
// to it. startOrder is the reverse of this ranking.
func roleStopRank(r Role) int {
	switch r {
	case RoleAgent:
		return 0
	case RoleServer:
		return 1
	case RoleDatastore:
		return 2
	default:
		return 3
	}
}

// orderByRole returns nodes sorted by roleStopRank; reversed flips it (start order). A stable
// sort preserves the recorded order within a role (server-1 before server-2). Pure helper
// shared by stopOrder/startOrder.
func orderByRole(nodes []NodeInfo, reversed bool) []NodeInfo {
	ordered := make([]NodeInfo, len(nodes))
	copy(ordered, nodes)

	sort.SliceStable(ordered, func(i, j int) bool {
		ri, rj := roleStopRank(ordered[i].Role), roleStopRank(ordered[j].Role)
		if reversed {
			return ri > rj
		}

		return ri < rj
	})

	return ordered
}

// stopOrder returns the cluster's nodes in safe STOP order: agents, then servers, then the
// datastore last. Pure so the ordering is unit-testable in isolation.
func stopOrder(nodes []NodeInfo) []NodeInfo { return orderByRole(nodes, false) }

// startOrder returns the cluster's nodes in safe START order: datastore first, then servers,
// then agents — the exact reverse of stopOrder. Pure.
func startOrder(nodes []NodeInfo) []NodeInfo { return orderByRole(nodes, true) }

// Stop halts every node of a cluster in dependency-safe order (agents -> servers ->
// datastore). It reads the node list from the cluster's saved state.json. Each `container
// stop` is issued one node at a time so the ordering is explicit and a single stuck node is
// easy to see in the log. stop() ignores "not found", so Stop is idempotent against a cluster
// that is already partly down.
func (p *provisioner) Stop(ctx context.Context, stateDir, clusterName string, logw io.Writer) error {
	if logw == nil {
		logw = io.Discard
	}

	state, err := LoadState(stateDir, clusterName)
	if err != nil {
		return err
	}

	for _, node := range stopOrder(state.Nodes) {
		fmt.Fprintln(logw, "stopping node", node.Name)

		if err := p.stop(ctx, node.ID); err != nil {
			return fmt.Errorf("stopping node %q: %w", node.Name, err)
		}
	}

	return nil
}

// Start boots every node of a cluster in dependency-safe order (datastore -> servers ->
// agents), reading the node list from saved state.json. `container start` takes ONE id per
// call (docs/ADR/0002), so nodes are started one at a time. After each k3s node comes up,
// Start waits for its new DHCP IPv4 and re-arms net.ipv4.ip_forward=1 — mandatory for k3s pod
// networking and lost across a cold stop/start, exactly as Create sets it. The datastore VM is
// not a k3s node, so it is started but not re-armed.
//
// NOTE: the datastore's --datastore-endpoint and every node's API endpoint are FQDNs that
// `container` DNS re-registers to the new IP on restart, which is what lets the control plane
// reconverge after the whole-cluster IP shift (docs/ADR/0002, VERIFICATION G9). Start relies on
// that — it does not rewrite any endpoint.
func (p *provisioner) Start(ctx context.Context, stateDir, clusterName string, logw io.Writer) error {
	if logw == nil {
		logw = io.Discard
	}

	state, err := LoadState(stateDir, clusterName)
	if err != nil {
		return err
	}

	for _, node := range startOrder(state.Nodes) {
		fmt.Fprintln(logw, "starting node", node.Name)

		if err := p.start(ctx, node.ID); err != nil {
			return fmt.Errorf("starting node %q: %w", node.Name, err)
		}

		// The datastore is not a k3s node — no ip_forward to re-arm.
		if node.Role == RoleDatastore {
			continue
		}

		// Post-boot window: wait for the node's NEW DHCP IPv4, then re-arm ip_forward.
		// waitForIPv4 polls `container inspect` until vmnet assigns an address, so it doubles
		// as the "node is back up" gate before the sysctl exec.
		if _, err := p.waitForIPv4(ctx, node.ID); err != nil {
			return fmt.Errorf("starting node %q: %w", node.Name, err)
		}

		if err := p.enableIPForward(ctx, node.ID); err != nil {
			return fmt.Errorf("starting node %q: %w", node.Name, err)
		}
	}

	return nil
}
