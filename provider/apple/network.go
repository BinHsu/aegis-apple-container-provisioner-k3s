// SPDX-License-Identifier: MIT

package apple

import (
	"context"
	"strings"
)

// defaultNetwork is apple/container's built-in network; it always exists and needs no
// creation. Carried over verbatim from the Talos sibling.
const defaultNetwork = "default"

// ensureNetwork creates the cluster network if it does not already exist. The built-in
// "default" network is used as-is.
//
// Note: the custom-subnet path carries over the Talos G5 finding that vmnet honors
// --subnet; not independently re-verified on k3s. The default network path (name == ""
// or name == defaultNetwork) does not exercise this.
func (p *provisioner) ensureNetwork(ctx context.Context, name, subnet string) error {
	if name == "" || name == defaultNetwork {
		return nil
	}

	args := []string{"network", "create"}
	if subnet != "" {
		args = append(args, "--subnet", subnet)
	}

	args = append(args, name)

	_, err := p.run(ctx, args...)
	if err != nil && strings.Contains(err.Error(), "already") {
		return nil // idempotent: re-use an existing network
	}

	return err
}

// destroyNetwork removes the cluster network, ignoring the built-in default and "not found".
func (p *provisioner) destroyNetwork(ctx context.Context, name string) error {
	if name == "" || name == defaultNetwork {
		return nil
	}

	_, err := p.run(ctx, "network", "delete", name)
	if err != nil && strings.Contains(err.Error(), "not found") {
		return nil
	}

	return err
}
