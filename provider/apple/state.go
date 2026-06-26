// SPDX-License-Identifier: MIT

package apple

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// statePath returns the path to a cluster's saved state.json.
func statePath(stateDir, clusterName string) string {
	return filepath.Join(stateDir, clusterName, "state.json")
}

// saveState persists cluster state to <statedir>/<name>/state.json. The Talos sibling
// got this from provision.State (state.yaml); k3s has no framework, so we write plain
// JSON. Destroy reads it back to find node IDs without depending on `container ls`
// label filtering (which the CLI does not support — Talos sibling finding).
func saveState(state ClusterState) error {
	dir := filepath.Join(state.StateDir, state.ClusterName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating state dir %q: %w", dir, err)
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling state: %w", err)
	}

	path := statePath(state.StateDir, state.ClusterName)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("writing state %q: %w", path, err)
	}

	return nil
}

// LoadState reads a cluster's saved state for teardown. This is the k3s equivalent of
// the Talos sibling's provisioner.Reflect — reconstruct the cluster from disk so
// Destroy can run without the original ClusterConfig in hand. Exported because the cmd
// driver lives in a separate package.
func LoadState(stateDir, clusterName string) (ClusterState, error) {
	path := statePath(stateDir, clusterName)

	data, err := os.ReadFile(path)
	if err != nil {
		return ClusterState{}, fmt.Errorf("reading state %q: %w", path, err)
	}

	var state ClusterState
	if err := json.Unmarshal(data, &state); err != nil {
		return ClusterState{}, fmt.Errorf("parsing state %q: %w", path, err)
	}

	return state, nil
}
