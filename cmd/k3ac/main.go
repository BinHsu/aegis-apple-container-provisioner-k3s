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
		agentCount  = flag.Int("agents", 1, "number of agent (worker) nodes")
		destroy     = flag.Bool("destroy", false, "destroy the named cluster instead of creating it")
	)

	flag.Parse()

	ctx := context.Background()

	prov, err := apple.NewProvisioner(ctx, apple.Config{DNSDomain: *dnsDomain})
	if err != nil {
		return err
	}

	defer prov.Close() //nolint:errcheck

	if *destroy {
		// Tolerate a missing state.json: a Create that failed before saveState (e.g. the
		// old readiness-probe timeout) leaves a running container + named volume but no
		// state.json. Aborting here would orphan them. LoadStateForDestroy falls back to a
		// name-only ClusterRef so Destroy reclaims them via the label sweep
		// (k3s.cluster.name=<name>); other (non-not-exist) load errors still surface.
		state, sweptByLabel, err := apple.LoadStateForDestroy(*stateDir, *clusterName)
		if err != nil {
			return fmt.Errorf("loading state for cluster %q: %w", *clusterName, err)
		}

		if sweptByLabel {
			fmt.Printf("no state.json for cluster %q; sweeping by label k3s.cluster.name=%s\n", *clusterName, *clusterName)
		}

		if err := prov.Destroy(ctx, state, log.Writer()); err != nil {
			return fmt.Errorf("destroying cluster %q: %w", *clusterName, err)
		}

		fmt.Printf("destroyed cluster %q\n", *clusterName)

		return nil
	}

	// NOTE: exactly one server is supported (sqlite single-server; see the launcher's
	// validateClusterConfig). The driver hard-codes one server and N agents.
	nodes := []apple.NodeConfig{
		{
			Name:     fmt.Sprintf("%s-server-1", *clusterName),
			Role:     apple.RoleServer,
			Memory:   *serverMemMB * mib,
			NanoCPUs: 2e9,
		},
	}

	for i := range *agentCount {
		nodes = append(nodes, apple.NodeConfig{
			Name:     fmt.Sprintf("%s-agent-%d", *clusterName, i+1),
			Role:     apple.RoleAgent,
			Memory:   *agentMemMB * mib,
			NanoCPUs: 2e9,
		})
	}

	cfg := apple.ClusterConfig{
		Name:     *clusterName,
		Image:    *k3sImage,
		Network:  *network,
		StateDir: *stateDir,
		Token:    *token,
		Nodes:    nodes,
	}

	state, err := prov.Create(ctx, cfg, log.Writer())
	if err != nil {
		return err
	}

	reportProvisioned(state, *dnsDomain)

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
