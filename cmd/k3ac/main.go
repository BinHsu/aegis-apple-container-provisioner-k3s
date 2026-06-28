// SPDX-License-Identifier: MIT

// Command k3ac (k3s on Apple container) is a thin driver that boots a k3s cluster on Apple's `container`
// runtime via the apple launcher. Unlike the Talos sibling (which mirrors what
// `talosctl cluster create` does because Talos has a provisioner framework), k3s has
// NO upstream provisioner interface — so this driver IS the entry point, not a precursor
// to an upstream subcommand. It builds a ClusterConfig and calls Create / Destroy.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"path/filepath"

	"github.com/BinHsu/aegis-apple-container-provisioner-k3s/provider/apple"
)

const mib = 1024 * 1024

func main() {
	if err := run(); err != nil {
		log.Fatalf("k3ac: %v", err)
	}
}

func run() error {
	var (
		// -config feeds a declarative JSON cluster spec; explicit flags always override it.
		// Precedence: built-in defaults < config file < explicit flags (via flag.Visit).
		configPath  = flag.String("config", "", "load cluster settings from a JSON file (explicit flags override file values)")
		clusterName = flag.String("name", "aegis", "cluster name")
		k3sImage    = flag.String("image", "", "rancher/k3s node image (empty = pinned default)")
		stateDir    = flag.String("state-dir", "_out/clusters", "cluster state directory (also holds state.json)")
		network     = flag.String("network", "default", "apple/container network name (default = built-in vmnet)")
		dnsDomain   = flag.String("dns-domain", "aegis", "Apple container DNS domain for stable FQDN node names "+
			"(<node>.<domain>); set to \"\" to disable FQDN naming and fall back to IP-only. "+
			"Prerequisite: sudo container system dns create <domain> (must re-run after macOS reboot).")
		token       = flag.String("token", "", "K3S_TOKEN (empty = generate with crypto/rand)")
		serverMemMB = flag.Int64("server-memory", 2048, "server memory (MB)")
		agentMemMB  = flag.Int64("agent-memory", 2048, "agent memory (MB)")
		serverCount = flag.Int("servers", 1, "number of server (control-plane) nodes (HA, see docs/ADR-0003). "+
			">1 with no -datastore-endpoint makes k3ac provision a managed etcd cluster (needs -dns-domain).")
		datastore = flag.String("datastore-endpoint", "", "bring-your-own external k3s datastore for HA, e.g. "+
			"postgres://user:pass@db.aegis:5432/kine. Empty + -servers>1 = k3ac provisions a managed etcd cluster; "+
			"empty + -servers=1 = embedded sqlite.")
		datastoreMembers = flag.Int("datastore-members", 3, "managed etcd cluster size for the auto-provisioned HA "+
			"datastore; MUST be odd and >=3 (3 or 5). Ignored with a bring-your-own -datastore-endpoint.")
		datastoreImage  = flag.String("datastore-image", "", "managed etcd member image (empty = pinned default). Managed-etcd path only.")
		datastoreMemMB  = flag.Int64("datastore-memory", 512, "managed etcd member memory (MB). Managed-etcd path only.")
		agentCount      = flag.Int("agents", 1, "number of agent (worker) nodes")
		serverCPUs      = flag.Int("server-cpus", 2, "vCPUs per server node")
		agentCPUs       = flag.Int("agent-cpus", 2, "vCPUs per agent node")
		destroy         = flag.Bool("destroy", false, "destroy the named cluster instead of creating it")
		addAgents       = flag.Int("add-agents", 0, "add N agent nodes to an existing cluster")
		addServer       = flag.Bool("add-server", false, "add one control-plane server to an existing HA cluster and update the API LB")
		removeNode      = flag.String("remove-node", "", "remove a node (by name) from an existing cluster")
		mergeKubeconfig = flag.String("merge-kubeconfig", "", "merge the named cluster's kubeconfig into ~/.kube/config (via kubectl) and set-context, then exit")
		list            = flag.Bool("list", false, "list clusters found under -state-dir (name, server/agent counts, URL, image) and exit")
		startName       = flag.String("start", "", "start every node of an existing cluster (datastore -> servers -> agents)")
		stopName        = flag.String("stop", "", "stop every node of an existing cluster (agents -> servers -> datastore)")

		// Repeated flags (pass each more than once). k3s-arg passthrough opens the closed box:
		// the verbatim args land after every built-in k3s flag, so they can disable traefik,
		// change the CNI/flannel backend, set --cluster-cidr, enable ServiceLB, etc.
		serverArgs stringList
		agentArgs  stringList
		nodeLabels stringList
		manifests  stringList
		envVars    stringList
	)

	flag.Var(&serverArgs, "k3s-server-arg", "extra k3s flag appended verbatim to every server (repeatable), e.g. --disable=traefik")
	flag.Var(&agentArgs, "k3s-agent-arg", "extra k3s flag appended verbatim to every agent (repeatable)")
	flag.Var(&nodeLabels, "node-label", "k3s node label KEY=VALUE applied to every node at create (repeatable)")
	flag.Var(&manifests, "manifest", "host-side manifest file auto-deployed via the bootstrap server (repeatable)")
	flag.Var(&envVars, "env", "environment variable KEY=VALUE injected into every k3s node (repeatable)")

	flag.Parse()

	// Apply config-file values for any flag the user did NOT set explicitly (extracted to
	// loadAndApplyConfig to keep run within the funlen budget).
	if *configPath != "" {
		if err := loadAndApplyConfig(*configPath, flagRefs{
			clusterName: clusterName, image: k3sImage, stateDir: stateDir, network: network,
			dnsDomain: dnsDomain, token: token, datastore: datastore,
			serverMemMB: serverMemMB, agentMemMB: agentMemMB,
			serverCount: serverCount, agentCount: agentCount,
			datastoreMembers: datastoreMembers, datastoreImage: datastoreImage, datastoreMemMB: datastoreMemMB,
			serverCPUs: serverCPUs, agentCPUs: agentCPUs,
			serverArgs: &serverArgs, agentArgs: &agentArgs,
			nodeLabels: &nodeLabels, manifests: &manifests, envVars: &envVars,
		}); err != nil {
			return err
		}
	}

	// -list is pure file I/O over -state-dir (no `container` daemon needed), so handle it
	// before constructing the provisioner and exit.
	if *list {
		return listClusters(*stateDir)
	}

	ctx := context.Background()

	// -merge-kubeconfig is kubectl + host file I/O only (no `container` daemon), so handle it
	// before constructing the provisioner and exit. MergeKubeconfig already returns descriptive
	// errors, so return it directly.
	if *mergeKubeconfig != "" {
		return apple.MergeKubeconfig(ctx, *stateDir, *mergeKubeconfig, log.Writer())
	}

	prov, err := apple.NewProvisioner(ctx, apple.Config{DNSDomain: *dnsDomain})
	if err != nil {
		return err
	}

	defer prov.Close() //nolint:errcheck

	// Lifecycle/membership ops on an existing cluster take precedence over create. -stop before
	// -start so a single invocation does exactly one thing if both are set.
	if *stopName != "" {
		if err := prov.Stop(ctx, *stateDir, *stopName, log.Writer()); err != nil {
			return fmt.Errorf("stopping cluster %q: %w", *stopName, err)
		}

		fmt.Printf("stopped cluster %q\n", *stopName)

		return nil
	}

	if *startName != "" {
		if err := prov.Start(ctx, *stateDir, *startName, log.Writer()); err != nil {
			return fmt.Errorf("starting cluster %q: %w", *startName, err)
		}

		fmt.Printf("started cluster %q\n", *startName)

		return nil
	}

	if *destroy {
		return runDestroy(ctx, prov, *stateDir, *clusterName)
	}

	// Membership-change modes (remove wins if several are set).
	if *removeNode != "" {
		if err := prov.RemoveNode(ctx, *stateDir, *clusterName, *removeNode, log.Writer()); err != nil {
			return fmt.Errorf("removing node %q from cluster %q: %w", *removeNode, *clusterName, err)
		}

		fmt.Printf("removed node %q from cluster %q\n", *removeNode, *clusterName)

		return nil
	}

	if *addAgents > 0 {
		// Reuse the create-path per-agent memory + vCPU. NanoCPUs is vCPUs * 1e9. v0.5.0 also
		// threads -node-label and -k3s-agent-arg so a post-create agent is configured like a
		// create-time one.
		state, err := prov.AddAgents(ctx, *stateDir, *clusterName, *addAgents, *agentMemMB*mib, vcpuToNano(*agentCPUs), nodeLabels, agentArgs, log.Writer())
		if err != nil {
			return err
		}

		reportProvisioned(state, *dnsDomain)

		return nil
	}

	if *addServer {
		// Add one control-plane server against the existing datastore and update the API LB. Reuses
		// the create-path per-SERVER memory + vCPU, plus -node-label / -k3s-server-arg.
		state, err := prov.AddServer(ctx, *stateDir, *clusterName, *serverMemMB*mib, vcpuToNano(*serverCPUs), nodeLabels, serverArgs, log.Writer())
		if err != nil {
			return err
		}

		reportProvisioned(state, *dnsDomain)

		return nil
	}

	// Default mode: create the cluster. The node set + ClusterConfig assembly and the Create call
	// are extracted to runCreate so run() stays within the funlen budget.
	return runCreate(ctx, prov, clusterConfigInputs{
		name: *clusterName, image: *k3sImage, network: *network, stateDir: *stateDir,
		token: *token, datastore: *datastore,
		serverCount: *serverCount, datastoreMembers: *datastoreMembers,
		datastoreImage: *datastoreImage, datastoreMemMB: *datastoreMemMB,
		manifests: manifests, envVars: envVars,
	}, nodeSpec{
		clusterName: *clusterName,
		serverCount: *serverCount, agentCount: *agentCount,
		serverCPUs: *serverCPUs, agentCPUs: *agentCPUs,
		serverMemMB: *serverMemMB, agentMemMB: *agentMemMB,
		labels: nodeLabels, serverArgs: serverArgs, agentArgs: agentArgs,
	}, *dnsDomain)
}

// loadAndApplyConfig loads the -config JSON file and applies its values to any flag the user did NOT
// set explicitly. flag.Visit reports only the flags actually passed, which is the "explicit" signal
// applyConfig needs (precedence: defaults < file < explicit flags). Extracted from run() to keep it
// within the funlen budget.
func loadAndApplyConfig(path string, r flagRefs) error {
	fc, err := loadFileConfig(path)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	explicit := make(map[string]bool)
	flag.Visit(func(f *flag.Flag) { explicit[f.Name] = true })

	applyConfig(fc, explicit, r)

	return nil
}

// clusterDestroyer is the destroy capability runDestroy needs (the concrete *provisioner is
// unexported, so it is passed through this interface).
type clusterDestroyer interface {
	Destroy(context.Context, apple.ClusterState, io.Writer) error
}

// runDestroy tears down the named cluster. It tolerates a missing state.json: a Create that failed
// before saveState leaves a running container + named volume but no state.json, so LoadStateForDestroy
// falls back to a name-only ClusterRef and Destroy reclaims orphans via the label sweep
// (k3s.cluster.name=<name>). Other (non-not-exist) load errors still surface. Extracted from run() to
// keep it within the funlen budget.
func runDestroy(ctx context.Context, prov clusterDestroyer, stateDir, clusterName string) error {
	state, sweptByLabel, err := apple.LoadStateForDestroy(stateDir, clusterName)
	if err != nil {
		return fmt.Errorf("loading state for cluster %q: %w", clusterName, err)
	}

	if sweptByLabel {
		fmt.Printf("no state.json for cluster %q; sweeping by label k3s.cluster.name=%s\n", clusterName, clusterName)
	}

	if err := prov.Destroy(ctx, state, log.Writer()); err != nil {
		return fmt.Errorf("destroying cluster %q: %w", clusterName, err)
	}

	fmt.Printf("destroyed cluster %q\n", clusterName)

	return nil
}

// k3sCreator is the create capability runCreate needs from the provisioner. The concrete type
// (apple's *provisioner) is unexported, so run passes it through this interface — that is what lets
// the create path be extracted out of run() into a separate function.
type k3sCreator interface {
	Create(context.Context, apple.ClusterConfig, io.Writer) (apple.ClusterState, error)
}

// runCreate builds the create-time node set (N servers then N agents, with per-node tunables) and
// ClusterConfig from the resolved flags, then creates the cluster and reports it. validateClusterConfig
// (in the provider) is the single source of truth for the >1-server datastore requirement.
func runCreate(ctx context.Context, prov k3sCreator, in clusterConfigInputs, ns nodeSpec, dnsDomain string) error {
	cfg := buildClusterConfig(in, buildNodes(ns))

	state, err := prov.Create(ctx, cfg, log.Writer())
	if err != nil {
		return err
	}

	reportProvisioned(state, dnsDomain)

	return nil
}

// clusterConfigInputs are the create-time scalars buildClusterConfig folds into an
// apple.ClusterConfig, grouped into a struct (same readability reasoning as nodeSpec/flagRefs)
// so run stays short.
type clusterConfigInputs struct {
	name, image, network, stateDir, token, datastore string
	serverCount, datastoreMembers                    int
	datastoreImage                                   string
	datastoreMemMB                                   int64
	manifests                                        []string
	envVars                                          []string
}

// buildClusterConfig assembles the provider ClusterConfig from the resolved flags and node set.
// ManageDatastore and ProvisionAPILB are INFERRED from server count (more than one server, no
// bring-your-own datastore) — k3ac provisions a managed etcd cluster (docs/ADR-0003) behind a
// single API-LB front door (<cluster>-api.<domain>, docs/ADR/0002, v0.3.0). With an explicit
// -datastore-endpoint the operator owns the datastore (BYO). The provider validates the rest
// (managed HA needs -dns-domain; etcd member count must be odd >=3; setupAPILB skips the LB
// gracefully without a DNS domain). The -config JSON path feeds serverCount, so HA-from-config
// gets both the managed datastore and the LB.
func buildClusterConfig(in clusterConfigInputs, nodes []apple.NodeConfig) apple.ClusterConfig {
	return apple.ClusterConfig{
		Name:                 in.name,
		Image:                in.image,
		Network:              in.network,
		StateDir:             in.stateDir,
		Token:                in.token,
		DatastoreEndpoint:    in.datastore,
		ManageDatastore:      in.serverCount > 1 && in.datastore == "",
		DatastoreMembers:     in.datastoreMembers,
		DatastoreImage:       in.datastoreImage,
		DatastoreMemoryBytes: in.datastoreMemMB * mib,
		ProvisionAPILB:       in.serverCount > 1,
		EnvVars:              in.envVars,
		Manifests:            in.manifests,
		Nodes:                nodes,
	}
}

// nodeSpec is the create-time node-set request, grouped into a struct so buildNodes takes one
// parameter instead of a ten-argument positional list (the same readability/footgun reasoning
// as flagRefs).
type nodeSpec struct {
	clusterName             string
	serverCount, agentCount int
	serverCPUs, agentCPUs   int
	serverMemMB, agentMemMB int64
	labels                  []string
	serverArgs, agentArgs   []string
}

// buildNodes assembles the create-time node set: serverCount servers then agentCount agents,
// each with its per-role memory, vCPU, node labels, and verbatim k3s args (v0.4.0). Extracted
// from run so the create path stays within the funlen budget and is independently testable.
func buildNodes(s nodeSpec) []apple.NodeConfig {
	var nodes []apple.NodeConfig

	for i := range s.serverCount {
		nodes = append(nodes, apple.NodeConfig{
			Name:      fmt.Sprintf("%s-server-%d", s.clusterName, i+1),
			Role:      apple.RoleServer,
			Memory:    s.serverMemMB * mib,
			NanoCPUs:  vcpuToNano(s.serverCPUs),
			Labels:    s.labels,
			ExtraArgs: s.serverArgs,
		})
	}

	for i := range s.agentCount {
		nodes = append(nodes, apple.NodeConfig{
			Name:      fmt.Sprintf("%s-agent-%d", s.clusterName, i+1),
			Role:      apple.RoleAgent,
			Memory:    s.agentMemMB * mib,
			NanoCPUs:  vcpuToNano(s.agentCPUs),
			Labels:    s.labels,
			ExtraArgs: s.agentArgs,
		})
	}

	return nodes
}

// vcpuToNano converts a whole-vCPU count to the nano-CPU unit NodeConfig.NanoCPUs uses
// (1 vCPU == 1e9 nano-CPUs). buildRunArgs divides back down to whole CPUs for `--cpus`.
func vcpuToNano(vcpus int) int64 { return int64(vcpus) * 1e9 }

// listClusters prints the -list table: one row per cluster found under stateDir, derived
// purely from each cluster's state.json. The empty case is reported plainly rather than as an
// error — no clusters is a valid state (e.g. before the first create).
func listClusters(stateDir string) error {
	clusters, err := apple.ListClusters(stateDir)
	if err != nil {
		return err
	}

	if len(clusters) == 0 {
		fmt.Printf("no clusters found under %s\n", stateDir)

		return nil
	}

	fmt.Printf("%-20s %7s %6s  %-40s %s\n", "NAME", "SERVERS", "AGENTS", "SERVER URL", "IMAGE")

	for _, c := range clusters {
		fmt.Printf("%-20s %7d %6d  %-40s %s\n", c.Name, c.Servers, c.Agents, c.ServerURL, c.Image)
	}

	return nil
}

// reportProvisioned prints the provisioned nodes and the operator's next steps.
func reportProvisioned(state apple.ClusterState, dnsDomain string) {
	fmt.Println("\n=== k3s cluster provisioned ===")

	for _, n := range state.Nodes {
		if len(n.IPs) > 0 {
			fmt.Printf("  %-28s %-8s %s\n", n.Name, n.Role, n.IPs[0])
		}
	}

	fmt.Printf("\nserver URL: %s\n", state.ServerURL)

	// The provisioner wrote a ready-to-use kubeconfig during create (server URL already
	// rewritten from 127.0.0.1 to the FQDN/IP). Point the operator straight at it.
	kubeconfigPath := filepath.Join(state.StateDir, state.ClusterName, "kubeconfig")

	fmt.Println("\nnext steps (operator):")
	fmt.Println("  # The provisioner wrote a ready-to-use kubeconfig (server URL already rewritten):")
	fmt.Printf("  export KUBECONFIG=%s\n", kubeconfigPath)
	fmt.Println("  kubectl get nodes")

	if dnsDomain != "" {
		fmt.Println("  # The kubeconfig points at the FQDN endpoint — it survives cold-restart IP changes.")
		fmt.Println("  # (valid: --tls-san covered the FQDN when the API server cert was issued at create time)")
	} else {
		fmt.Printf("  # IP-only mode: the endpoint is %s (current DHCP address).\n", state.ServerURL)
		fmt.Println("  # Note: the IP may change after a cold restart (no FQDN in IP-only mode).")
	}
}
