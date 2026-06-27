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

// Destroy tears down a cluster from its saved state. Idempotent (stop/remove and
// volume deletes ignore "not found"), mirroring the Talos sibling.
//
// Teardown runs in two passes, both idempotent:
//
//  1. Recorded-state pass: node IDs/names come from the cluster's recorded state
//     (ClusterState.Nodes), and each node's k3s datastore NAMED VOLUME is deleted by
//     its derived name. Deleting this volume is mandatory — skipping it would leave old
//     sqlite data behind, so recreating a same-named cluster would hit the stale-state
//     guard (or boot onto stale state). This pass is a no-op when Nodes is empty.
//
//  2. Label sweep: containers and volumes are also listed by the cluster label
//     (k3s.cluster.name=<name>) and stopped/removed/deleted. The CLI has no native
//     label filter, so the sweep lists --format json and matches client-side (see
//     listContainersByLabel / listVolumesByLabel). This pass closes the half-created-
//     cluster gap: a Create that FAILED before saveState leaves orphaned
//     containers/volumes but no recorded node list, and the sweep reclaims them from
//     the labels alone.
//
// Finally the per-cluster state dir (containing state.json) is removed. Named volumes
// are container-managed (not under stateDir), so they are deleted by the two passes
// above, not here.
func (p *provisioner) Destroy(ctx context.Context, state ClusterState, logw io.Writer) error {
	if logw == nil {
		logw = io.Discard
	}

	var errs []error

	errs = append(errs, p.destroyRecordedNodes(ctx, state, logw)...)
	errs = append(errs, p.sweepByLabel(ctx, state.ClusterName, logw)...)

	if err := p.destroyNetwork(ctx, state.Network); err != nil {
		errs = append(errs, err)
	}

	// Remove the per-cluster state dir (state.json). Named volumes are container-managed
	// and deleted above; this removes only the provisioner's own JSON state file.
	if state.StateDir != "" && state.ClusterName != "" {
		clusterDir := filepath.Join(state.StateDir, state.ClusterName)
		if err := os.RemoveAll(clusterDir); err != nil {
			errs = append(errs, fmt.Errorf("removing state dir %q: %w", clusterDir, err))
		}
	}

	return errors.Join(errs...)
}

// destroyRecordedNodes is the recorded-state teardown pass: for each node in
// ClusterState.Nodes it stops and removes the container (using node.ID, which is the
// FQDN when dns-domain was set at create time), then deletes the node's k3s datastore
// named volume (name derived from the same nodeVolumeName Create used). Every step
// ignores "not found", so it is idempotent and a no-op when Nodes is empty. It
// collects errors rather than aborting, so one stuck node does not block teardown of
// the rest.
func (p *provisioner) destroyRecordedNodes(ctx context.Context, state ClusterState, log io.Writer) []error {
	var errs []error

	for _, node := range state.Nodes {
		fmt.Fprintln(log, "destroying node", node.Name)

		if err := p.stop(ctx, node.ID); err != nil {
			errs = append(errs, err)
		}

		if err := p.remove(ctx, node.ID); err != nil {
			errs = append(errs, err)
		}

		// The managed datastore node uses its own volume scheme (<cluster>-db-pg) and mount,
		// not the k3s nodeVolumeName scheme — pick the right name by role so this pass deletes
		// it explicitly rather than relying on the label sweep alone.
		vol := nodeVolumeName(state.ClusterName, node.Name)
		if node.Role == RoleDatastore {
			vol = datastoreVolumeName(state.ClusterName)
		}

		if err := p.volumeDelete(ctx, vol); err != nil {
			errs = append(errs, err)
		}
	}

	return errs
}

// sweepByLabel reclaims every container and named volume tagged
// k3s.cluster.name=<clusterName>, independent of any recorded node list. It is the
// half-created-cluster fix: a Create that failed before saveState leaves orphaned
// containers/volumes but no state.json, and this sweep finds them from their labels
// alone. Each step is idempotent (stop/remove/volumeDelete ignore "not found"). An
// empty clusterName is skipped — the selector "k3s.cluster.name=" would match nothing
// useful.
func (p *provisioner) sweepByLabel(ctx context.Context, clusterName string, log io.Writer) []error {
	if clusterName == "" {
		return nil
	}

	selector := clusterLabelSelector(clusterName)

	var errs []error

	if names, err := p.listContainersByLabel(ctx, selector); err != nil {
		errs = append(errs, err)
	} else {
		for _, name := range names {
			fmt.Fprintln(log, "sweeping container", name)

			if err := p.stop(ctx, name); err != nil {
				errs = append(errs, err)
			}

			if err := p.remove(ctx, name); err != nil {
				errs = append(errs, err)
			}
		}
	}

	if vols, err := p.listVolumesByLabel(ctx, selector); err != nil {
		errs = append(errs, err)
	} else {
		for _, vol := range vols {
			fmt.Fprintln(log, "sweeping volume", vol)

			if err := p.volumeDelete(ctx, vol); err != nil {
				errs = append(errs, err)
			}
		}
	}

	return errs
}
