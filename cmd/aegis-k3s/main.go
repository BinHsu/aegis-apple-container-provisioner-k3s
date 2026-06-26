// SPDX-License-Identifier: MIT

// Command aegis-k3s is a thin driver that boots a k3s cluster on Apple's `container`
// runtime via the apple launcher. Unlike the Talos sibling (which mirrors what
// `talosctl cluster create` does because Talos has a provisioner framework), k3s has
// NO upstream provisioner interface — so this driver IS the entry point, not a precursor
// to an upstream subcommand. It builds a ClusterConfig and calls Create / Destroy.
//
// SPIKE DRAFT — NOT verified to run. See docs/VERIFICATION.md for the open G-gates.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"

	"github.com/BinHsu/aegis-apple-container-provisioner-k3s/provider/apple"
)

const mib = 1024 * 1024

func main() {
	if err := run(); err != nil {
		log.Fatalf("aegis-k3s: %v", err)
	}
}

func run() error {
	var (
		clusterName = flag.String("name", "aegis", "cluster name")
		k3sImage    = flag.String("image", "", "rancher/k3s node image (empty = pinned default)")
		stateDir    = flag.String("state-dir", "_out/clusters", "cluster state directory (also holds datastore bind-mounts)")
		network     = flag.String("network", "default", "apple/container network name (default = built-in vmnet)")
		clusterDNS  = flag.String("cluster-dns", "", "stable name for the API server cert SAN (empty = default)")
		token       = flag.String("token", "", "K3S_TOKEN (empty = generate with crypto/rand)")
		serverMemMB = flag.Int64("server-memory", 2048, "server memory (MB)")
		agentMemMB  = flag.Int64("agent-memory", 2048, "agent memory (MB)")
		agentCount  = flag.Int("agents", 1, "number of agent (worker) nodes")
		destroy     = flag.Bool("destroy", false, "destroy the named cluster instead of creating it")
	)

	flag.Parse()

	ctx := context.Background()

	prov, err := apple.NewProvisioner(ctx)
	if err != nil {
		return err
	}

	defer prov.Close() //nolint:errcheck

	if *destroy {
		state, err := apple.LoadState(*stateDir, *clusterName)
		if err != nil {
			return fmt.Errorf("loading state for cluster %q: %w", *clusterName, err)
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
		Name:       *clusterName,
		Image:      *k3sImage,
		Network:    *network,
		StateDir:   *stateDir,
		Token:      *token,
		ClusterDNS: *clusterDNS,
		Nodes:      nodes,
	}

	state, err := prov.Create(ctx, cfg, log.Writer())
	if err != nil {
		return err
	}

	fmt.Println("\n=== k3s cluster provisioned (SPIKE — unverified) ===")

	for _, n := range state.Nodes {
		if len(n.IPs) > 0 {
			fmt.Printf("  %-28s %-8s %s\n", n.Name, n.Role, n.IPs[0])
		}
	}

	fmt.Printf("\nserver URL: %s\n", state.ServerURL)
	fmt.Println("\nnext steps (operator):")
	fmt.Printf("  container exec %s-server-1 cat /etc/rancher/k3s/k3s.yaml > kubeconfig\n", *clusterName)
	fmt.Printf("  # then rewrite the kubeconfig server: field to %s and:\n", state.ServerURL)
	fmt.Printf("  KUBECONFIG=./kubeconfig kubectl get nodes\n")

	return nil
}
