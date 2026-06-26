// SPDX-License-Identifier: MIT

package apple

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"strconv"
	"time"
)

// Create boots a k3s cluster on Apple's `container` runtime.
//
// Lifecycle (mirrors the Talos sibling's create.go shape, but encodes the k3s-specific
// server-first + readiness-gate ordering that Talos got from its framework):
//
//	validate -> ensureNetwork -> launch SERVER (sqlite, host-gw, tls-san, K3S_TOKEN preset)
//	  -> waitForIPv4(server) -> exec sysctl ip_forward=1 -> wait k3s READY (/readyz)
//	  -> for each AGENT: launch (K3S_URL + K3S_TOKEN) -> waitForIPv4 -> exec sysctl
//	  -> assertDistinctIPs -> saveState
//
// `container run` pulls the image on demand, so there is no explicit image-pull step.
//
// SPIKE DRAFT — this orchestration has NOT been run. Each step is a hypothesis gated in
// docs/VERIFICATION.md (G1 caps/containerd, G4 readiness, etc.).
func (p *provisioner) Create(ctx context.Context, cfg ClusterConfig, logw io.Writer) (ClusterState, error) {
	if logw == nil {
		logw = io.Discard
	}

	if err := validateClusterConfig(cfg); err != nil {
		return ClusterState{}, err
	}

	// Generate a shared K3S_TOKEN if the caller did not supply one.
	if cfg.Token == "" {
		tok, err := generateToken()
		if err != nil {
			return ClusterState{}, err
		}

		cfg.Token = tok
	}

	if cfg.ClusterDNS == "" {
		cfg.ClusterDNS = defaultClusterDNS
	}

	if cfg.Image == "" {
		cfg.Image = defaultK3sImage
	}

	fmt.Fprintln(logw, "ensuring network", cfg.Network)

	if err := p.ensureNetwork(ctx, cfg.Network, ""); err != nil {
		return ClusterState{}, fmt.Errorf("ensuring network: %w", err)
	}

	// Pre-create every node's datastore bind-mount dir on the host. apple/container's
	// --volume needs the host path to exist (UNVERIFIED whether it auto-creates; we
	// create it to be safe — gate G3).
	for _, node := range cfg.Nodes {
		dir := nodeDatastoreHostPath(cfg, node)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return ClusterState{}, fmt.Errorf("creating datastore dir %q: %w", dir, err)
		}
	}

	server, agents := splitRoles(cfg.Nodes)

	// 1) Launch the SERVER node.
	fmt.Fprintln(logw, "launching k3s server", server.Name)

	serverInfo, err := p.launchNode(ctx, cfg, server, "")
	if err != nil {
		return ClusterState{}, err
	}

	serverIP := serverInfo.IPs[0]

	// 2) ip_forward is mandatory and there is no systemd to set it — the kiac spike
	// proved k3s networking is broken without it; the Talos sibling hid this inside
	// machined. Do it explicitly via exec.
	if err := p.enableIPForward(ctx, serverInfo.ID); err != nil {
		return ClusterState{}, fmt.Errorf("server %q: %w", server.Name, err)
	}

	// 3) Wait for the k3s API to report ready before joining agents.
	fmt.Fprintln(logw, "waiting for k3s server readiness on", serverIP)

	if err := p.waitForReady(ctx, serverInfo.ID); err != nil {
		return ClusterState{}, fmt.Errorf("server %q readiness: %w", server.Name, err)
	}

	serverURL := "https://" + net.JoinHostPort(serverIP.String(), strconv.Itoa(k3sAPIPort))

	nodes := []NodeInfo{serverInfo}

	// 4) Launch AGENT nodes pointed at the server.
	for _, agent := range agents {
		fmt.Fprintln(logw, "launching k3s agent", agent.Name, "->", serverURL)

		info, err := p.launchNode(ctx, cfg, agent, serverURL)
		if err != nil {
			return ClusterState{}, err
		}

		if err := p.enableIPForward(ctx, info.ID); err != nil {
			return ClusterState{}, fmt.Errorf("agent %q: %w", agent.Name, err)
		}

		nodes = append(nodes, info)
	}

	// Everyday-correctness guard carried over from the Talos sibling: every node must
	// get a distinct vmnet IP, else the cluster silently breaks.
	if err := assertDistinctIPs(nodes); err != nil {
		return ClusterState{}, err
	}

	state := ClusterState{
		Provisioner: ProviderName,
		ClusterName: cfg.Name,
		Network:     cfg.Network,
		Token:       cfg.Token,
		StateDir:    cfg.StateDir,
		ServerURL:   serverURL,
		Nodes:       nodes,
	}

	if err := saveState(state); err != nil {
		return ClusterState{}, err
	}

	return state, nil
}

// launchNode runs one node and returns its NodeInfo once it has a vmnet IP.
func (p *provisioner) launchNode(ctx context.Context, cfg ClusterConfig, node NodeConfig, serverURL string) (NodeInfo, error) {
	args := buildRunArgs(cfg, node, serverURL)

	if _, err := p.run(ctx, args...); err != nil {
		return NodeInfo{}, fmt.Errorf("launching node %q: %w", node.Name, err)
	}

	// apple/container uses --name as the container ID (Talos sibling finding).
	id := node.Name

	addr, err := p.waitForIPv4(ctx, id)
	if err != nil {
		return NodeInfo{}, err
	}

	return NodeInfo{
		ID:   id,
		Name: node.Name,
		Role: node.Role,
		IPs:  []netip.Addr{addr},
	}, nil
}

// enableIPForward sets net.ipv4.ip_forward=1 inside a node. Mandatory for k3s pod
// networking; the kiac spike proved it. UNVERIFIED that `container exec ... sysctl`
// works and that the guest kernel permits the write even with --cap-add ALL (G1/G4).
func (p *provisioner) enableIPForward(ctx context.Context, id string) error {
	if _, err := p.exec(ctx, id, "sysctl", "-w", "net.ipv4.ip_forward=1"); err != nil {
		return fmt.Errorf("enabling ip_forward: %w", err)
	}

	return nil
}

// readyTimeout bounds how long we wait for the k3s server to report ready.
const readyTimeout = 120 * time.Second

// waitForReady polls k3s readiness from INSIDE the server node via
// `k3s kubectl get --raw /readyz`.
//
// Why exec-and-not-HTTPS (decided, see G4): there is no systemd to query, and an HTTPS
// GET to https://<ip>:6443/readyz from the host would need TLS handling (the server CA
// is not yet fetchable as a kubeconfig at this point in bring-up). The in-node
// `k3s kubectl` uses /etc/rancher/k3s/k3s.yaml, which already trusts the local CA, so it
// is the simplest correct probe. UNVERIFIED that /readyz returns "ok" before the
// kubeconfig is host-fetchable, and that `k3s kubectl` is on PATH this early (G4).
func (p *provisioner) waitForReady(ctx context.Context, id string) error {
	deadline := time.Now().Add(readyTimeout)

	for {
		out, err := p.exec(ctx, id, "k3s", "kubectl", "get", "--raw", "/readyz")
		if err == nil && out == "ok" {
			return nil
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for /readyz (last: %q, err: %v)", out, err)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// splitRoles returns the single server node and the agent nodes. validateClusterConfig
// has already guaranteed exactly one server, so indexing [0] of the servers is safe.
func splitRoles(nodes []NodeConfig) (server NodeConfig, agents []NodeConfig) {
	for _, n := range nodes {
		if n.Role == RoleServer {
			server = n
		} else {
			agents = append(agents, n)
		}
	}

	return server, agents
}

// validateClusterConfig rejects requests that would break bring-up before launching
// anything. The meaningful boundary (BVA, CLAUDE.md k) is the server count: this is a
// sqlite single-server cluster, so exactly ONE server is required.
//   - 0 servers  (B-1): rejected — nothing owns the datastore/API.
//   - 1 server   (B)  : accepted — the single-server case k3s+sqlite supports.
//   - 2+ servers (B+1): rejected — multi-server needs embedded etcd (--cluster-init),
//     which we deliberately do NOT enable (see node.go: etcd's IP-bound membership does
//     not survive the vmnet IP-change problem).
func validateClusterConfig(cfg ClusterConfig) error {
	servers := 0

	for _, n := range cfg.Nodes {
		if n.Role == RoleServer {
			servers++
		}
	}

	switch {
	case servers == 0:
		return fmt.Errorf("cluster %q: exactly one server node is required, got 0 (of %d nodes)",
			cfg.Name, len(cfg.Nodes))
	case servers > 1:
		return fmt.Errorf("cluster %q: this is a sqlite single-server launcher; got %d servers (multi-server needs embedded etcd, which is intentionally disabled)",
			cfg.Name, servers)
	}

	return nil
}

// assertDistinctIPs fails if any two nodes share an IP (everyday-correctness regression
// guard, carried over verbatim from the Talos sibling).
func assertDistinctIPs(nodes []NodeInfo) error {
	seen := make(map[string]string, len(nodes))

	for _, node := range nodes {
		if len(node.IPs) == 0 {
			return fmt.Errorf("node %q has no IP", node.Name)
		}

		ip := node.IPs[0].String()
		if other, dup := seen[ip]; dup {
			return fmt.Errorf("nodes %q and %q were both assigned IP %s", other, node.Name, ip)
		}

		seen[ip] = node.Name
	}

	return nil
}
