// SPDX-License-Identifier: MIT

package apple

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// ClusterSummary is one row of the `-list` table: a cluster's recorded name, its server
// and agent counts, the API endpoint, and the resolved k3s image. It is derived purely
// from a cluster's saved state.json (no `container` CLI calls), so listing works offline
// and never depends on the runtime daemon being up.
type ClusterSummary struct {
	Name      string
	Servers   int
	Agents    int
	ServerURL string
	Image     string
}

// summarizeState reduces a loaded ClusterState to a ClusterSummary, counting nodes by role.
// Pure (no I/O) so the role-counting is unit-testable. The managed datastore node
// (RoleDatastore) is neither a server nor an agent, so it is excluded from both counts —
// the table reports k3s nodes only.
func summarizeState(state ClusterState) ClusterSummary {
	var servers, agents int

	for _, n := range state.Nodes {
		switch n.Role {
		case RoleServer:
			servers++
		case RoleAgent:
			agents++
		case RoleDatastore:
			// not a k3s node; excluded from both counts.
		}
	}

	return ClusterSummary{
		Name:      state.ClusterName,
		Servers:   servers,
		Agents:    agents,
		ServerURL: state.ServerURL,
		Image:     state.Image,
	}
}

// ListClusters scans stateDir for per-cluster state.json files and returns one summary per
// cluster, sorted by name. The layout mirrors saveState: <stateDir>/<cluster>/state.json. A
// subdirectory without a readable state.json is skipped (e.g. a cluster mid-create, or a
// stray directory), so a single bad entry never breaks the listing. A missing stateDir is
// treated as "no clusters" (nil, nil), not an error — listing before any create is valid.
// Exported because the cmd driver lives in a separate package.
func ListClusters(stateDir string) ([]ClusterSummary, error) {
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil // no state dir yet == no clusters
		}

		return nil, fmt.Errorf("reading state dir %q: %w", stateDir, err)
	}

	var summaries []ClusterSummary

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}

		// Each cluster's state lives at <stateDir>/<name>/state.json. A directory without
		// one (or with an unreadable/corrupt one) is skipped rather than failing the whole
		// listing — a half-created or unrelated directory must not mask the real clusters.
		if _, err := os.Stat(filepath.Join(stateDir, e.Name(), "state.json")); err != nil {
			continue
		}

		state, err := LoadState(stateDir, e.Name())
		if err != nil {
			continue
		}

		summaries = append(summaries, summarizeState(state))
	}

	sort.Slice(summaries, func(i, j int) bool { return summaries[i].Name < summaries[j].Name })

	return summaries, nil
}
