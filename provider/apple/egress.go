// SPDX-License-Identifier: MIT

package apple

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
//   - run ac-egress bound to that address as a transient launchd job in the user's domain (label
//     io.github.binhsu.k3ac.egress.<name>; binary copy, plist and log in
//     ~/Library/Caches/k3ac/egress/<name>/, pid in <state>/<name>/egress.pid), so it is not a
//     child of k3ac or of whatever shell ran k3ac,
//   - install the guest DNAT rules in every k3s node (servers + agents; pods are covered through
//     nat PREROUTING, so no per-pod proxy env is needed),
//   - stop the relay with launchctl bootout of that label (or, for a pid file left by an older
//     setsid-detached relay, by that pid after verifying it is ac-egress) on -stop / -destroy.
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

// EnableEgress makes sure the cluster's egress relay runs on the network gateway and installs the
// guest rules in every k3s node, then records Egress=true in state so a later plain -start
// re-enables it. Idempotent: a relay this cluster already runs under launchd on the same gateway
// is kept (and the guest chain rebuilt); anything else recorded for the cluster is stopped first.
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

	if err := p.ensureRelay(ctx, stateDir, clusterName, gw, logw); err != nil {
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

// ensureRelay keeps a healthy relay for (cluster, gw) or replaces whatever is recorded with a new
// launchd job.
func (p *provisioner) ensureRelay(ctx context.Context, stateDir, clusterName string, gw netip.Addr, logw io.Writer) error {
	dir := filepath.Join(stateDir, clusterName)

	label, err := egressLabel(clusterName)
	if err != nil {
		return err
	}

	if domain, pid, loaded := launchdJob(label); loaded && pid > 0 && relayServes(pid, gw) {
		if err := writePIDFile(dir, pid); err != nil {
			return err
		}

		fmt.Fprintf(logw, "egress relay pid %d already running on %s (launchd %s/%s)\n", pid, gw, domain, label)

		return nil
	}

	if err := DisableEgress(stateDir, clusterName, logw); err != nil {
		return err
	}

	bin, err := findEgressBinary()
	if err != nil {
		return err
	}

	run, err := relayRunDir(clusterName)
	if err != nil {
		return err
	}

	domain, pid, err := spawnRelay(ctx, bin, gw, dir, run, label)
	if err != nil {
		return err
	}

	fmt.Fprintf(logw, "egress relay pid %d on %s (launchd %s/%s, binary and log in %s)\n", pid, gw, domain, label, run)

	if err := waitRelayReady(ctx, gw, pid, filepath.Join(run, egressLogFile)); err != nil {
		DisableEgress(stateDir, clusterName, logw) //nolint:errcheck

		return err
	}

	return nil
}

// The relay is a launchd job in the user's own domain, not a child of k3ac: whatever shell, terminal
// or agent ran k3ac can exit (or have its process tree reaped) without taking egress down. The job
// is transient: its plist is never put in ~/Library/LaunchAgents, so it is not started at login and
// disappears at logout or on -stop/-destroy (launchctl bootout).
//
// The job runs a COPY of ac-egress from the user cache dir (~/Library/Caches/k3ac/egress/<name>/),
// with its plist and log there too. A launchd job is its own TCC "responsible process": exec'ing a
// binary under ~/Documents (where checkouts usually live) blocked in dyld's open() until the
// Documents-folder privacy check was answered (measured 2026-10-29: ~23 s, and indefinitely if
// nobody answers), whereas the same build started from outside ~/Documents listened within 1 s.
const (
	egressPlistFile   = "egress.plist"
	egressLabelPrefix = "io.github.binhsu.k3ac.egress."
	// launchdPIDTimeout bounds how long spawnRelay waits for launchd to report the job's pid.
	launchdPIDTimeout = 5 * time.Second
	relayCacheSubdir  = "k3ac/egress"
)

// relayRunDir is where the launchd job's binary copy, plist and log live (created 0700).
func relayRunDir(cluster string) (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("egress: %w", err)
	}

	dir := filepath.Join(base, relayCacheSubdir, cluster)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}

	return dir, nil
}

// installRelayBinary copies bin to dir/ac-egress unless an identical copy is already there, by
// writing a temp file and renaming it (a running relay keeps its old inode). Returns the copy.
func installRelayBinary(bin, dir string) (string, error) {
	src, err := os.ReadFile(bin) //nolint:gosec // bin resolved by findEgressBinary
	if err != nil {
		return "", err
	}

	dst := filepath.Join(dir, EgressBinary)
	if cur, err := os.ReadFile(dst); err == nil && bytes.Equal(cur, src) { //nolint:gosec // our own cache path
		return dst, nil
	}

	tmp, err := os.CreateTemp(dir, EgressBinary+".tmp*")
	if err != nil {
		return "", err
	}

	defer os.Remove(tmp.Name()) //nolint:errcheck // gone after a successful rename

	if _, err := tmp.Write(src); err != nil {
		tmp.Close() //nolint:errcheck

		return "", err
	}

	if err := tmp.Close(); err != nil {
		return "", err
	}

	if err := os.Chmod(tmp.Name(), 0o700); err != nil { //nolint:gosec // executable, owner only
		return "", err
	}

	return dst, os.Rename(tmp.Name(), dst)
}

var labelSafe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// egressLabel is the launchd label of a cluster's relay. Pure.
func egressLabel(cluster string) (string, error) {
	if !labelSafe.MatchString(cluster) {
		return "", fmt.Errorf("egress: cluster name %q cannot be used in a launchd label", cluster)
	}

	return egressLabelPrefix + cluster, nil
}

// launchdDomains are tried in order: the login session (gui) first, the per-user background
// domain (e.g. an SSH session without a console login) second.
func launchdDomains() []string {
	uid := strconv.Itoa(os.Getuid())

	return []string{"gui/" + uid, "user/" + uid}
}

// relayPlist renders the transient launchd job for one relay. bin, logPath must be absolute. Pure.
func relayPlist(label, bin string, gw netip.Addr, logPath string) string {
	esc := func(s string) string {
		var b strings.Builder
		xml.EscapeText(&b, []byte(s)) //nolint:errcheck // strings.Builder never fails

		return b.String()
	}

	var b strings.Builder

	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
`)
	fmt.Fprintf(&b, "\t<key>Label</key><string>%s</string>\n", esc(label))
	b.WriteString("\t<key>ProgramArguments</key>\n\t<array>\n")

	for _, a := range []string{bin, "-listen", gw.String()} {
		fmt.Fprintf(&b, "\t\t<string>%s</string>\n", esc(a))
	}

	b.WriteString("\t</array>\n")
	fmt.Fprintf(&b, "\t<key>StandardOutPath</key><string>%s</string>\n", esc(logPath))
	fmt.Fprintf(&b, "\t<key>StandardErrorPath</key><string>%s</string>\n", esc(logPath))
	b.WriteString("\t<key>RunAtLoad</key><true/>\n\t<key>KeepAlive</key><false/>\n</dict>\n</plist>\n")

	return b.String()
}

// launchdJob reports whether label is loaded in one of launchdDomains, and its running pid (0 =
// loaded but not running).
func launchdJob(label string) (domain string, pid int, loaded bool) {
	for _, d := range launchdDomains() {
		out, err := exec.Command("launchctl", "print", d+"/"+label).Output()
		if err == nil {
			return d, parseLaunchdPID(string(out)), true
		}
	}

	return "", 0, false
}

// parseLaunchdPID reads the job's top-level "pid = N" line from `launchctl print`. Nested blocks
// are indented deeper and ignored. Pure.
func parseLaunchdPID(out string) int {
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "\tpid = ") {
			continue
		}

		if pid, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "\tpid = "))); err == nil && pid > 1 {
			return pid
		}
	}

	return 0
}

// spawnRelay copies ac-egress into run, hands that copy to launchd as a transient job, and records
// its pid in the cluster state dir.
func spawnRelay(ctx context.Context, bin string, gw netip.Addr, dir, run, label string) (string, int, error) {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return "", 0, err
	}

	if bin, err = installRelayBinary(bin, run); err != nil {
		return "", 0, fmt.Errorf("egress: copying relay binary: %w", err)
	}

	logPath := filepath.Join(run, egressLogFile)
	plistPath := filepath.Join(run, egressPlistFile)

	if err := os.WriteFile(plistPath, []byte(relayPlist(label, bin, gw, logPath)), 0o600); err != nil {
		return "", 0, err
	}

	var errs []error

	domain := ""

	for _, d := range launchdDomains() {
		out, err := exec.Command("launchctl", "bootstrap", d, plistPath).CombinedOutput()
		if err == nil {
			domain = d

			break
		}

		errs = append(errs, fmt.Errorf("launchctl bootstrap %s: %w: %s", d, err, strings.TrimSpace(string(out))))
	}

	if domain == "" {
		os.Remove(plistPath) //nolint:errcheck

		return "", 0, fmt.Errorf("egress: could not start %s under launchd: %w", label, errors.Join(errs...))
	}

	deadline := time.Now().Add(launchdPIDTimeout)

	for {
		if _, pid, loaded := launchdJob(label); loaded && pid > 0 {
			if err := writePIDFile(absDir, pid); err != nil {
				bootout(domain, label) //nolint:errcheck

				return "", 0, err
			}

			return domain, pid, nil
		}

		if time.Now().After(deadline) {
			bootout(domain, label) //nolint:errcheck

			return "", 0, fmt.Errorf("egress: launchd job %s has no pid after %s (see %s)", label, launchdPIDTimeout, logPath)
		}

		select {
		case <-ctx.Done():
			bootout(domain, label) //nolint:errcheck

			return "", 0, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func writePIDFile(dir string, pid int) error {
	return os.WriteFile(filepath.Join(dir, egressPIDFile), []byte(strconv.Itoa(pid)+"\n"), 0o600)
}

// bootout removes the job; launchd signals exactly that job's process (SIGTERM, then SIGKILL).
func bootout(domain, label string) error {
	if out, err := exec.Command("launchctl", "bootout", domain+"/"+label).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl bootout %s/%s: %w: %s", domain, label, err, strings.TrimSpace(string(out)))
	}

	return nil
}

// relayServes reports whether pid is an ac-egress relay bound to gw.
func relayServes(pid int, gw netip.Addr) bool {
	out, err := exec.Command("ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output()

	return err == nil && relayCommandFor(string(out), gw)
}

// relayCommandFor reports whether a `ps -o command=` line is an ac-egress relay listening on gw. Pure.
func relayCommandFor(cmdline string, gw netip.Addr) bool {
	if !isRelayCommand(cmdline) {
		return false
	}

	f := strings.Fields(cmdline)
	for i := 0; i+1 < len(f); i++ {
		if f[i] == "-listen" {
			return f[i+1] == gw.String()
		}
	}

	return false
}

func waitRelayReady(ctx context.Context, gw netip.Addr, pid int, logPath string) error {
	addr := net.JoinHostPort(gw.String(), strconv.Itoa(egress.DefaultHTTPPort))
	deadline := time.Now().Add(egressReadyTimeout)

	for {
		if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
			c.Close() //nolint:errcheck

			return nil
		}

		if syscall.Kill(pid, 0) != nil {
			return fmt.Errorf("egress: relay pid %d exited before listening on %s (see %s)", pid, addr, logPath)
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("egress: relay not listening on %s after %s (see %s)", addr, egressReadyTimeout, logPath)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// DisableEgress stops the cluster's relay. A launchd job with the cluster's label is booted out
// (launchd signals exactly its process). A pid file left by a pre-launchd relay (v0.8.0 dev builds
// ran it setsid-detached) is honoured too, but its pid is signalled ONLY after confirming it is
// still an ac-egress process (a reused pid is discarded, never signalled). Nothing recorded =
// nothing to do.
func DisableEgress(stateDir, clusterName string, logw io.Writer) error {
	if logw == nil {
		logw = io.Discard
	}

	label, err := egressLabel(clusterName)
	if err != nil {
		return err
	}

	jobPID := 0

	if domain, pid, loaded := launchdJob(label); loaded {
		if err := bootout(domain, label); err != nil {
			return fmt.Errorf("egress: %w", err)
		}

		if pid > 0 {
			waitExit(pid)
			fmt.Fprintf(logw, "egress relay pid %d stopped (launchd %s/%s)\n", pid, domain, label)
		}

		jobPID = pid
	}

	if base, err := os.UserCacheDir(); err == nil {
		plist := filepath.Join(base, relayCacheSubdir, clusterName, egressPlistFile)
		if err := os.Remove(plist); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}

	path := egressPIDPath(stateDir, clusterName)

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 1 || pid == jobPID {
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

	waitExit(pid)
	fmt.Fprintf(logw, "egress relay pid %d stopped\n", pid)

	return os.Remove(path)
}

// waitExit polls up to 5 s for pid to disappear.
func waitExit(pid int) {
	for range 50 {
		if syscall.Kill(pid, 0) != nil {
			return
		}

		time.Sleep(100 * time.Millisecond)
	}
}

// isRelayCommand reports whether a `ps -o command=` line is an ac-egress relay. Pure.
func isRelayCommand(cmdline string) bool {
	f := strings.Fields(cmdline)

	return len(f) > 0 && filepath.Base(f[0]) == EgressBinary && strings.Contains(cmdline, "-listen")
}
