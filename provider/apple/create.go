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
//	  -> launch BOOTSTRAP server (host-gw, tls-san, K3S_TOKEN preset, kubeconfig bind-mount;
//	     embedded sqlite OR --datastore-endpoint when HA)
//	  -> waitForIPv4 -> exec sysctl ip_forward=1 -> waitForReady (TLS dial + kubeconfig file)
//	  -> rewrite kubeconfig server URL -> save to <stateDir>/<cluster>/kubeconfig
//	  -> launchJoinServers (HA only): each additional server joins the SHARED datastore
//	  -> for each AGENT: launch (K3S_URL=FQDN + K3S_TOKEN) -> waitForIPv4 -> exec sysctl
//	  -> assertDistinctIPs -> saveState
//
// `container run` pulls the image on demand, so there is no explicit image-pull step.
// launchAgents launches every agent node pointed at serverURL (the LB FQDN when an LB was
// provisioned, else the bootstrap server), arming ip_forward on each. Extracted from Create to
// keep it readable and under the cognitive-complexity gate.
func (p *provisioner) launchAgents(ctx context.Context, cfg ClusterConfig, agents []NodeConfig, serverURL string, logw io.Writer) ([]NodeInfo, error) {
	var infos []NodeInfo

	for _, agent := range agents {
		fmt.Fprintln(logw, "launching k3s agent", agent.Name, "->", serverURL)

		info, err := p.launchNode(ctx, cfg, agent, serverURL, "")
		if err != nil {
			return nil, err
		}

		if err := p.enableIPForward(ctx, info.ID); err != nil {
			return nil, fmt.Errorf("agent %q: %w", agent.Name, err)
		}

		infos = append(infos, info)
	}

	return infos, nil
}

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

	// Managed datastore (HA): provision the etcd cluster and wire its --datastore-endpoint BEFORE
	// any server launches. A bring-your-own DatastoreEndpoint skips this. Returns one NodeInfo per
	// etcd member. Extracted to setupDatastore to keep Create readable.
	datastoreInfos, endpoint, err := p.setupDatastore(ctx, cfg, logw)
	if err != nil {
		return ClusterState{}, err
	}

	cfg.DatastoreEndpoint = endpoint

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

	servers, agents := splitRoles(cfg.Nodes)
	server := servers[0] // bootstrap server: launched first, delivers the kubeconfig

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

	// AUTO-DEPLOY MANIFESTS (v0.4.0): resolve each requested manifest to an absolute path
	// (the `container` runtime resolves a relative bind source against the container root —
	// same absolute-path requirement as the kubeconfig mount above), confirm it exists, and
	// reject basename collisions (two files would mount to the same in-node path). Done once,
	// before any launch, so a bad -manifest fails fast instead of mid-bring-up. buildRunArgs
	// mounts the resolved paths into the bootstrap server only.
	if len(cfg.Manifests) > 0 {
		resolved, err := resolveManifests(cfg.Manifests)
		if err != nil {
			return ClusterState{}, err
		}

		cfg.Manifests = resolved
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

	// 3+4) Gate on the bootstrap API server, then deliver the kubeconfig (loopback rewritten to
	// the stable cluster endpoint — the LB FQDN in HA, else the server). Extracted to
	// deliverKubeconfig to keep Create readable.
	serverURL, err := p.deliverKubeconfig(ctx, cfg, server, serverIP, clusterDir, logw)
	if err != nil {
		return ClusterState{}, err
	}

	nodes := []NodeInfo{serverInfo}

	// 4.5) Launch ADDITIONAL servers for HA (servers[1:]). Extracted to keep Create readable;
	// see launchJoinServers for why these join with no kubeconfig mount and no --cluster-init.
	joinInfos, err := p.launchJoinServers(ctx, cfg, servers[1:], logw)
	if err != nil {
		return ClusterState{}, err
	}

	nodes = append(nodes, joinInfos...)

	// 4.6) Provision the API load balancer (multi-server HA only). It fronts every server at the
	// shared FQDN serverURL already points at, so it must come up before agents join through it.
	// Extracted to setupAPILB; returns nil when no LB is needed (single server or IP-only mode).
	lbInfo, err := p.setupAPILB(ctx, cfg, servers, clusterDir, logw)
	if err != nil {
		return ClusterState{}, err
	}

	if lbInfo != nil {
		nodes = append(nodes, *lbInfo)
	}

	// 5) Launch AGENT nodes pointed at the server endpoint (the LB FQDN when an LB was provisioned).
	// Extracted to launchAgents to keep Create readable and under the cognitive-complexity gate.
	agentInfos, err := p.launchAgents(ctx, cfg, agents, serverURL, logw)
	if err != nil {
		return ClusterState{}, err
	}

	nodes = append(nodes, agentInfos...)

	// Record the managed datastore (etcd) members FIRST in state, so teardown and reporting see
	// them. They are not k3s nodes — they carry no kubeconfig and join no k3s cluster — but they
	// are real infrastructure to reclaim, so they ride ClusterState.Nodes with RoleDatastore.
	if len(datastoreInfos) > 0 {
		nodes = append(append([]NodeInfo{}, datastoreInfos...), nodes...)
	}

	// Everyday-correctness guard carried over from the Talos sibling: every node must
	// get a distinct vmnet IP, else the cluster silently breaks. The datastore VM is
	// included — its IP must differ from every k3s node too.
	if err := assertDistinctIPs(nodes); err != nil {
		return ClusterState{}, err
	}

	state := ClusterState{
		Provisioner:       ProviderName,
		ClusterName:       cfg.Name,
		Network:           cfg.Network,
		Token:             cfg.Token,
		StateDir:          cfg.StateDir,
		Image:             cfg.Image, // resolved (empty->defaultK3sImage above); AddAgents reuses it
		DatastoreEndpoint: cfg.DatastoreEndpoint,
		ServerURL:         serverURL,
		Nodes:             nodes,
	}

	if err := saveState(state); err != nil {
		return ClusterState{}, err
	}

	return state, nil
}

// deliverKubeconfig gates on the bootstrap k3s API server becoming reachable, then reads the
// admin kubeconfig k3s wrote to the bind-mounted host dir, rewrites its loopback server address
// to the stable cluster endpoint, and writes the operator copy. No container cp: readiness is a
// host-side TLS dial and the kubeconfig arrives via the host bind-mount (ADR-0001).
//
// The endpoint is chosen by clusterAPIEndpoint: the shared LB FQDN https://<cluster>-api.<domain>:6443
// when an API LB will be provisioned (multi-server HA), else the bootstrap server's own FQDN
// (survives cold-restart IP changes), else the current DHCP IP (IP-only mode). The LB itself is
// provisioned by Create AFTER this call, but the URL is a stable name the LB registers before
// Create returns, so the operator copy is valid by the time it is used. Returns the endpoint URL
// (also used as the agents' K3S_URL).
func (p *provisioner) deliverKubeconfig(ctx context.Context, cfg ClusterConfig, server NodeConfig, serverIP netip.Addr, clusterDir string, logw io.Writer) (string, error) {
	kubeconfigSrc := filepath.Join(clusterDir, kubeconfigFileName) // written by k3s via the mount
	kubeconfigPath := filepath.Join(clusterDir, "kubeconfig")      // operator copy, endpoint rewritten

	fmt.Fprintln(logw, "waiting for k3s API server on the network")

	if err := p.waitForReady(ctx, serverIP, kubeconfigSrc); err != nil {
		return "", fmt.Errorf("server %q readiness: %w", server.Name, err)
	}

	serverURL := clusterAPIEndpoint(cfg, server.Name, serverIP, p.dnsDomain)

	raw, err := os.ReadFile(kubeconfigSrc)
	if err != nil {
		return "", fmt.Errorf("reading kubeconfig written by k3s at %q: %w", kubeconfigSrc, err)
	}

	if err := os.WriteFile(kubeconfigPath, rewriteKubeconfigServer(raw, serverURL), 0o600); err != nil {
		return "", fmt.Errorf("writing kubeconfig %q: %w", kubeconfigPath, err)
	}

	fmt.Fprintf(logw, "kubeconfig saved to %s\n", kubeconfigPath)

	return serverURL, nil
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

// launchJoinServers brings up the non-bootstrap HA servers (servers[1:]) and returns their
// NodeInfo in launch order. validateClusterConfig has guaranteed an external datastore endpoint
// is set whenever there is more than one server, so each joins simply by running `k3s server`
// against the SAME shared datastore + token — no --cluster-init, no separate join step
// (docs/ADR/0002). They get no kubeconfig mount (the bootstrap server already delivered it), so
// readiness is the host-side TLS dial only (no kubeconfig-file wait). Returns the slice unchanged
// (nil) when there are no additional servers — the single-server path.
func (p *provisioner) launchJoinServers(ctx context.Context, cfg ClusterConfig, servers []NodeConfig, logw io.Writer) ([]NodeInfo, error) {
	var infos []NodeInfo

	for _, s := range servers {
		fmt.Fprintln(logw, "launching k3s server", s.Name, "(HA, shared datastore)")

		info, err := p.launchNode(ctx, cfg, s, "", "")
		if err != nil {
			return nil, err
		}

		if err := p.enableIPForward(ctx, info.ID); err != nil {
			return nil, fmt.Errorf("server %q: %w", s.Name, err)
		}

		if err := p.waitForAPIServer(ctx, info.IPs[0]); err != nil {
			return nil, fmt.Errorf("server %q readiness: %w", s.Name, err)
		}

		infos = append(infos, info)
	}

	return infos, nil
}

// setupDatastore provisions the managed etcd cluster when one is requested, returning one NodeInfo
// per member and the --datastore-endpoint the servers should use. When no managed datastore is
// needed (single server, or a bring-your-own endpoint already set) it returns
// (nil, cfg.DatastoreEndpoint, nil). A managed datastore requires a DNS domain: every member and
// every server reaches the others by FQDN, which is what lets the control plane survive the
// cold-restart IP shift (docs/ADR-0003 — name-bound etcd membership, unlike the IP-bound embedded
// etcd ADR-0002 rejected).
func (p *provisioner) setupDatastore(ctx context.Context, cfg ClusterConfig, logw io.Writer) ([]NodeInfo, string, error) {
	if !cfg.ManageDatastore || cfg.DatastoreEndpoint != "" {
		return nil, cfg.DatastoreEndpoint, nil
	}

	if p.dnsDomain == "" {
		return nil, "", fmt.Errorf("cluster %q: a managed datastore (HA) requires a DNS domain for a stable "+
			"endpoint; set -dns-domain (and run: sudo container system dns create <domain>)", cfg.Name)
	}

	return p.provisionEtcdCluster(ctx, cfg, logw)
}

// provisionEtcdCluster brings up the N-member etcd cluster and returns one NodeInfo per member
// plus the comma-separated k3s --datastore-endpoint the servers should use. Member count defaults
// to defaultEtcdMembers and must be odd ≥3 (validateEtcdMemberCount). Each member's data dir is a
// named volume (labeled for the destroy sweep) with the lost+found-subdir guard.
//
// Ordering matters: ALL members are launched before readiness is asserted, because etcd forms its
// initial quorum only once every peer in --initial-cluster is up. Readiness is a per-member
// host-side TCP dial to the client port — it proves each etcd process bound its socket, not that
// quorum formed (the k3s servers retry their datastore connection until quorum settles). We
// deliberately do NOT add an immediate cross-member quorum assert: the in-VM resolver negative-
// caches a peer's NXDOMAIN for ~30s right after startup, so a strict cross-node health probe here
// would be flaky (ADR-0003 gotcha). The caller must have ensured p.dnsDomain != "".
func (p *provisioner) provisionEtcdCluster(ctx context.Context, cfg ClusterConfig, logw io.Writer) ([]NodeInfo, string, error) {
	count := cfg.DatastoreMembers
	if count == 0 {
		count = defaultEtcdMembers
	}

	if err := validateEtcdMemberCount(count); err != nil {
		return nil, "", fmt.Errorf("cluster %q: %w", cfg.Name, err)
	}

	members := etcdMembers(cfg.Name, count)
	initialCluster := etcdInitialCluster(cfg.Name, p.dnsDomain, count)

	// Create every member's data volume first (with the stale-state guard), so a leftover volume
	// from a prior run is caught before any container launches — same discipline as
	// prepareNodeVolumes. etcd volumes use their own scheme (etcdVolumeName), not nodeVolumeName.
	for _, m := range members {
		vol := etcdVolumeName(m.Name)

		present, err := p.volumeExists(ctx, vol)
		if err != nil {
			return nil, "", fmt.Errorf("checking etcd volume %q: %w", vol, err)
		}

		if present {
			return nil, "", fmt.Errorf("etcd volume %q already exists (stale state from a prior run); "+
				"run -destroy for this cluster first — refusing to reuse it", vol)
		}

		if err := p.volumeCreate(ctx, vol, volumeLabels(cfg.Name)...); err != nil {
			return nil, "", fmt.Errorf("creating etcd volume %q: %w", vol, err)
		}
	}

	// Launch all members, then assert readiness — quorum needs every peer running first.
	infos := make([]NodeInfo, 0, count)

	for _, m := range members {
		id := nodeFQDN(m.Name, p.dnsDomain)

		fmt.Fprintln(logw, "provisioning etcd member", id)

		if _, err := p.run(ctx, buildEtcdRunArgs(cfg, m, p.dnsDomain, initialCluster)...); err != nil {
			return nil, "", fmt.Errorf("launching etcd member %q: %w", id, err)
		}

		addr, err := p.waitForIPv4(ctx, id)
		if err != nil {
			return nil, "", err
		}

		infos = append(infos, NodeInfo{ID: id, Name: m.Name, Role: RoleDatastore, IPs: []netip.Addr{addr}})
	}

	for _, info := range infos {
		if err := p.waitForEtcdMember(ctx, info.IPs[0]); err != nil {
			return nil, "", fmt.Errorf("etcd member %q readiness: %w", info.ID, err)
		}
	}

	return infos, etcdDatastoreEndpoint(cfg.Name, p.dnsDomain, count), nil
}

// setupAPILB provisions the API load balancer when one is requested, returning its NodeInfo.
// It returns (nil, nil) when no LB is needed: a single-server cluster (the server IS the
// endpoint), or a cluster with no DNS domain (the LB is FQDN-only — its --name and the cert SAN
// <cluster>-api.<domain> exist only in FQDN mode). A multi-server IP-only cluster (BYO datastore,
// no -dns-domain) therefore degrades gracefully to pointing at the bootstrap server, matching
// clusterAPIEndpoint's own gate so the kubeconfig never names an LB that was not provisioned.
func (p *provisioner) setupAPILB(ctx context.Context, cfg ClusterConfig, servers []NodeConfig, clusterDir string, logw io.Writer) (*NodeInfo, error) {
	if !cfg.ProvisionAPILB {
		return nil, nil
	}

	if p.dnsDomain == "" {
		fmt.Fprintln(logw, "skipping API load balancer: no DNS domain (the LB is FQDN-addressed); "+
			"the kubeconfig points at the bootstrap server")

		return nil, nil
	}

	info, err := p.provisionAPILB(ctx, cfg, servers, clusterDir, logw)
	if err != nil {
		return nil, err
	}

	return &info, nil
}

// provisionAPILB brings up the API load-balancer micro-VM and returns its NodeInfo. It generates
// the haproxy.cfg (L4 passthrough across the server FQDNs), writes it to a subdir of the cluster
// state dir, bind-mounts that subdir into the haproxy container, then waits for the LB to answer.
// Readiness reuses waitForAPIServer: a TLS handshake to the LB's :6443 is passed straight through
// to a backend apiserver (mode tcp), so a completed handshake proves the LB is forwarding to a
// live server end to end — not just that haproxy bound the port. The caller must have ensured
// p.dnsDomain != "" (the LB is FQDN-only; setupAPILB enforces this).
func (p *provisioner) provisionAPILB(ctx context.Context, cfg ClusterConfig, servers []NodeConfig, clusterDir string, logw io.Writer) (NodeInfo, error) {
	name := apiLBNodeName(cfg.Name)
	id := nodeFQDN(name, p.dnsDomain)

	fmt.Fprintln(logw, "provisioning API load balancer", id)

	configDir, err := writeAPILBConfig(clusterDir, buildAPILBConfig(cfg.Name, servers, p.dnsDomain))
	if err != nil {
		return NodeInfo{}, err
	}

	if _, err := p.run(ctx, buildAPILBRunArgs(cfg, p.dnsDomain, configDir)...); err != nil {
		return NodeInfo{}, fmt.Errorf("launching API load balancer %q: %w", id, err)
	}

	addr, err := p.waitForIPv4(ctx, id)
	if err != nil {
		return NodeInfo{}, err
	}

	if err := p.waitForAPIServer(ctx, addr); err != nil {
		return NodeInfo{}, fmt.Errorf("API load balancer %q readiness: %w", id, err)
	}

	return NodeInfo{ID: id, Name: name, Role: RoleLB, IPs: []netip.Addr{addr}}, nil
}

// writeAPILBConfig writes the generated haproxy.cfg into <clusterDir>/<apiLBConfigSubdir> and
// returns the ABSOLUTE path of that dir, ready to bind-mount into the LB container. Absolute is
// required: `container` resolves a relative bind source against the container root (same reason
// Create makes clusterDir absolute for the kubeconfig mount). The config is regenerated on every
// create, so it is plain host I/O — no named volume, no stale-state guard (the LB is stateless).
func writeAPILBConfig(clusterDir, config string) (string, error) {
	dir, err := filepath.Abs(filepath.Join(clusterDir, apiLBConfigSubdir))
	if err != nil {
		return "", fmt.Errorf("resolving API LB config dir: %w", err)
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("creating API LB config dir %q: %w", dir, err)
	}

	path := filepath.Join(dir, apiLBConfigFileName)
	if err := os.WriteFile(path, []byte(config), 0o644); err != nil { //nolint:gosec // haproxy reads this as a non-root container user; 0644 keeps it readable, it holds no secrets (L4 passthrough config).
		return "", fmt.Errorf("writing API LB config %q: %w", path, err)
	}

	return dir, nil
}

// etcdReadyTimeout bounds the wait for an etcd member to bind its client port. etcd opens
// listen-client-urls early in startup, so a successful dial is a sound "the process is up"
// signal — it does NOT prove quorum (the k3s servers retry their datastore connection until
// quorum settles; see provisionEtcdCluster).
const etcdReadyTimeout = 120 * time.Second

// waitForEtcdMember polls a plain TCP connect against <ip>:2379 until the etcd member answers or
// the timeout elapses. Unlike waitForAPIServer this is a bare TCP dial (no TLS): the readiness
// signal is only that the client port is open. The k3s servers do their own etcd handshake after.
func (p *provisioner) waitForEtcdMember(ctx context.Context, ip netip.Addr) error {
	addr := net.JoinHostPort(ip.String(), strconv.Itoa(etcdClientPort))
	deadline := time.Now().Add(etcdReadyTimeout)
	dialer := &net.Dialer{Timeout: apiDialTimeout}

	var lastErr error
	for {
		conn, err := dialer.DialContext(ctx, "tcp", addr)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		lastErr = err

		if time.Now().After(deadline) {
			return fmt.Errorf("etcd member at %s not reachable within %s: %w", addr, etcdReadyTimeout, lastErr)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(apiPollInterval):
		}
	}
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

// splitRoles partitions nodes into servers and agents, preserving order. servers[0] is the
// bootstrap server (launched first, owns kubeconfig delivery); servers[1:] join the same
// datastore. validateClusterConfig has already guaranteed len(servers) >= 1, so indexing
// servers[0] in Create is safe.
func splitRoles(nodes []NodeConfig) (servers, agents []NodeConfig) {
	for _, n := range nodes {
		if n.Role == RoleServer {
			servers = append(servers, n)
		} else {
			agents = append(agents, n)
		}
	}

	return servers, agents
}

// validateClusterConfig rejects requests that would break bring-up before launching
// anything. The meaningful boundary (BVA, CLAUDE.md k) is the server count, and it is now
// gated on the datastore mode:
//   - 0 servers              : rejected — nothing owns the API.
//   - 1 server               : accepted — embedded sqlite (v0.1.x) or external datastore.
//   - 2+ servers, no endpoint: rejected — multi-server sqlite is impossible (sqlite is a
//     local file; see docs/ADR/0002) and embedded etcd is deliberately disabled (its
//     IP-bound peer membership cannot survive the vmnet DHCP IP shift).
//   - 2+ servers, endpoint set: accepted — HA against the shared external datastore.
//
// So the boundary is B=1 ONLY when DatastoreEndpoint is empty; with an endpoint, any
// servers>=1 is valid. Both sides of B+1 (2 servers) are exercised: rejected without an
// endpoint, accepted with one.
func validateClusterConfig(cfg ClusterConfig) error {
	servers := 0

	for _, n := range cfg.Nodes {
		if n.Role == RoleServer {
			servers++
		}
	}

	switch {
	case servers == 0:
		return fmt.Errorf("cluster %q: at least one server node is required, got 0 (of %d nodes)",
			cfg.Name, len(cfg.Nodes))
	case servers > 1 && cfg.DatastoreEndpoint == "" && !cfg.ManageDatastore:
		return fmt.Errorf("cluster %q: %d servers require an external datastore "+
			"(-datastore-endpoint, or let k3ac provision a managed etcd cluster); multi-server sqlite "+
			"is impossible and embedded etcd is intentionally disabled (see docs/ADR/0002, docs/ADR-0003)",
			cfg.Name, servers)
	}

	// Managed etcd: when an explicit member count is supplied, it must be odd and ≥3. 0 is left
	// to default (defaultEtcdMembers) in provisionEtcdCluster, so it is not rejected here. Skipped
	// for the bring-your-own path (the operator owns their datastore's topology).
	if cfg.ManageDatastore && cfg.DatastoreEndpoint == "" && cfg.DatastoreMembers != 0 {
		if err := validateEtcdMemberCount(cfg.DatastoreMembers); err != nil {
			return fmt.Errorf("cluster %q: %w", cfg.Name, err)
		}
	}

	return nil
}

// resolveManifests turns the requested host manifest paths into absolute paths, confirms
// each exists and is a regular file, and rejects basename collisions. It is the create-time
// guard for the -manifest / auto-deploy feature (v0.4.0): each manifest is bind-mounted into
// the bootstrap server at k3sManifestsDir/<basename>, so two paths sharing a basename would
// collide at the same in-node target and one would silently win. The `container` runtime also
// requires an absolute bind source. Returns the resolved absolute paths in input order.
//
// Boundaries (BVA, CLAUDE.md k): zero manifests is handled by the caller (this is only called
// for len > 0); one manifest passes; many distinct basenames pass; an empty path, a missing
// file, a directory, and a duplicate basename each error.
func resolveManifests(manifests []string) ([]string, error) {
	resolved := make([]string, 0, len(manifests))
	seen := make(map[string]string, len(manifests)) // basename -> first path that claimed it

	for _, m := range manifests {
		if m == "" {
			return nil, fmt.Errorf("manifest path is empty")
		}

		abs, err := filepath.Abs(m)
		if err != nil {
			return nil, fmt.Errorf("resolving manifest path %q: %w", m, err)
		}

		info, err := os.Stat(abs)
		if err != nil {
			return nil, fmt.Errorf("manifest %q: %w", m, err)
		}

		if info.IsDir() {
			return nil, fmt.Errorf("manifest %q is a directory; pass individual manifest files "+
				"(each is mounted at %s/<basename>)", m, k3sManifestsDir)
		}

		base := filepath.Base(abs)
		if other, dup := seen[base]; dup {
			return nil, fmt.Errorf("manifest basename collision: %q and %q both mount to %s/%s — "+
				"rename one so each manifest has a distinct filename", other, m, k3sManifestsDir, base)
		}

		seen[base] = m

		resolved = append(resolved, abs)
	}

	return resolved, nil
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
