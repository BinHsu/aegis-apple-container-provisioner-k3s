// SPDX-License-Identifier: MIT

package apple

import (
	"context"
	"fmt"
	"io"
	"net/netip"
)

// rotate.go implements the v0.6.0 cert and token rotation. Both are DESTRUCTIVE (they restart or
// recreate every node) and need -force.
//
//   - -rotate-certs regenerates the managed etcd CA + every member/client cert (reusing etcd_tls.go),
//     redelivers them over the existing bind-mounts, and restarts etcd + the servers in lifecycle
//     order so the new certs take effect. It then rotates each k3s SERVER's own certificates with an
//     OFFLINE one-shot container (`k3s certificate rotate` on the server's stopped data volume) before
//     restarting it — the same image, mount, and "no exec into a k3s multi-call binary" discipline the
//     rest of the provisioner uses.
//   - -rotate-token generates a new K3S_TOKEN, rotates it on a running server, then recreates every
//     server and agent so the new token is baked into their `container run` env (and survives a cold
//     restart). PRESERVES every named state volume — only the containers are replaced.
//
// HARDWARE CAVEATS (flagged loudly, see the per-function comments): the etcd-TLS rotation is fully in
// our control and is the verified-design part. The k3s-side `certificate rotate` / `token rotate`
// semantics for an EXTERNAL-datastore cluster (where k3s persists its bootstrap data + certs in the
// datastore, not the local data dir) are the integration risk and MUST be hardware-verified.

// buildK3sCertRotateArgs assembles the `container run` vector for the OFFLINE k3s certificate-rotate
// one-shot: the same k3s image and state-volume mount the server normally runs with, but the command
// is `certificate rotate` instead of `server`. Pure (unit-testable). Foreground one-shot — it runs
// while the server container is stopped, mutates the certs on the named volume, and exits, after which
// the server is started again. This avoids `container exec` into the running k3s multi-call binary
// (the documented footgun in container.go); the image entrypoint runs `k3s <args>`, so passing
// `certificate rotate` invokes `k3s certificate rotate` cleanly.
func buildK3sCertRotateArgs(cfg ClusterConfig, node NodeConfig) []string {
	args := []string{
		"run",
		"--name", node.Name + "-certrotate",
		"--volume", nodeVolumeName(cfg.Name, node.Name) + ":" + k3sDatastoreMount,
	}

	if cfg.Network != "" {
		args = append(args, "--network", cfg.Network)
	}

	args = append(args, cfg.Image, "certificate", "rotate", "--data-dir", k3sDatastoreMount)

	return args
}

// k3sTokenRotateExecArgs is the `k3s token rotate` argument vector run on a LIVE server (token rotate
// re-encrypts the cluster's bootstrap data with the new token in the datastore, so it needs a running
// server, unlike the offline certificate rotate). Pure so the flag composition is unit-testable. NOTE:
// this is the one place a k3s subcommand is execed into a running container; `token` is a real k3s
// subcommand (not a kubectl/crictl multi-call symlink), so it dispatches cleanly — but the
// external-datastore token semantics are a flagged hardware-verification item.
func k3sTokenRotateExecArgs(oldToken, newToken string) []string {
	return []string{"k3s", "token", "rotate", "--token", oldToken, "--new-token", newToken}
}

// RotateCerts regenerates and redelivers the managed etcd TLS bundle and rotates each k3s server's
// certificates, restarting etcd + servers in lifecycle order. DESTRUCTIVE — needs -force. Requires a
// managed etcd datastore (the regenerated CA is the etcd quorum's).
func (p *provisioner) RotateCerts(ctx context.Context, stateDir, clusterName string, force bool, logw io.Writer) error {
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
		return fmt.Errorf("cluster %q has no managed etcd datastore; -rotate-certs rotates the managed etcd TLS "+
			"(bring-your-own-endpoint clusters own their datastore's TLS)", clusterName)
	}

	if err := ensureForced(force, logw, "-rotate-certs", []string{
		"regenerate the etcd CA and every member/client certificate (the OLD CA is discarded)",
		fmt.Sprintf("stop and restart every node of cluster %q in lifecycle order", clusterName),
		"rotate each k3s server's certificates (offline) and restart it",
	}); err != nil {
		return err
	}

	return p.runRotateCerts(ctx, &state, clusterDir, members, logw)
}

// runRotateCerts is the orchestration body of RotateCerts (extracted for the funlen/gocognit gates).
func (p *provisioner) runRotateCerts(ctx context.Context, state *ClusterState, clusterDir string, members []NodeInfo, logw io.Writer) error {
	cfg := clusterConfigFromState(*state, state.StateDir, clusterDir)

	// 1) Stop every node so nothing is using the old certs while they are replaced.
	for _, node := range stopOrder(state.Nodes) {
		fmt.Fprintln(logw, "stopping node", node.Name)

		if err := p.stop(ctx, node.ID); err != nil {
			return fmt.Errorf("stopping node %q: %w", node.Name, err)
		}
	}

	// 2) Regenerate the bundle and OVERWRITE the same on-disk dirs the members + servers bind-mount,
	// so a plain restart picks up the new certs (the bind-mount path is unchanged).
	if err := p.redeliverEtcdTLS(state.ClusterName, clusterDir, members); err != nil {
		return err
	}

	// 3) Restart etcd (loads new member certs), then rotate + restart each server (loads the new
	// client bundle and rotates its own certs), then start the LB + agents.
	if err := p.restartEtcdMembers(ctx, state, members, logw); err != nil {
		return err
	}

	if err := p.rotateK3sServerCerts(ctx, cfg, state, logw); err != nil {
		return err
	}

	if err := p.startLBAndAgents(ctx, state, logw); err != nil {
		return err
	}

	fmt.Fprintf(logw, "rotated certs for cluster %q (etcd TLS + k3s server certs)\n", state.ClusterName)

	return saveState(*state)
}

// redeliverEtcdTLS regenerates the etcd TLS bundle for the recorded members and writes it over the
// existing on-disk dirs (member dirs + the client dir). Reuses etcd_tls.go (deliverEtcdTLS) end to
// end; the member set comes from recorded state, so the regenerated bundle covers exactly the live
// members and lands at the SAME paths the members + servers already bind-mount, so a restart loads
// the new material.
func (p *provisioner) redeliverEtcdTLS(clusterName, clusterDir string, members []NodeInfo) error {
	memberCfgs := make([]NodeConfig, len(members))
	for i, m := range members {
		memberCfgs[i] = nodeConfigFromInfo(m)
	}

	if _, _, err := p.deliverEtcdTLS(clusterName, clusterDir, memberCfgs); err != nil {
		return err
	}

	return nil
}

// restartEtcdMembers starts each etcd member (picking up the new server certs) and waits for the
// quorum's client ports, updating each member's IP in state. Mirrors lifecycle start for the
// datastore role (no ip_forward — etcd is not a k3s node).
func (p *provisioner) restartEtcdMembers(ctx context.Context, state *ClusterState, members []NodeInfo, logw io.Writer) error {
	for _, m := range members {
		fmt.Fprintln(logw, "starting etcd member", m.Name)

		if err := p.start(ctx, m.ID); err != nil {
			return fmt.Errorf("starting etcd member %q: %w", m.Name, err)
		}

		addr, err := p.waitForIPv4(ctx, m.ID)
		if err != nil {
			return err
		}

		if err := p.waitForEtcdMember(ctx, addr); err != nil {
			return fmt.Errorf("etcd member %q readiness after cert rotation: %w", m.ID, err)
		}

		replaceNodeInState(state, NodeInfo{ID: m.ID, Name: m.Name, Role: RoleDatastore, IPs: []netip.Addr{addr}, Memory: m.Memory})
	}

	return nil
}

// rotateK3sServerCerts rotates each server's certificates offline (buildK3sCertRotateArgs on the
// stopped server's volume), then starts the server, re-arms ip_forward, and confirms its apiserver is
// back (which also proves it reloaded the new datastore client bundle). The servers were stopped in
// runRotateCerts step 1.
func (p *provisioner) rotateK3sServerCerts(ctx context.Context, cfg ClusterConfig, state *ClusterState, logw io.Writer) error {
	for _, s := range serverNodes(state.Nodes) {
		fmt.Fprintln(logw, "rotating k3s certificates for server", s.Name)

		helper := s.Name + "-certrotate"
		_ = p.remove(ctx, helper)

		if _, err := p.run(ctx, buildK3sCertRotateArgs(cfg, nodeConfigFromInfo(s))...); err != nil {
			return fmt.Errorf("k3s certificate rotate for server %q: %w", s.Name, err)
		}

		_ = p.remove(ctx, helper)

		if err := p.start(ctx, s.ID); err != nil {
			return fmt.Errorf("starting server %q after cert rotation: %w", s.Name, err)
		}

		addr, err := p.waitForIPv4(ctx, s.ID)
		if err != nil {
			return err
		}

		if err := p.enableIPForward(ctx, s.ID); err != nil {
			return fmt.Errorf("server %q: %w", s.Name, err)
		}

		if err := p.waitForAPIServer(ctx, addr); err != nil {
			return fmt.Errorf("server %q apiserver after cert rotation: %w", s.Name, err)
		}

		replaceNodeInState(state, NodeInfo{
			ID: s.ID, Name: s.Name, Role: RoleServer, IPs: []netip.Addr{addr},
			Memory: s.Memory, NanoCPUs: s.NanoCPUs, Labels: s.Labels, ExtraArgs: s.ExtraArgs,
		})
	}

	return nil
}

// startLBAndAgents starts the API LB and the agents (the nodes runRotateCerts has not started yet),
// re-arming ip_forward on the agents. Servers + datastore are already up by here.
func (p *provisioner) startLBAndAgents(ctx context.Context, state *ClusterState, logw io.Writer) error {
	for _, node := range startOrder(state.Nodes) {
		if node.Role == RoleDatastore || node.Role == RoleServer {
			continue // already started
		}

		fmt.Fprintln(logw, "starting node", node.Name)

		if err := p.start(ctx, node.ID); err != nil {
			return fmt.Errorf("starting node %q: %w", node.Name, err)
		}

		if node.Role != RoleAgent {
			continue // LB: not a k3s node
		}

		addr, err := p.waitForIPv4(ctx, node.ID)
		if err != nil {
			return err
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

// RotateToken generates a new K3S_TOKEN, rotates it on a running server, and recreates every server
// and agent so the new token is in their `container run` env. DESTRUCTIVE — needs -force. Updates
// state.Token. PRESERVES every named state volume (only containers are replaced).
func (p *provisioner) RotateToken(ctx context.Context, stateDir, clusterName string, force bool, logw io.Writer) (ClusterState, error) {
	if logw == nil {
		logw = io.Discard
	}

	state, err := LoadState(stateDir, clusterName)
	if err != nil {
		return ClusterState{}, err
	}

	clusterDir, err := ensureClusterDir(stateDir, clusterName)
	if err != nil {
		return ClusterState{}, err
	}

	servers := serverNodes(state.Nodes)
	if len(servers) == 0 {
		return ClusterState{}, fmt.Errorf("cluster %q has no server to rotate the token on", clusterName)
	}

	if err := ensureForced(force, logw, "-rotate-token", []string{
		fmt.Sprintf("generate a new K3S_TOKEN for cluster %q and rotate it on a server", clusterName),
		"recreate every server and agent with the new token (state volumes preserved)",
	}); err != nil {
		return ClusterState{}, err
	}

	return p.runRotateToken(ctx, &state, stateDir, clusterDir, servers, logw)
}

// runRotateToken is the orchestration body of RotateToken (extracted for the funlen/gocognit gates).
func (p *provisioner) runRotateToken(ctx context.Context, state *ClusterState, stateDir, clusterDir string, servers []NodeInfo, logw io.Writer) (ClusterState, error) {
	newToken, err := generateToken()
	if err != nil {
		return ClusterState{}, err
	}

	// Rotate the token on a LIVE bootstrap server so the cluster's bootstrap data is re-encrypted with
	// the new token in the datastore BEFORE any node is recreated with it. HARDWARE-VERIFY: external-
	// datastore token semantics (see the file header).
	fmt.Fprintln(logw, "rotating k3s token on server", servers[0].Name)

	if _, err := p.exec(ctx, servers[0].ID, k3sTokenRotateExecArgs(state.Token, newToken)...); err != nil {
		return ClusterState{}, fmt.Errorf("k3s token rotate on %q: %w", servers[0].Name, err)
	}

	// Recreate every server then every agent with the new token baked into their env. recreateK3sNode
	// preserves each node's state volume.
	cfg := clusterConfigFromState(*state, stateDir, clusterDir)
	cfg.Token = newToken

	for _, node := range k3sNodesInUpgradeOrder(state.Nodes) {
		url := ""
		if node.Role == RoleAgent {
			url = state.ServerURL
		}

		info, err := p.recreateK3sNode(ctx, cfg, nodeConfigFromInfo(node), url, logw)
		if err != nil {
			return ClusterState{}, err
		}

		replaceNodeInState(state, info)
	}

	state.Token = newToken

	if err := saveState(*state); err != nil {
		return ClusterState{}, err
	}

	fmt.Fprintf(logw, "rotated K3S_TOKEN for cluster %q and re-registered all servers + agents\n", state.ClusterName)

	return *state, nil
}
