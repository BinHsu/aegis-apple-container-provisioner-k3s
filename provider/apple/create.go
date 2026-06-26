// SPDX-License-Identifier: MIT

package apple

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"time"
)

// Create boots a k3s cluster on Apple's `container` runtime.
//
// Lifecycle (mirrors the Talos sibling's create.go shape, but encodes the k3s-specific
// server-first + readiness-gate ordering that Talos got from its framework):
//
//	validate -> ensureNetwork -> DNS domain precheck
//	  -> prepareNodeVolumes (create named volumes, stale-state guard)
//	  -> launch SERVER (sqlite, host-gw, tls-san, K3S_TOKEN preset)
//	  -> waitForIPv4(server) -> exec sysctl ip_forward=1 -> wait k3s READY (/readyz)
//	  -> for each AGENT: launch (K3S_URL=FQDN + K3S_TOKEN) -> waitForIPv4 -> exec sysctl
//	  -> assertDistinctIPs -> saveState
//
// `container run` pulls the image on demand, so there is no explicit image-pull step.
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

	if cfg.Image == "" {
		cfg.Image = defaultK3sImage
	}

	fmt.Fprintln(logw, "ensuring network", cfg.Network)

	if err := p.ensureNetwork(ctx, cfg.Network, ""); err != nil {
		return ClusterState{}, fmt.Errorf("ensuring network: %w", err)
	}

	// DNS domain precheck: if FQDN-endpoint mode is enabled, the resolver entry must
	// exist before containers are launched. A missing entry means host-to-container FQDN
	// lookups silently fall through to public DNS — the cluster would come up but the
	// stable-endpoint goal is not met. Failing early avoids a hard-to-diagnose
	// connectivity gap after a (successful) create. Mirrors the Talos sibling.
	if p.dnsDomain != "" {
		if err := p.checkDNSDomain(ctx, p.dnsDomain); err != nil {
			return ClusterState{}, err
		}
	}

	// Create each node's k3s datastore named volume before launch, and refuse to boot
	// onto stale state from a prior run (see prepareNodeVolumes). Volumes are stamped
	// with the cluster labels so the destroy label sweep can reclaim them even if this
	// Create fails before saveState (the half-created-cluster gap).
	createVolume := func(ctx context.Context, name string) error {
		return p.volumeCreate(ctx, name, volumeLabels(cfg.Name)...)
	}

	if err := prepareNodeVolumes(ctx, cfg.Name, cfg.Nodes, p.volumeExists, createVolume); err != nil {
		return ClusterState{}, err
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

	// 4) Compute the server URL: use the FQDN endpoint when dns-domain is set so agents
	// join via a stable name that survives cold-restart IP changes; fall back to the
	// current DHCP IP in IP-only mode.
	var serverURL string
	if p.dnsDomain != "" {
		serverFQDN := nodeFQDN(server.Name, p.dnsDomain)
		serverURL = "https://" + net.JoinHostPort(serverFQDN, strconv.Itoa(k3sAPIPort))
	} else {
		serverURL = "https://" + net.JoinHostPort(serverIP.String(), strconv.Itoa(k3sAPIPort))
	}

	nodes := []NodeInfo{serverInfo}

	// 5) Launch AGENT nodes pointed at the server.
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
	args := buildRunArgs(cfg, node, serverURL, p.dnsDomain)

	if _, err := p.run(ctx, args...); err != nil {
		return NodeInfo{}, fmt.Errorf("launching node %q: %w", node.Name, err)
	}

	// The container ID is the --name we passed: FQDN when a DNS domain is set, bare name
	// otherwise. NodeInfo.ID carries this so stop/remove/inspect all use the right handle.
	// NodeInfo.Name stays as the bare node name so volume naming (nodeVolumeName) and
	// destroy label-sweep both derive the same identifiers as at create time.
	id := nodeFQDN(node.Name, p.dnsDomain)

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
// networking; the kiac spike proved it.
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
// is the simplest correct probe.
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

// prepareNodeVolumes creates each node's k3s datastore named volume, and guards
// against booting onto stale state.
//
// The guard is the load-bearing consequence of using named volumes: a volume left behind
// by a prior run carries an old sqlite database (server) or old kubelet/agent state.
// Reusing either silently would resurrect a stale, half-broken cluster (wrong certs,
// divergent state) rather than boot a clean one. We refuse and tell the operator to
// destroy first — never silent reuse, never silent wipe. Mirrors the Talos sibling's
// prepareNodeVolumes.
//
// exists/create are injected (p.volumeExists / p.volumeCreate in production) so the
// guard is unit-testable without the `container` CLI.
func prepareNodeVolumes(
	ctx context.Context,
	clusterName string,
	nodes []NodeConfig,
	exists func(context.Context, string) (bool, error),
	create func(context.Context, string) error,
) error {
	for _, node := range nodes {
		vol := nodeVolumeName(clusterName, node.Name)

		present, err := exists(ctx, vol)
		if err != nil {
			return fmt.Errorf("checking volume %q for node %q: %w", vol, node.Name, err)
		}

		if present {
			return fmt.Errorf(
				"node %q: named volume %q already exists (stale state from a prior run); "+
					"run destroy for this cluster first — refusing to reuse it",
				node.Name, vol,
			)
		}

		if err := create(ctx, vol); err != nil {
			return fmt.Errorf("creating volume %q for node %q: %w", vol, node.Name, err)
		}
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
