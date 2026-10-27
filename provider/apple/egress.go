// SPDX-License-Identifier: MIT

package apple

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/BinHsu/aegis-apple-container-provisioner-k3s/egress"
)

// egress.go is k3ac's consumer side of the egress relay (v0.8.0, docs/ADR/0004). The relay itself
// (package egress, command ac-egress) knows nothing about k3s; this file owns the k3s wiring:
//
//   - find the cluster network's vmnet gateway (the host bridge address),
//   - run ac-egress bound to that address as a detached process, pid in <state>/<name>/egress.pid,
//   - install the guest DNAT rules in every k3s node (servers + agents; pods are covered through
//     nat PREROUTING, so no per-pod proxy env is needed),
//   - stop the relay by its recorded pid (verified to be ac-egress) on -stop / -destroy.
//
// Cluster-internal traffic is never redirected: the pod (10.42/16) and service (10.43/16) CIDRs,
// node IPs and every other private range are in egress.DefaultBypass.

// Relay file names under <stateDir>/<cluster>/.
const (
	egressPIDFile = "egress.pid"
	egressLogFile = "egress.log"
	// EgressBinary is the relay command k3ac launches: next to the k3ac binary, else on PATH.
	EgressBinary = "ac-egress"
)

// egressReadyTimeout bounds how long Enable waits for the relay to accept connections.
const egressReadyTimeout = 10 * time.Second

func egressPIDPath(stateDir, cluster string) string {
	return filepath.Join(stateDir, cluster, egressPIDFile)
}

// networkGateway returns the vmnet gateway (= host bridge address) of a container network.
func (p *provisioner) networkGateway(ctx context.Context, network string) (netip.Addr, error) {
	if network == "" {
		network = defaultNetwork
	}

	out, err := p.run(ctx, "network", "inspect", network)
	if err != nil {
		return netip.Addr{}, err
	}

	return parseNetworkGateway(out)
}

// parseNetworkGateway reads .[0].status.ipv4Gateway from `container network inspect`. Pure.
func parseNetworkGateway(out string) (netip.Addr, error) {
	var nets []struct {
		Status struct {
			IPv4Gateway string `json:"ipv4Gateway"`
		} `json:"status"`
	}

	if err := json.Unmarshal([]byte(out), &nets); err != nil {
		return netip.Addr{}, fmt.Errorf("parsing network inspect: %w", err)
	}

	if len(nets) == 0 || nets[0].Status.IPv4Gateway == "" {
		return netip.Addr{}, errors.New("network inspect: no ipv4Gateway (is a node on that network running?)")
	}

	return netip.ParseAddr(nets[0].Status.IPv4Gateway)
}

// findEgressBinary resolves ac-egress next to the running k3ac, else on PATH.
func findEgressBinary() (string, error) {
	if self, err := os.Executable(); err == nil {
		cand := filepath.Join(filepath.Dir(self), EgressBinary)
		if st, err := os.Stat(cand); err == nil && !st.IsDir() {
			return cand, nil
		}
	}

	path, err := exec.LookPath(EgressBinary)
	if err != nil {
		return "", fmt.Errorf("%s not found next to k3ac or on PATH (build it: go build -o _out/%s ./cmd/%s)",
			EgressBinary, EgressBinary, EgressBinary)
	}

	return path, nil
}

// EnableEgress (re)starts the cluster's egress relay and installs the guest rules in every k3s
// node, then records Egress=true in state so a later plain -start re-enables it. Idempotent: an
// existing relay for this cluster is stopped first and the guest chain is rebuilt.
func (p *provisioner) EnableEgress(ctx context.Context, stateDir, clusterName string, logw io.Writer) error {
	if logw == nil {
		logw = io.Discard
	}

	state, err := LoadState(stateDir, clusterName)
	if err != nil {
		return err
	}

	gw, err := p.networkGateway(ctx, state.Network)
	if err != nil {
		return fmt.Errorf("egress: %w", err)
	}

	if err := DisableEgress(stateDir, clusterName, logw); err != nil {
		return err
	}

	bin, err := findEgressBinary()
	if err != nil {
		return err
	}

	pid, err := spawnRelay(bin, gw, filepath.Join(stateDir, clusterName))
	if err != nil {
		return err
	}

	fmt.Fprintf(logw, "egress relay %s pid %d on %s (log %s)\n", bin, pid, gw, filepath.Join(stateDir, clusterName, egressLogFile))

	if err := waitRelayReady(ctx, gw, pid); err != nil {
		DisableEgress(stateDir, clusterName, logw) //nolint:errcheck

		return err
	}

	script, err := egress.GuestScript(gw, egress.DefaultHTTPPort, egress.DefaultTLSPort, egress.DefaultDNSPort, egress.DefaultBypass)
	if err != nil {
		return err
	}

	for _, n := range state.Nodes {
		if n.Role != RoleServer && n.Role != RoleAgent {
			continue
		}

		if _, err := p.exec(ctx, n.ID, "sh", "-c", script); err != nil {
			return fmt.Errorf("egress: installing guest rules on %q: %w", n.Name, err)
		}

		fmt.Fprintf(logw, "egress rules installed on %s\n", n.Name)
	}

	if !state.Egress {
		state.Egress = true
		if err := saveState(state); err != nil {
			return err
		}
	}

	return nil
}

// spawnRelay starts ac-egress detached (own session, so it outlives this k3ac invocation and is
// not killed with the terminal's process group) and writes its pid file.
func spawnRelay(bin string, gw netip.Addr, dir string) (int, error) {
	logf, err := os.OpenFile(filepath.Join(dir, egressLogFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, err
	}
	defer logf.Close() //nolint:errcheck

	cmd := exec.Command(bin, "-listen", gw.String()) //nolint:gosec // bin resolved by findEgressBinary
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("egress: starting %s: %w", bin, err)
	}

	pid := cmd.Process.Pid
	cmd.Process.Release() //nolint:errcheck

	if err := os.WriteFile(filepath.Join(dir, egressPIDFile), []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
		syscall.Kill(pid, syscall.SIGTERM) //nolint:errcheck

		return 0, err
	}

	return pid, nil
}

func waitRelayReady(ctx context.Context, gw netip.Addr, pid int) error {
	addr := net.JoinHostPort(gw.String(), strconv.Itoa(egress.DefaultHTTPPort))
	deadline := time.Now().Add(egressReadyTimeout)

	for {
		if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
			c.Close() //nolint:errcheck

			return nil
		}

		if syscall.Kill(pid, 0) != nil {
			return fmt.Errorf("egress: relay pid %d exited before listening on %s (see %s)", pid, addr, egressLogFile)
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("egress: relay not listening on %s after %s", addr, egressReadyTimeout)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// DisableEgress stops the cluster's relay if one is recorded. It signals ONLY the recorded pid,
// and only after confirming that pid is still an ac-egress process (a stale pid file whose pid was
// reused by something else is discarded, never signalled). No pid file = nothing to do.
func DisableEgress(stateDir, clusterName string, logw io.Writer) error {
	if logw == nil {
		logw = io.Discard
	}

	path := egressPIDPath(stateDir, clusterName)

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 1 {
		return os.Remove(path)
	}

	out, err := exec.Command("ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil || !isRelayCommand(string(out)) {
		fmt.Fprintf(logw, "egress: pid %d is not a running %s; discarding stale pid file\n", pid, EgressBinary)

		return os.Remove(path)
	}

	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("egress: stopping relay pid %d: %w", pid, err)
	}

	for range 50 {
		if syscall.Kill(pid, 0) != nil {
			break
		}

		time.Sleep(100 * time.Millisecond)
	}

	fmt.Fprintf(logw, "egress relay pid %d stopped\n", pid)

	return os.Remove(path)
}

// isRelayCommand reports whether a `ps -o command=` line is an ac-egress relay. Pure.
func isRelayCommand(cmdline string) bool {
	f := strings.Fields(cmdline)

	return len(f) > 0 && filepath.Base(f[0]) == EgressBinary && strings.Contains(cmdline, "-listen")
}
