// SPDX-License-Identifier: MIT

package apple

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// kubeconfig_merge.go implements -merge-kubeconfig (v0.5.0): fold a cluster's kubeconfig into the
// user's ~/.kube/config and switch to it, so `kubectl get nodes` works without exporting KUBECONFIG.
//
// APPROACH (stdlib-only — no YAML library, no client-go): shell out to
//   kubectl config set-cluster / set-credentials / set-context / use-context.
// These commands merge ADDITIVELY into ~/.kube/config, keyed by the cluster NAME, so the cluster's
// entries de-collide from any other context AND the user's existing contexts are preserved — unlike
// a flat `kubectl config view --flatten` of two configs, where k3s's fixed "default" context/cluster/
// user names would clobber each other. set-cluster/set-credentials want the CA + client cert/key as
// FILES, so we extract the four fields (server URL + three base64 blobs) from the cluster kubeconfig
// with a tiny line scan (the format is one we generate, so a targeted scan beats pulling in a YAML
// dep), base64-decode the blobs to short-lived PEM files under the cluster state dir, and pass them
// with --embed-certs so kubectl inlines them. If kubectl is absent we skip execution and print the
// exact commands (the cert files are kept for the operator to run them by hand).

// kubeconfigCreds is the subset of a kubeconfig needed to register it under a fresh context: the
// API server URL and the three base64-encoded cert blobs k3s writes.
type kubeconfigCreds struct {
	Server         string // https://<endpoint>:6443
	CAData         string // base64 PEM (certificate-authority-data)
	ClientCertData string // base64 PEM (client-certificate-data)
	ClientKeyData  string // base64 PEM (client-key-data)
}

// parseKubeconfigCreds extracts the server URL and the three base64 cert blobs from a kubeconfig.
// It is a targeted line scan (NOT a YAML parser): each field is a single `key: value` line in the
// k3s kubeconfig this tool generates, so scanning for the known keys is robust and dependency-free.
// Pure so the extraction is unit-testable. Returns an error if any required field is missing.
func parseKubeconfigCreds(raw []byte) (kubeconfigCreds, error) {
	var c kubeconfigCreds

	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024) // cert blobs are long single lines

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		switch {
		case c.Server == "" && strings.HasPrefix(line, "server:"):
			c.Server = strings.TrimSpace(strings.TrimPrefix(line, "server:"))
		case c.CAData == "" && strings.HasPrefix(line, "certificate-authority-data:"):
			c.CAData = strings.TrimSpace(strings.TrimPrefix(line, "certificate-authority-data:"))
		case c.ClientCertData == "" && strings.HasPrefix(line, "client-certificate-data:"):
			c.ClientCertData = strings.TrimSpace(strings.TrimPrefix(line, "client-certificate-data:"))
		case c.ClientKeyData == "" && strings.HasPrefix(line, "client-key-data:"):
			c.ClientKeyData = strings.TrimSpace(strings.TrimPrefix(line, "client-key-data:"))
		}
	}

	if err := scanner.Err(); err != nil {
		return kubeconfigCreds{}, fmt.Errorf("scanning kubeconfig: %w", err)
	}

	if c.Server == "" || c.CAData == "" || c.ClientCertData == "" || c.ClientKeyData == "" {
		return kubeconfigCreds{}, fmt.Errorf("kubeconfig is missing one of server / certificate-authority-data / " +
			"client-certificate-data / client-key-data (is it a k3s admin kubeconfig?)")
	}

	return c, nil
}

// mergeCertFiles are the host paths the decoded PEM blobs are written to, fed to the kubectl
// set-cluster / set-credentials commands.
type mergeCertFiles struct {
	CA         string
	ClientCert string
	ClientKey  string
}

// mergeKubeconfigCommands builds the four kubectl argument vectors that register a cluster under a
// fresh, cluster-name-keyed context in ~/.kube/config and switch to it. Pure (no I/O, no exec) so
// the command construction is unit-testable. --embed-certs inlines the cert files so the merged
// config is self-contained (the temp files can then be removed). Each vector omits --kubeconfig so
// kubectl targets the user's default config (KUBECONFIG[0] or ~/.kube/config).
func mergeKubeconfigCommands(clusterName, server string, files mergeCertFiles) [][]string {
	return [][]string{
		{
			"config", "set-cluster", clusterName,
			"--server=" + server,
			"--certificate-authority=" + files.CA,
			"--embed-certs=true",
		},
		{
			"config", "set-credentials", clusterName,
			"--client-certificate=" + files.ClientCert,
			"--client-key=" + files.ClientKey,
			"--embed-certs=true",
		},
		{
			"config", "set-context", clusterName,
			"--cluster=" + clusterName,
			"--user=" + clusterName,
		},
		{"config", "use-context", clusterName},
	}
}

// MergeKubeconfig folds the cluster's kubeconfig into the user's ~/.kube/config under a context
// named after the cluster and switches to it. Package-level (not a provisioner method) because it
// touches no `container` daemon — it is kubectl + host file I/O only, like ListClusters. Exported
// for the cmd driver. See the file-level comment for the stdlib-only approach.
func MergeKubeconfig(ctx context.Context, stateDir, clusterName string, logw io.Writer) error {
	if logw == nil {
		logw = io.Discard
	}

	clusterDir, err := ensureClusterDir(stateDir, clusterName)
	if err != nil {
		return err
	}

	kubeconfigPath := filepath.Join(clusterDir, "kubeconfig")

	raw, err := os.ReadFile(kubeconfigPath)
	if err != nil {
		return fmt.Errorf("reading kubeconfig %q (create the cluster first): %w", kubeconfigPath, err)
	}

	creds, err := parseKubeconfigCreds(raw)
	if err != nil {
		return fmt.Errorf("cluster %q: %w", clusterName, err)
	}

	// Decode the three base64 PEM blobs to short-lived files under a .merge subdir. set-cluster /
	// set-credentials need file paths; --embed-certs then inlines them into ~/.kube/config.
	mergeDir := filepath.Join(clusterDir, ".merge")

	files, err := writeMergeCertFiles(mergeDir, creds)
	if err != nil {
		return err
	}

	cmds := mergeKubeconfigCommands(clusterName, creds.Server, files)

	// kubectl absent: keep the cert files and print the exact commands for the operator to run.
	if _, err := exec.LookPath("kubectl"); err != nil {
		printMergeCommands(logw, cmds)

		return nil
	}

	if err := runKubectlCommands(ctx, cmds, logw); err != nil {
		return err
	}

	// Certs are embedded in ~/.kube/config now; the temp PEM files are no longer needed.
	if err := os.RemoveAll(mergeDir); err != nil {
		return fmt.Errorf("cleaning up temp cert files %q: %w", mergeDir, err)
	}

	fmt.Fprintf(logw, "merged cluster %q into %s and set the current context to %q\n", clusterName, kubeconfigTarget(), clusterName)

	return nil
}

// kubeconfigTarget reports where kubectl actually writes the merged config so the success message
// is accurate: the FIRST entry of $KUBECONFIG when set (kubectl writes to the first file in the
// list), else the default ~/.kube/config.
func kubeconfigTarget() string {
	if kc := os.Getenv("KUBECONFIG"); kc != "" {
		if first := strings.SplitN(kc, string(os.PathListSeparator), 2)[0]; first != "" {
			return first
		}
	}

	return "~/.kube/config"
}

// writeMergeCertFiles base64-decodes the CA / client cert / client key blobs and writes them as PEM
// files (0600) under dir, returning their paths. The key file is a secret, so 0600 throughout.
func writeMergeCertFiles(dir string, creds kubeconfigCreds) (mergeCertFiles, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return mergeCertFiles{}, fmt.Errorf("creating merge dir %q: %w", dir, err)
	}

	files := mergeCertFiles{
		CA:         filepath.Join(dir, "ca.crt"),
		ClientCert: filepath.Join(dir, "client.crt"),
		ClientKey:  filepath.Join(dir, "client.key"),
	}

	blobs := map[string]string{
		files.CA:         creds.CAData,
		files.ClientCert: creds.ClientCertData,
		files.ClientKey:  creds.ClientKeyData,
	}

	for path, b64 := range blobs {
		pemBytes, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return mergeCertFiles{}, fmt.Errorf("decoding cert data for %q: %w", filepath.Base(path), err)
		}

		if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
			return mergeCertFiles{}, fmt.Errorf("writing %q: %w", path, err)
		}
	}

	return files, nil
}

// runKubectlCommands runs each kubectl argument vector in order, surfacing the first failure with
// its stderr. Stops on the first error (a later command depends on an earlier one succeeding).
func runKubectlCommands(ctx context.Context, cmds [][]string, logw io.Writer) error {
	for _, args := range cmds {
		fmt.Fprintln(logw, "kubectl", strings.Join(args, " "))

		out, err := exec.CommandContext(ctx, "kubectl", args...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("kubectl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
	}

	return nil
}

// printMergeCommands prints the kubectl commands for the operator to run when kubectl is absent.
func printMergeCommands(logw io.Writer, cmds [][]string) {
	fmt.Fprintln(logw, "kubectl not found on PATH; run these commands once kubectl is installed:")

	for _, args := range cmds {
		fmt.Fprintln(logw, "  kubectl", strings.Join(args, " "))
	}
}
