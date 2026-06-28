// SPDX-License-Identifier: MIT

package apple

import (
	"context"
	"errors"
	"fmt"
	"io"
)

// day2.go holds the shared scaffolding for the v0.6.0 "day-2" operations (snapshot/restore,
// upgrade/rollback, cert/token rotation). Each of those lives in its own file; the pieces they
// ALL share — the destructive-action -force guard, the role filters over recorded state, the
// ClusterConfig/NodeConfig reconstruction from saved state, and the recreate-a-k3s-node primitive
// — sit here so the three feature files stay focused and cannot drift apart.

// errForceRequired is returned by a destructive op invoked without -force. The op has already
// printed exactly what it WOULD do (ensureForced); this is the refusal the cmd driver surfaces as
// a non-zero exit. A sentinel so a test can assert the guard fired without string-matching.
var errForceRequired = errors.New("refused: this is a destructive operation — re-run with -force to proceed")

// ensureForced is the single guard every destructive verb (-restore, -upgrade, -rollback,
// -rotate-certs, -rotate-token) calls before touching a running cluster. When force is false it
// prints the op name plus the exact list of actions it WOULD take and returns errForceRequired —
// so a bare invocation can never mutate or destroy a cluster, only describe what it would do.
// When force is true it returns nil and the caller proceeds.
//
// Pure enough to unit-test as a boundary (CLAUDE.md k): force=false -> errForceRequired (refuse),
// force=true -> nil (proceed). The plan text is informational; the boolean is the contract.
func ensureForced(force bool, logw io.Writer, op string, plan []string) error {
	if force {
		return nil
	}

	fmt.Fprintf(logw, "%s is a DESTRUCTIVE operation and would perform:\n", op)

	for _, line := range plan {
		fmt.Fprintf(logw, "  - %s\n", line)
	}

	fmt.Fprintln(logw, "re-run with -force to proceed.")

	return errForceRequired
}

// nodesWithRole returns the recorded nodes carrying role, preserving order. Pure filter shared by
// every day-2 op (datastore members for restore/rotate-certs, servers for the rolling ops, etc.).
func nodesWithRole(nodes []NodeInfo, role Role) []NodeInfo {
	var out []NodeInfo

	for _, n := range nodes {
		if n.Role == role {
			out = append(out, n)
		}
	}

	return out
}

// datastoreNodes / serverNodes / agentNodes are the role filters the day-2 orchestration reads off
// recorded state. Thin wrappers over nodesWithRole so call sites read intent-first.
func datastoreNodes(nodes []NodeInfo) []NodeInfo { return nodesWithRole(nodes, RoleDatastore) }
func serverNodes(nodes []NodeInfo) []NodeInfo    { return nodesWithRole(nodes, RoleServer) }
func agentNodes(nodes []NodeInfo) []NodeInfo     { return nodesWithRole(nodes, RoleAgent) }

// k3sNodesInUpgradeOrder returns the cluster's K3S nodes (servers then agents) in the order a
// rolling replace must visit them: every server first, then every agent, each in recorded order.
// The etcd datastore and the API LB are NOT k3s nodes and are EXCLUDED — a rolling k3s image
// change never recreates them. Pure so the ordering is unit-testable (BVA, CLAUDE.md k): servers
// before agents, datastore/LB absent.
//
// Servers go first because they are stateless against the external datastore: a recreated server
// just reconnects, so the control plane is continuously available as long as one server is up at a
// time. Agents follow once the control plane is fully on the new image.
func k3sNodesInUpgradeOrder(nodes []NodeInfo) []NodeInfo {
	ordered := make([]NodeInfo, 0, len(nodes))
	ordered = append(ordered, serverNodes(nodes)...)
	ordered = append(ordered, agentNodes(nodes)...)

	return ordered
}

// nodeConfigFromInfo reconstructs the NodeConfig that launched a node from its recorded NodeInfo,
// so a recreate (-upgrade/-rollback/-rotate-token) reproduces the node's size, labels, and verbatim
// k3s args. Pre-v0.6.0 states carry zero Memory/NanoCPUs and nil Labels/ExtraArgs (the fields did
// not exist), so a recreate of an old node falls back to the runtime defaults — flagged in the
// NodeInfo doc, not silently presented as faithful.
func nodeConfigFromInfo(info NodeInfo) NodeConfig {
	return NodeConfig{
		Name:      info.Name,
		Role:      info.Role,
		Memory:    info.Memory,
		NanoCPUs:  info.NanoCPUs,
		Labels:    info.Labels,
		ExtraArgs: info.ExtraArgs,
	}
}

// clusterConfigFromState synthesizes the ClusterConfig the launch/recreate path needs from saved
// state. It mirrors what AddAgents/AddServer already do, centralized here because three day-2 ops
// need it. Image falls back to the pinned default for a pre-v0.2.0 state; the datastore client TLS
// dir is re-derived from disk (existingDatastoreTLSDir) so a recreated server reaches managed etcd
// with the SAME client bundle the original servers use. EnvVars are NOT recoverable (state never
// recorded -env), so a recreate drops them — a known limitation, flagged, not silently "correct".
func clusterConfigFromState(state ClusterState, stateDir, clusterDir string) ClusterConfig {
	image := state.Image
	if image == "" {
		image = defaultK3sImage
	}

	return ClusterConfig{
		Name:              state.ClusterName,
		Image:             image,
		Network:           state.Network,
		StateDir:          stateDir,
		Token:             state.Token,
		DatastoreEndpoint: state.DatastoreEndpoint,
		DatastoreImage:    state.DatastoreImage,
		DatastoreTLSDir:   existingDatastoreTLSDir(clusterDir),
	}
}

// recreateK3sNode stops and removes a k3s node's CONTAINER (its named state volume is left intact —
// rm targets the container only, and buildRunArgs re-derives the same volume name from the bare node
// name) and relaunches it from cfg, then re-arms ip_forward. This is the shared primitive behind a
// rolling -upgrade/-rollback (cfg.Image is the new image) and -rotate-token (cfg.Token is the new
// token): the only thing that changes is what cfg carries, so the recreate path can never diverge
// between the two. serverURL is "" for a server (it reconnects to the datastore) and the cluster API
// endpoint for an agent (its K3S_URL). No kubeconfig mount: the bootstrap copy already exists on the
// host, so a recreated server is launched join-style exactly like Create's launchJoinServers.
//
// PRESERVES the named volume (server datastore / agent state). REPLACES the container (and thus the
// image and the K3S_TOKEN env). This is the load-bearing data-safety property: a wrong change here
// — rm-ing the volume, or deriving a different volume name — would wipe the node's state.
func (p *provisioner) recreateK3sNode(ctx context.Context, cfg ClusterConfig, node NodeConfig, serverURL string, logw io.Writer) (NodeInfo, error) {
	id := nodeFQDN(node.Name, p.dnsDomain)

	fmt.Fprintln(logw, "recreating k3s node", node.Name)

	if err := p.stop(ctx, id); err != nil {
		return NodeInfo{}, fmt.Errorf("stopping node %q for recreate: %w", node.Name, err)
	}

	// remove the CONTAINER only — never volumeDelete here; the node's state volume must survive.
	if err := p.remove(ctx, id); err != nil {
		return NodeInfo{}, fmt.Errorf("removing node %q container for recreate: %w", node.Name, err)
	}

	info, err := p.launchNode(ctx, cfg, node, serverURL, "")
	if err != nil {
		return NodeInfo{}, err
	}

	if err := p.enableIPForward(ctx, info.ID); err != nil {
		return NodeInfo{}, fmt.Errorf("node %q: %w", node.Name, err)
	}

	return info, nil
}

// replaceNodeInState swaps the recorded NodeInfo for the node sharing info.ID (a node's FQDN ID is
// stable across a recreate, only its DHCP IP changes), so saved state reflects the recreated node's
// current IP and any new launch spec. Returns true when a slot matched. Pure helper.
func replaceNodeInState(state *ClusterState, info NodeInfo) bool {
	for i := range state.Nodes {
		if state.Nodes[i].ID == info.ID {
			state.Nodes[i] = info

			return true
		}
	}

	return false
}
