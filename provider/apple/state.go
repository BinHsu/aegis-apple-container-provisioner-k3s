// SPDX-License-Identifier: MIT

package apple

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
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

// ClusterRef builds a minimal ClusterState for a name-only, label-sweep teardown. It
// carries just the cluster name and state dir, so Destroy's recorded-node pass is a no-op
// (no Nodes) and the label sweep (k3s.cluster.name=<name>) reclaims any orphaned
// containers/volumes by label alone. Mirrors the Talos sibling's apple.ClusterRef.
func ClusterRef(clusterName, stateDir string) ClusterState {
	return ClusterState{ClusterName: clusterName, StateDir: stateDir}
}

// LoadStateForDestroy loads a cluster's saved state for teardown, tolerating a missing
// state.json. A Create that fails before saveState (e.g. the readiness probe timing out)
// leaves running containers + named volumes but no state.json; aborting here would orphan
// them. When state.json does not exist, this returns a name-only ClusterRef and
// sweptByLabel=true so Destroy falls back to the label sweep. Any other read/parse error
// (a real I/O fault, a corrupt file) still surfaces — only fs.ErrNotExist is tolerated.
// Mirrors the Talos sibling's runDestroy fs.ErrNotExist handling.
func LoadStateForDestroy(stateDir, clusterName string) (state ClusterState, sweptByLabel bool, err error) {
	state, err = LoadState(stateDir, clusterName)

	switch {
	case err == nil:
		return state, false, nil
	case errors.Is(err, fs.ErrNotExist):
		return ClusterRef(clusterName, stateDir), true, nil
	default:
		return ClusterState{}, false, err
	}
}
