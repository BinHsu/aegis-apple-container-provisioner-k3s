// SPDX-License-Identifier: MIT

package apple

import (
	"bytes"
	"context"
	"crypto/tls"
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

	// The kubeconfig is delivered via a host BIND-MOUNT, not container cp. k3s writes its
	// admin kubeconfig into the bind-mounted cluster state dir (--write-kubeconfig), and we
	// read it straight off the host filesystem. This avoids container cp / exec entirely —
	// both ride the guest agent (vminitd over vsock), which faults under k3s's cold-boot
	// image-extraction I/O and SIGKILLs the cp, cascading to a whole-daemon stop/rm hang
	// (Apple containerization #678/#712, container #861; verified 2026-06-27). The mount
	// must exist before the server launches, so resolve+create it now and hand the absolute
	// path to the server's run args. (Absolute is also required: container resolves a
	// relative bind source against the container root.)
	rel := filepath.Join(cfg.StateDir, cfg.Name)
	clusterDir, err := filepath.Abs(rel)
	if err != nil {
		return ClusterState{}, fmt.Errorf("resolving cluster state dir %q: %w", rel, err)
	}
	if err := os.MkdirAll(clusterDir, 0o755); err != nil {
		return ClusterState{}, fmt.Errorf("creating cluster state dir %q: %w", clusterDir, err)
	}

	// 1) Launch the SERVER node. clusterDir is bind-mounted so k3s writes the kubeconfig
	// straight to the host (see the note above).
	fmt.Fprintln(logw, "launching k3s server", server.Name)

	serverInfo, err := p.launchNode(ctx, cfg, server, "", clusterDir)
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

	// 3) Wait for the k3s API server to answer on the network, then read the kubeconfig that
	// k3s has written to the bind-mounted host dir. No container cp — see the note above.
	kubeconfigSrc := filepath.Join(clusterDir, kubeconfigFileName) // written by k3s via the mount
	kubeconfigPath := filepath.Join(clusterDir, "kubeconfig")      // operator copy, endpoint rewritten

	fmt.Fprintln(logw, "waiting for k3s API server on the network")

	if err := p.waitForReady(ctx, serverIP, kubeconfigSrc); err != nil {
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

	raw, err := os.ReadFile(kubeconfigSrc)
	if err != nil {
		return ClusterState{}, fmt.Errorf("reading kubeconfig written by k3s at %q: %w", kubeconfigSrc, err)
	}

	if err := os.WriteFile(kubeconfigPath, rewriteKubeconfigServer(raw, serverURL), 0o600); err != nil {
		return ClusterState{}, fmt.Errorf("writing kubeconfig %q: %w", kubeconfigPath, err)
	}

	fmt.Fprintf(logw, "kubeconfig saved to %s\n", kubeconfigPath)

	nodes := []NodeInfo{serverInfo}

	// 5) Launch AGENT nodes pointed at the server.
	for _, agent := range agents {
		fmt.Fprintln(logw, "launching k3s agent", agent.Name, "->", serverURL)

		info, err := p.launchNode(ctx, cfg, agent, serverURL, "")
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
		Image:       cfg.Image, // resolved (empty->defaultK3sImage above); AddAgents reuses it
		ServerURL:   serverURL,
		Nodes:       nodes,
	}

	if err := saveState(state); err != nil {
		return ClusterState{}, err
	}

	return state, nil
}

// launchNode runs one node and returns its NodeInfo once it has a vmnet IP.
func (p *provisioner) launchNode(ctx context.Context, cfg ClusterConfig, node NodeConfig, serverURL, kubeconfigHostDir string) (NodeInfo, error) {
	args := buildRunArgs(cfg, node, serverURL, p.dnsDomain, kubeconfigHostDir)

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

// readyTimeout bounds how long we wait for the k3s API server to start answering on
// <serverIP>:6443. k3s on a cold VM is normally serving within ~60-90s; 120s leaves margin
// for a slow image-extraction boot without waiting on a genuinely dead server forever.
const readyTimeout = 120 * time.Second

// apiDialTimeout and apiPollInterval bound the host-side TLS readiness probe: each dial
// gives up after apiDialTimeout, and we re-dial every apiPollInterval until readyTimeout.
const (
	apiDialTimeout  = 5 * time.Second
	apiPollInterval = 3 * time.Second
)

// kubeconfigWriteTimeout / kubeconfigPollInterval bound the wait for k3s to finish writing
// its admin kubeconfig to the bind-mounted host path. The API server being up normally
// means the file is already present; this only covers the brief write race.
const (
	kubeconfigWriteTimeout = 30 * time.Second
	kubeconfigPollInterval = 1 * time.Second
)

// waitForReady gates bring-up on the k3s API server becoming reachable from the HOST over
// the network, then waits for k3s to land its admin kubeconfig on the bind-mounted host
// path. Neither step touches the guest agent.
//
// WHY no cp/exec: `container cp` and `container exec` both ride the guest agent (vminitd
// over vsock), which faults under k3s's cold-boot image-extraction I/O — even a single
// un-killed cp in that window gets SIGKILLed, and the fault cascades to container stop/rm
// (whole-daemon hang, recoverable only by force-killing the runtime helper; verified
// 2026-06-27, Apple containerization #678/#712). A host-side TLS dial to <serverIP>:6443 is
// answered by kube-apiserver directly, and the kubeconfig arrives via a host bind-mount
// (see kubeconfigMount), so the whole readiness+delivery path is guest-agent-free.
func (p *provisioner) waitForReady(ctx context.Context, serverIP netip.Addr, kubeconfigSrc string) error {
	if err := p.waitForAPIServer(ctx, serverIP); err != nil {
		return err
	}

	return waitForKubeconfigFile(ctx, kubeconfigSrc)
}

// waitForKubeconfigFile waits for k3s to write a non-empty admin kubeconfig to the
// bind-mounted host path. Pure host filesystem (os.Stat) — no container cp/exec, so it
// cannot wedge the guest agent.
func waitForKubeconfigFile(ctx context.Context, path string) error {
	deadline := time.Now().Add(kubeconfigWriteTimeout)

	for {
		if info, err := os.Stat(path); err == nil && info.Size() > 0 {
			return nil
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("k3s did not write a kubeconfig to %q within %s", path, kubeconfigWriteTimeout)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(kubeconfigPollInterval):
		}
	}
}

// waitForAPIServer polls a TLS handshake against <serverIP>:6443 until kube-apiserver
// answers or readyTimeout elapses. A completed handshake means the API server is up (and
// therefore k3s.yaml has been written). The probe never trusts the connection — it only
// proves the listener is live — so certificate verification is skipped.
func (p *provisioner) waitForAPIServer(ctx context.Context, serverIP netip.Addr) error {
	addr := net.JoinHostPort(serverIP.String(), strconv.Itoa(k3sAPIPort))
	deadline := time.Now().Add(readyTimeout)
	dialer := &net.Dialer{Timeout: apiDialTimeout}
	tlsCfg := &tls.Config{InsecureSkipVerify: true} //nolint:gosec // readiness probe only; the connection is closed immediately and never trusted.

	var lastErr error
	for {
		conn, err := tls.DialWithDialer(dialer, "tcp", addr, tlsCfg)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		lastErr = err

		if time.Now().After(deadline) {
			return fmt.Errorf("k3s API server at %s not reachable within %s: %w", addr, readyTimeout, lastErr)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(apiPollInterval):
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
