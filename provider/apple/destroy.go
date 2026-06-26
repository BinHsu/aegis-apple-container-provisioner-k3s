// SPDX-License-Identifier: MIT

package apple

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Destroy tears down a cluster from its saved state. Idempotent (stop/remove ignore
// "not found"), mirroring the Talos sibling.
//
// IMPORTANT k3s-specific note: the per-node datastore lives in a host BIND-MOUNT
// (<statedir>/<name>/<node>/k3s), NOT in tmpfs. So `container stop`/`rm` alone does NOT
// erase cluster state — the sqlite datastore persists on the host. That persistence is
// the whole point during normal stop/start (G3/G5), but it means a "clean" destroy MUST
// also remove the host state dir. Removing the entire <statedir>/<name> tree is what
// distinguishes a true teardown from a stop-for-restart: leave the dir and the next
// Create with the same name would resurrect the old datastore.
func (p *provisioner) Destroy(ctx context.Context, state ClusterState, logw io.Writer) error {
	if logw == nil {
		logw = io.Discard
	}

	var errs []error

	for _, node := range state.Nodes {
		fmt.Fprintln(logw, "destroying node", node.Name)

		if err := p.stop(ctx, node.ID); err != nil {
			errs = append(errs, err)
		}

		if err := p.remove(ctx, node.ID); err != nil {
			errs = append(errs, err)
		}
	}

	if err := p.destroyNetwork(ctx, state.Network); err != nil {
		errs = append(errs, err)
	}

	// Remove the host state tree, including the bind-mounted datastore dirs. This is the
	// step that makes destroy truly clean vs. leaving state for a restart (see doc above).
	if state.StateDir != "" && state.ClusterName != "" {
		clusterDir := filepath.Join(state.StateDir, state.ClusterName)
		if err := os.RemoveAll(clusterDir); err != nil {
			errs = append(errs, fmt.Errorf("removing state dir %q: %w", clusterDir, err))
		}
	}

	return errors.Join(errs...)
}
