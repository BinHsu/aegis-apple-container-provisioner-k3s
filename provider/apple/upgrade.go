// SPDX-License-Identifier: MIT

package apple

import (
	"context"
	"fmt"
	"io"
	"net/netip"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// upgrade.go implements the v0.6.0 rolling k3s version upgrade and rollback.
//
// Both are the SAME orchestration (rollingReplace): visit the k3s nodes one at a time, servers
// before agents (k3sNodesInUpgradeOrder), and for each — cordon, drain, recreate the container on
// the target image PRESERVING its named state volume, wait for it to rejoin Ready, uncordon — then
// move to the next. Servers are stateless against the external datastore, so a recreated server just
// reconnects over TLS; the API LB re-resolves its FQDN automatically. Only one node is ever down at a
// time, so the cluster stays available throughout.
//
//   - -upgrade pins the CURRENT image into state.PreviousImage and moves to the new image.
//   - -rollback moves to state.PreviousImage and pins the image it rolled away from, so a rollback is
//     itself reversible.
//
// Both need -force: recreating a node drains its pods and briefly takes it down.
//
// cordon/drain/uncordon and the Ready wait are HOST kubectl calls against the saved kubeconfig (the
// drainNode pattern), never `container exec` — k3s is a multi-call binary and exec mangles its
// subcommands (container.go), so all kubectl work stays on the host.

// k3sUpgradeWaitTimeout bounds how long the rolling replace waits for a recreated node to report
// Ready on the new image before giving up. Generous: a cold k3s VM plus image pull can take a while.
const k3sUpgradeWaitTimeout = 5 * time.Minute

// resolvedImage returns img, or the pinned default when img is empty (a pre-v0.2.0 state). The single
// fallback both -upgrade (pinning the outgoing image) and -rollback rely on, so "" is never recorded
// as a previous image.
func resolvedImage(img string) string {
	if img == "" {
		return defaultK3sImage
	}

	return img
}

// upgradePins computes the (recordedPrevious, recordedNew) image pins for an -upgrade: the new image
// moves in, and the cluster's CURRENT image (resolved) is pinned as the rollback target. Pure so the
// pin bookkeeping is unit-testable (CLAUDE.md k) without recreating anything.
func upgradePins(currentImage, newImage string) (recordedPrevious, recordedNew string) {
	return resolvedImage(currentImage), newImage
}

// rollbackPins computes the (target, recordedPrevious) for a -rollback: move to the recorded previous
// image, and pin the image we rolled AWAY from (the current one) so the rollback is reversible. err
// when there is no previous image to roll back to. Pure (BVA: empty previous -> error; set -> target).
func rollbackPins(state ClusterState) (target, recordedPrevious string, err error) {
	if state.PreviousImage == "" {
		return "", "", fmt.Errorf("cluster %q has no previous image recorded; nothing to roll back to "+
			"(run -upgrade first)", state.ClusterName)
	}

	return state.PreviousImage, resolvedImage(state.Image), nil
}

// kubectlCordonArgs / kubectlUncordonArgs / kubectlDrainArgs / kubectlWaitReadyArgs build the host
// kubectl argument vectors for the rolling replace. Pure so the flag composition is unit-testable
// (BVA, CLAUDE.md k) — notably that drain carries --ignore-daemonsets and --delete-emptydir-data
// (without them a drain stalls on DaemonSet pods or local-storage pods and the upgrade hangs).
func kubectlCordonArgs(kubeconfig, node string) []string {
	return []string{"--kubeconfig", kubeconfig, "cordon", node}
}

func kubectlUncordonArgs(kubeconfig, node string) []string {
	return []string{"--kubeconfig", kubeconfig, "uncordon", node}
}

func kubectlDrainArgs(kubeconfig, node string) []string {
	return []string{"--kubeconfig", kubeconfig, "drain", node, "--ignore-daemonsets", "--delete-emptydir-data"}
}

func kubectlWaitReadyArgs(kubeconfig, node string, timeout time.Duration) []string {
	return []string{"--kubeconfig", kubeconfig, "wait", "--for=condition=Ready",
		"node/" + node, fmt.Sprintf("--timeout=%ds", int(timeout.Seconds()))}
}

// Upgrade rolls the cluster onto newImage one k3s node at a time (servers first, then agents),
// pinning the outgoing image for -rollback. Needs -force.
func (p *provisioner) Upgrade(ctx context.Context, stateDir, clusterName, newImage string, force bool, logw io.Writer) (ClusterState, error) {
	if logw == nil {
		logw = io.Discard
	}

	if newImage == "" {
		return ClusterState{}, fmt.Errorf("-upgrade needs a target image (set -image)")
	}

	state, err := LoadState(stateDir, clusterName)
	if err != nil {
		return ClusterState{}, err
	}

	clusterDir, err := ensureClusterDir(stateDir, clusterName)
	if err != nil {
		return ClusterState{}, err
	}

	prev, next := upgradePins(state.Image, newImage)

	if err := ensureForced(force, logw, "-upgrade", []string{
		fmt.Sprintf("roll every k3s node of cluster %q from %s to %s", clusterName, resolvedImage(state.Image), next),
		"cordon, drain, recreate, and uncordon each node one at a time (pods are disrupted)",
	}); err != nil {
		return ClusterState{}, err
	}

	if err := p.rollingReplace(ctx, &state, stateDir, clusterDir, next, logw); err != nil {
		return ClusterState{}, err
	}

	state.PreviousImage = prev
	state.Image = next

	if err := saveState(state); err != nil {
		return ClusterState{}, err
	}

	fmt.Fprintf(logw, "upgraded cluster %q to %s (previous image %s pinned for -rollback)\n", clusterName, next, prev)

	return state, nil
}

// Rollback rolls the cluster back onto the image pinned at the last -upgrade, using the same rolling
// orchestration. Needs -force.
func (p *provisioner) Rollback(ctx context.Context, stateDir, clusterName string, force bool, logw io.Writer) (ClusterState, error) {
	if logw == nil {
		logw = io.Discard
	}

	state, err := LoadState(stateDir, clusterName)
	if err != nil {
		return ClusterState{}, err
	}

	clusterDir, err := ensureClusterDir(stateDir, clusterName)
	if err != nil {
		return ClusterState{}, err
	}

	target, prev, err := rollbackPins(state)
	if err != nil {
		return ClusterState{}, err
	}

	if err := ensureForced(force, logw, "-rollback", []string{
		fmt.Sprintf("roll every k3s node of cluster %q from %s back to %s", clusterName, resolvedImage(state.Image), target),
		"cordon, drain, recreate, and uncordon each node one at a time (pods are disrupted)",
	}); err != nil {
		return ClusterState{}, err
	}

	if err := p.rollingReplace(ctx, &state, stateDir, clusterDir, target, logw); err != nil {
		return ClusterState{}, err
	}

	state.PreviousImage = prev
	state.Image = target

	if err := saveState(state); err != nil {
		return ClusterState{}, err
	}

	fmt.Fprintf(logw, "rolled cluster %q back to %s\n", clusterName, target)

	return state, nil
}

// rollingReplace is the shared upgrade/rollback engine: recreate each k3s node on targetImage, one at
// a time, cordon/drain before and Ready-wait/uncordon after. Mutates state in place (each recreated
// node's NodeInfo, carrying its new DHCP IP). The caller records the image pins and persists.
func (p *provisioner) rollingReplace(ctx context.Context, state *ClusterState, stateDir, clusterDir, targetImage string, logw io.Writer) error {
	cfg := clusterConfigFromState(*state, stateDir, clusterDir)
	cfg.Image = targetImage

	kubeconfig := filepath.Join(clusterDir, "kubeconfig")

	for _, node := range k3sNodesInUpgradeOrder(state.Nodes) {
		if err := p.rollOneNode(ctx, cfg, node, kubeconfig, state.ServerURL, logw); err != nil {
			return err
		}
	}

	// Re-resolve every recreated node's current IP into state (recreate boots a new DHCP IP).
	for _, node := range k3sNodesInUpgradeOrder(state.Nodes) {
		if addr, err := p.inspectIPv4(ctx, node.ID); err == nil {
			info := nodeConfigFromInfo(node)
			replaceNodeInState(state, NodeInfo{
				ID: node.ID, Name: node.Name, Role: node.Role, IPs: []netip.Addr{addr},
				Memory: info.Memory, NanoCPUs: info.NanoCPUs, Labels: info.Labels, ExtraArgs: info.ExtraArgs,
			})
		}
	}

	return nil
}

// rollOneNode cordons + drains a single node, recreates it on cfg.Image, waits for it to rejoin
// Ready, and uncordons it. Extracted so rollingReplace stays under the complexity gates and each
// node's full disrupt-then-restore cycle reads as one unit.
func (p *provisioner) rollOneNode(ctx context.Context, cfg ClusterConfig, node NodeInfo, kubeconfig, serverURL string, logw io.Writer) error {
	fmt.Fprintf(logw, "rolling node %s onto %s\n", node.Name, cfg.Image)

	// cordon + drain are best-effort guards against pod disruption; on a single-server cluster the
	// API may briefly be the very node being rolled, so a drain failure must not abort the upgrade.
	p.runKubectl(ctx, kubectlCordonArgs(kubeconfig, node.Name), logw)
	p.runKubectl(ctx, kubectlDrainArgs(kubeconfig, node.Name), logw)

	url := ""
	if node.Role == RoleAgent {
		url = serverURL // an agent needs its K3S_URL; a server reconnects to the datastore (url stays "")
	}

	if _, err := p.recreateK3sNode(ctx, cfg, nodeConfigFromInfo(node), url, logw); err != nil {
		return err
	}

	// Confirm the node is back on the new image before moving to the next, so only ever one node is
	// down at a time. A server proves liveness with a host-side TLS dial to its apiserver; an agent
	// has no apiserver, so we wait on its Kubernetes Ready condition via kubectl.
	if err := p.waitNodeReady(ctx, node, kubeconfig, logw); err != nil {
		return err
	}

	p.runKubectl(ctx, kubectlUncordonArgs(kubeconfig, node.Name), logw)

	return nil
}

// waitNodeReady blocks until a recreated node is serving again: a server via waitForAPIServer (its
// apiserver answering proves it reconnected to the datastore on the new image), an agent via
// `kubectl wait --for=condition=Ready`. Extracted for clarity and to keep rollOneNode small.
func (p *provisioner) waitNodeReady(ctx context.Context, node NodeInfo, kubeconfig string, logw io.Writer) error {
	if node.Role == RoleServer {
		addr, err := p.inspectIPv4(ctx, node.ID)
		if err != nil {
			return fmt.Errorf("node %q: %w", node.Name, err)
		}

		return p.waitForAPIServer(ctx, addr)
	}

	if err := p.runKubectlErr(ctx, kubectlWaitReadyArgs(kubeconfig, node.Name, k3sUpgradeWaitTimeout), logw); err != nil {
		return fmt.Errorf("node %q did not become Ready after upgrade: %w", node.Name, err)
	}

	return nil
}

// runKubectl runs a host kubectl command best-effort, logging a warning on failure but not aborting —
// for the cordon/drain/uncordon steps, which must not break the upgrade if kubectl is absent or the
// API momentarily unreachable (the drainNode pattern). The kubeconfig is in the args.
func (p *provisioner) runKubectl(ctx context.Context, args []string, logw io.Writer) {
	if err := p.runKubectlErr(ctx, args, logw); err != nil {
		fmt.Fprintf(logw, "warning: kubectl %s failed (continuing): %v\n", strings.Join(args, " "), err)
	}
}

// runKubectlErr runs a host kubectl command and returns its error (with stderr). Used directly where
// the result IS load-bearing (the Ready wait) and via runKubectl where it is best-effort.
func (p *provisioner) runKubectlErr(ctx context.Context, args []string, logw io.Writer) error {
	fmt.Fprintln(logw, "kubectl", strings.Join(args, " "))

	out, err := exec.CommandContext(ctx, "kubectl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("kubectl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}

	return nil
}
