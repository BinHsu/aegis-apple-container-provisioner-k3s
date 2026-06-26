// SPDX-License-Identifier: MIT

package apple

import (
	"bytes"
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

// Create boots a k3s cluster on Apple's `container` runtime.
//
// Lifecycle (mirrors the Talos sibling's create.go shape, but encodes the k3s-specific
// server-first + readiness-gate ordering that Talos got from its framework):
//
//	validate -> ensureNetwork -> DNS domain precheck
//	  -> prepareNodeVolumes (create named volumes, stale-state guard)
//	  -> launch SERVER (sqlite, host-gw, tls-san, K3S_TOKEN preset)
//	  -> waitForIPv4(server) -> exec sysctl ip_forward=1
//	  -> poll k3s.yaml via container cp (readiness signal + kubeconfig delivery)
//	  -> rewrite kubeconfig server URL -> save to <stateDir>/<cluster>/kubeconfig
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

	// 3) Wait for k3s to initialize by polling `container cp` for k3s.yaml.
	// k3s writes /etc/rancher/k3s/k3s.yaml only once the API server is fully up, so a
	// successful cp is a reliable "server is up" signal. It also delivers the kubeconfig
	// in one shot — no separate fetch step. Ensure the cluster state dir exists first.
	clusterDir := filepath.Join(cfg.StateDir, cfg.Name)
	if err := os.MkdirAll(clusterDir, 0o755); err != nil {
		return ClusterState{}, fmt.Errorf("creating cluster state dir %q: %w", clusterDir, err)
	}

	kubeconfigPath := filepath.Join(clusterDir, "kubeconfig")

	fmt.Fprintln(logw, "waiting for k3s to initialize (polling k3s.yaml via container cp)")

	if err := p.waitForReady(ctx, serverInfo.ID, kubeconfigPath); err != nil {
		return ClusterState{}, fmt.Errorf("server %q readiness: %w", server.Name, err)
	}

	// 4) Compute the server URL, then rewrite the kubeconfig's loopback server address.
	// k3s always writes https://127.0.0.1:6443; rewrite to the FQDN (stable across
	// cold-restart IP changes when dns-domain is set) or the current DHCP IP otherwise.
	var serverURL string
	if p.dnsDomain != "" {
		serverURL = "https://" + net.JoinHostPort(nodeFQDN(server.Name, p.dnsDomain), strconv.Itoa(k3sAPIPort))
	} else {
		serverURL = "https://" + net.JoinHostPort(serverIP.String(), strconv.Itoa(k3sAPIPort))
	}

	raw, err := os.ReadFile(kubeconfigPath)
	if err != nil {
		return ClusterState{}, fmt.Errorf("reading fetched kubeconfig: %w", err)
	}

	if err := os.WriteFile(kubeconfigPath, rewriteKubeconfigServer(raw, serverURL), 0o600); err != nil {
		return ClusterState{}, fmt.Errorf("writing kubeconfig %q: %w", kubeconfigPath, err)
	}

	fmt.Fprintf(logw, "kubeconfig saved to %s\n", kubeconfigPath)

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

// readyTimeout bounds how long we wait for the k3s server to initialize.
const readyTimeout = 120 * time.Second

// cpAttemptTimeout bounds a single `container cp` readiness probe so a wedged container daemon
// (e.g. under extreme host memory pressure) cannot hang the readiness loop forever — the attempt
// is killed via its context and retried until readyTimeout. (Origin 2026-06-26: a cp wedged ~4h
// with no per-call bound, holding the VM's RAM and a stuck daemon.)
const cpAttemptTimeout = 15 * time.Second

// waitForReady polls until the k3s server has written /etc/rancher/k3s/k3s.yaml. k3s
// only creates this file once the API server is fully initialized (CA issued,
// control-plane healthy), so a successful `container cp` is a reliable "server is up"
// signal. The file is written directly to kubeconfigPath, so the caller can immediately
// rewrite the server URL and hand it to the operator — no separate fetch step needed.
//
// WHY cp AND NOT exec: `container exec` mangles the rancher/k3s multi-call binary's args.
// `container exec <id> k3s kubectl get --raw /readyz` produces "unknown command 'kubectl'
// for 'kubectl'" — verified on G1 hardware 2026-06-26. `container cp` has no such
// restriction; it copies the file directly from the container filesystem.
func (p *provisioner) waitForReady(ctx context.Context, id, kubeconfigPath string) error {
	deadline := time.Now().Add(readyTimeout)
	src := id + ":/etc/rancher/k3s/k3s.yaml"

	for {
		attemptCtx, cancel := context.WithTimeout(ctx, cpAttemptTimeout)
		err := p.containerCP(attemptCtx, src, kubeconfigPath)
		cancel()

		if err == nil {
			return nil
		}

		if time.Now().After(deadline) {
			return fmt.Errorf(
				"timed out after %s: k3s.yaml not yet written by %q (server may still be initializing)",
				readyTimeout, id,
			)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// rewriteKubeconfigServer replaces every occurrence of the k3s loopback server address
// (https://127.0.0.1:6443) in a fetched kubeconfig with newServerURL. k3s always writes
// the loopback as the server address; this substitution makes the kubeconfig usable from
// the host. Extracted as a pure function so the replacement is unit-testable without the
// container CLI.
func rewriteKubeconfigServer(in []byte, newServerURL string) []byte {
	return bytes.ReplaceAll(in, []byte("https://127.0.0.1:6443"), []byte(newServerURL))
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
