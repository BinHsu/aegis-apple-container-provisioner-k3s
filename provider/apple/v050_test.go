// SPDX-License-Identifier: MIT

package apple

import (
	"crypto/x509"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// --- Feature 1: etcd peer/client TLS ---

// parseCertPEM decodes a single PEM CERTIFICATE block and parses it.
func parseCertPEM(t *testing.T, pemBytes []byte) *x509.Certificate {
	t.Helper()

	block, _ := pem.Decode(pemBytes)
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatalf("expected a PEM CERTIFICATE block, got %q", string(pemBytes))
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parsing certificate: %v", err)
	}

	return cert
}

// hasLoopbackIP reports whether 127.0.0.1 is among ips.
func hasLoopbackIP(ips []net.IP) bool {
	for _, ip := range ips {
		if ip.Equal(net.IPv4(127, 0, 0, 1)) {
			return true
		}
	}

	return false
}

// TestEtcdMemberSANs is the BVA (CLAUDE.md k) on the load-bearing SAN contents: the member FQDN
// (so TLS verifies after the IP shift — name-bound, never IP-bound), plus localhost + 127.0.0.1.
func TestEtcdMemberSANs(t *testing.T) {
	dnsNames, ips := etcdMemberSANs("aegis-etcd-1", "aegis")

	if !slices.Contains(dnsNames, "aegis-etcd-1.aegis") {
		t.Errorf("member FQDN SAN missing (load-bearing across the IP shift): %v", dnsNames)
	}

	if !slices.Contains(dnsNames, "localhost") {
		t.Errorf("localhost SAN missing: %v", dnsNames)
	}

	if !hasLoopbackIP(ips) {
		t.Errorf("127.0.0.1 IP SAN missing: %v", ips)
	}

	// No vmnet IP is ever a SAN — only the loopback (a vmnet IP SAN would go stale on restart).
	if len(ips) != 1 {
		t.Errorf("expected exactly the loopback IP SAN, got %v", ips)
	}

	// IP-only fallback (empty domain): the bare member name is the FQDN SAN.
	bareDNS, _ := etcdMemberSANs("aegis-etcd-1", "")
	if !slices.Contains(bareDNS, "aegis-etcd-1") {
		t.Errorf("empty-domain SAN must be the bare name: %v", bareDNS)
	}
}

// TestGenerateEtcdTLS proves the generated bundle is a real, verifiable PKI: each member's server
// cert chains to the CA and verifies AT its own FQDN with serverAuth (and carries clientAuth, since
// a peer is also a client); the client cert chains with clientAuth; and a member cert does NOT
// verify as a different member's FQDN (the SAN is member-specific — BVA negative boundary).
func TestGenerateEtcdTLS(t *testing.T) {
	names := []string{"aegis-etcd-1", "aegis-etcd-2", "aegis-etcd-3"}

	bundle, err := generateEtcdTLS("aegis", "aegis", names)
	if err != nil {
		t.Fatalf("generateEtcdTLS: %v", err)
	}

	if len(bundle.MemberCerts) != len(names) {
		t.Fatalf("got %d member certs, want %d", len(bundle.MemberCerts), len(names))
	}

	ca := parseCertPEM(t, bundle.CAPEM)
	if !ca.IsCA {
		t.Error("CA cert must have IsCA=true")
	}

	pool := x509.NewCertPool()
	pool.AddCert(ca)

	for _, name := range names {
		pair := bundle.MemberCerts[name]
		cert := parseCertPEM(t, pair.Cert)
		fqdn := name + ".aegis"

		if !slices.Contains(cert.DNSNames, fqdn) || !slices.Contains(cert.DNSNames, "localhost") {
			t.Errorf("member %q SANs missing FQDN/localhost: %v", name, cert.DNSNames)
		}

		if !hasLoopbackIP(cert.IPAddresses) {
			t.Errorf("member %q missing 127.0.0.1 IP SAN: %v", name, cert.IPAddresses)
		}

		if _, err := cert.Verify(x509.VerifyOptions{
			DNSName:   fqdn,
			Roots:     pool,
			KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}); err != nil {
			t.Errorf("member %q server cert must verify against the CA at %q: %v", name, fqdn, err)
		}

		if !slices.Contains(cert.ExtKeyUsage, x509.ExtKeyUsageClientAuth) {
			t.Errorf("member %q cert must also carry clientAuth (a peer is a client too)", name)
		}

		// The private key must parse (PKCS#8).
		keyBlock, _ := pem.Decode(pair.Key)
		if keyBlock == nil {
			t.Fatalf("member %q: no PEM key block", name)
		}

		if _, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes); err != nil {
			t.Errorf("member %q key must parse as PKCS#8: %v", name, err)
		}
	}

	// Client cert: clientAuth, chains to the CA (no hostname).
	client := parseCertPEM(t, bundle.Client.Cert)
	if !slices.Contains(client.ExtKeyUsage, x509.ExtKeyUsageClientAuth) {
		t.Error("client cert must carry clientAuth")
	}

	if _, err := client.Verify(x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		t.Errorf("client cert must verify against the CA: %v", err)
	}

	// BVA negative: member-1's cert must NOT verify as member-2's FQDN.
	c1 := parseCertPEM(t, bundle.MemberCerts["aegis-etcd-1"].Cert)
	if _, err := c1.Verify(x509.VerifyOptions{
		DNSName:   "aegis-etcd-2.aegis",
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err == nil {
		t.Error("member-1 cert must NOT verify as member-2's FQDN (SAN is member-specific)")
	}
}

// TestWriteEtcdTLS locks the delivery layout: one subdir per member (ca.crt/server.crt/server.key)
// and a client subdir (ca.crt/client.crt/client.key), all absolute paths, keys at 0600.
func TestWriteEtcdTLS(t *testing.T) {
	bundle, err := generateEtcdTLS("aegis", "aegis", []string{"aegis-etcd-1"})
	if err != nil {
		t.Fatal(err)
	}

	memberDirs, clientDir, err := writeEtcdTLS(t.TempDir(), bundle)
	if err != nil {
		t.Fatalf("writeEtcdTLS: %v", err)
	}

	md := memberDirs["aegis-etcd-1"]
	if !filepath.IsAbs(md) || !filepath.IsAbs(clientDir) {
		t.Errorf("TLS dirs must be absolute (bind sources): member=%q client=%q", md, clientDir)
	}

	for _, f := range []string{etcdCACertFile, etcdServerCertFile, etcdServerKeyFile} {
		if _, err := os.Stat(filepath.Join(md, f)); err != nil {
			t.Errorf("member dir missing %q: %v", f, err)
		}
	}

	for _, f := range []string{etcdCACertFile, etcdClientCertFile, etcdClientKeyFile} {
		if _, err := os.Stat(filepath.Join(clientDir, f)); err != nil {
			t.Errorf("client dir missing %q: %v", f, err)
		}
	}

	info, err := os.Stat(filepath.Join(md, etcdServerKeyFile))
	if err != nil {
		t.Fatal(err)
	}

	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("server key must be 0600 (secret), got %o", perm)
	}
}

// TestBuildRunArgs_DatastoreTLS locks the k3s server datastore-TLS recipe (v0.5.0): EVERY server
// (here a non-bootstrap join server) bind-mounts the client bundle and carries the three
// --datastore-*file flags; an agent never does; and an empty DatastoreTLSDir (bring-your-own) emits
// neither (the BVA boundary).
func TestBuildRunArgs_DatastoreTLS(t *testing.T) {
	cfg := recipeCfg()
	cfg.DatastoreEndpoint = "https://aegis-etcd-1.aegis:2379"
	cfg.DatastoreTLSDir = "/abs/state/aegis/etcd-tls/client"

	server := NodeConfig{Name: "aegis-server-2", Role: RoleServer}
	args := buildRunArgs(cfg, server, "", "aegis", "")
	joined := strings.Join(args, " ")

	checks := []struct {
		ok   bool
		desc string
	}{
		{hasPair(args, "--volume", cfg.DatastoreTLSDir+":"+etcdTLSMount+":ro"), "client TLS bundle bind-mounted read-only on the server"},
		{slices.Contains(args, "--datastore-cafile="+etcdTLSMount+"/"+etcdCACertFile), "--datastore-cafile points at the bind-mounted CA"},
		{slices.Contains(args, "--datastore-certfile="+etcdTLSMount+"/"+etcdClientCertFile), "--datastore-certfile points at the client cert"},
		{slices.Contains(args, "--datastore-keyfile="+etcdTLSMount+"/"+etcdClientKeyFile), "--datastore-keyfile points at the client key"},
		{slices.Contains(args, "--datastore-endpoint="+cfg.DatastoreEndpoint), "https datastore endpoint carried"},
	}

	for _, c := range checks {
		if !c.ok {
			t.Errorf("datastore-TLS server check failed: %s\nargs: %s", c.desc, joined)
		}
	}

	// An agent must never get the datastore TLS material.
	agentArgs := buildRunArgs(cfg, NodeConfig{Name: "aegis-agent-1", Role: RoleAgent}, "https://aegis-api.aegis:6443", "aegis", "")
	if strings.Contains(strings.Join(agentArgs, " "), etcdTLSMount) {
		t.Errorf("agent must NOT mount datastore TLS: %v", agentArgs)
	}

	// BVA: empty DatastoreTLSDir (bring-your-own endpoint) emits no mount and no --datastore-*file.
	cfg.DatastoreTLSDir = ""
	plain := strings.Join(buildRunArgs(cfg, server, "", "aegis", ""), " ")
	if strings.Contains(plain, etcdTLSMount) || strings.Contains(plain, "--datastore-cafile") {
		t.Errorf("empty DatastoreTLSDir must emit no TLS mount/flags: %s", plain)
	}
}

// --- Feature 3: datastore tuning ---

// TestBuildEtcdRunArgs_Tuning locks -datastore-image / -datastore-memory: a custom image is the
// positional image and a custom memory is the --memory value; empty image falls back to the pinned
// default (so the existing recipe-lock keeps finding defaultEtcdImage).
func TestBuildEtcdRunArgs_Tuning(t *testing.T) {
	cfg := recipeCfg()
	cfg.DatastoreImage = "quay.io/coreos/etcd:v3.5.21"
	member := NodeConfig{Name: "aegis-etcd-1", Role: RoleDatastore, Memory: 1024 * 1024 * 1024}

	args := buildEtcdRunArgs(cfg, member, "aegis", "x=y", "/abs/tls")

	if !slices.Contains(args, "quay.io/coreos/etcd:v3.5.21") {
		t.Errorf("custom datastore image must be the positional image: %v", args)
	}

	if !hasPair(args, "--memory", "1024MB") {
		t.Errorf("custom datastore memory must be the --memory value: %v", args)
	}

	cfg.DatastoreImage = ""
	if !slices.Contains(buildEtcdRunArgs(cfg, member, "aegis", "x=y", "/abs/tls"), defaultEtcdImage) {
		t.Error("empty DatastoreImage must fall back to defaultEtcdImage")
	}
}

// TestBuildEtcdRunArgs_DomainThreaded locks that the dns domain threads into the container --name
// FQDN for a non-default domain (the FQDN is what container DNS registers; ADR-0003).
func TestBuildEtcdRunArgs_DomainThreaded(t *testing.T) {
	cfg := recipeCfg()
	member := NodeConfig{Name: "aegis-etcd-1", Role: RoleDatastore, Memory: defaultEtcdMemoryBytes}

	args := buildEtcdRunArgs(cfg, member, "k3s", "x=y", "/abs/tls")
	if !hasPair(args, "--name", "aegis-etcd-1.k3s") {
		t.Errorf("container --name must thread the dns domain (.k3s): %v", args)
	}
}

// TestBuildAPILBConfig_ClusterNameInHeader locks that the generated haproxy.cfg names the cluster in
// its header and addresses backends by the dns domain (exercises a non-default cluster name + domain).
func TestBuildAPILBConfig_ClusterNameInHeader(t *testing.T) {
	cfgText := buildAPILBConfig("k3v", []NodeConfig{{Name: "k3v-server-1", Role: RoleServer}}, "k3s")

	if !strings.Contains(cfgText, `cluster "k3v"`) {
		t.Errorf("haproxy.cfg header must name the cluster:\n%s", cfgText)
	}

	if !strings.Contains(cfgText, "k3v-server-1.k3s:6443") {
		t.Errorf("backend must use the dns domain:\n%s", cfgText)
	}
}

// TestEtcdMembers_Memory locks that etcdMembers stamps the supplied per-member memory (the resolved
// -datastore-memory). BVA: the default and a custom value.
func TestEtcdMembers_Memory(t *testing.T) {
	for _, mem := range []int64{defaultEtcdMemoryBytes, 256 * 1024 * 1024} {
		for _, m := range etcdMembers("aegis", 3, mem) {
			if m.Memory != mem {
				t.Errorf("etcdMembers memory: got %d, want %d", m.Memory, mem)
			}
		}
	}
}

// --- Feature 6: env injection ---

// TestBuildRunArgs_EnvInjection is the BVA (CLAUDE.md k) on the -env accumulation: zero (no user
// --env), one, many — on both roles. The built-in K3S_TOKEN env stays regardless.
func TestBuildRunArgs_EnvInjection(t *testing.T) {
	cfg := recipeCfg()

	t.Run("zero: no user env, K3S_TOKEN still present", func(t *testing.T) {
		args := buildRunArgs(cfg, NodeConfig{Name: "aegis-server-1", Role: RoleServer}, "", "aegis", "/abs")
		if !hasPair(args, "--env", "K3S_TOKEN=deadbeef") {
			t.Error("built-in K3S_TOKEN env must remain")
		}

		if hasPair(args, "--env", "HTTP_PROXY=http://proxy:3128") {
			t.Error("no user env expected")
		}
	})

	t.Run("one on a server", func(t *testing.T) {
		cfg.EnvVars = []string{"HTTP_PROXY=http://proxy:3128"}
		args := buildRunArgs(cfg, NodeConfig{Name: "aegis-server-1", Role: RoleServer}, "", "aegis", "/abs")

		if !hasPair(args, "--env", "HTTP_PROXY=http://proxy:3128") {
			t.Errorf("user env must be injected: %v", args)
		}
	})

	t.Run("many on an agent too", func(t *testing.T) {
		cfg.EnvVars = []string{"A=1", "B=2"}
		args := buildRunArgs(cfg, NodeConfig{Name: "aegis-agent-1", Role: RoleAgent}, "https://aegis-api.aegis:6443", "aegis", "")

		if !hasPair(args, "--env", "A=1") || !hasPair(args, "--env", "B=2") {
			t.Errorf("all user envs must reach the agent: %v", args)
		}
	})
}

// --- Feature 2: add-server / LB-backend regeneration ---

// TestEnsureAddServerable guards -add-server: it requires an external datastore endpoint AND an
// existing API LB node. BVA over the three meaningful shapes.
func TestEnsureAddServerable(t *testing.T) {
	lb := NodeInfo{Name: "aegis-api", Role: RoleLB}
	server := NodeInfo{Name: "aegis-server-1", Role: RoleServer}

	tests := []struct {
		name    string
		state   ClusterState
		wantErr bool
	}{
		{"single-server sqlite (no datastore): reject", ClusterState{ClusterName: "aegis", Nodes: []NodeInfo{server}}, true},
		{"HA datastore but no LB (IP-only): reject", ClusterState{ClusterName: "aegis", DatastoreEndpoint: "https://db:2379", Nodes: []NodeInfo{server}}, true},
		{"HA datastore + LB: accept", ClusterState{ClusterName: "aegis", DatastoreEndpoint: "https://db:2379", Nodes: []NodeInfo{server, lb}}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ensureAddServerable(tt.state); (err != nil) != tt.wantErr {
				t.Errorf("wantErr=%v, got %v", tt.wantErr, err)
			}
		})
	}
}

// TestServerConfigsFromState_LBRegen is the core add-server/LB interaction: extracting the server
// set from state and regenerating haproxy.cfg must yield exactly one backend per server. BVA on the
// backend count across an add: 2 servers before, 3 after.
func TestServerConfigsFromState_LBRegen(t *testing.T) {
	mk := func(name string, role Role) NodeInfo { return NodeInfo{Name: name, Role: role} }

	before := []NodeInfo{
		mk("aegis-etcd-1", RoleDatastore),
		mk("aegis-server-1", RoleServer),
		mk("aegis-server-2", RoleServer),
		mk("aegis-api", RoleLB),
		mk("aegis-agent-1", RoleAgent),
	}

	servers := serverConfigsFromState(before)
	if len(servers) != 2 {
		t.Fatalf("serverConfigsFromState: got %d servers, want 2 (datastore/lb/agent excluded)", len(servers))
	}

	cfgText := buildAPILBConfig("aegis", servers, "aegis")
	if got := strings.Count(cfgText, "\n    server srv"); got != 2 {
		t.Errorf("2-server LB config must have 2 backends, got %d", got)
	}

	// Simulate AddServer appending server-3, then regenerating.
	after := append(before, mk("aegis-server-3", RoleServer))

	cfgText3 := buildAPILBConfig("aegis", serverConfigsFromState(after), "aegis")
	if got := strings.Count(cfgText3, "\n    server srv"); got != 3 {
		t.Errorf("after add-server the LB config must have 3 backends, got %d", got)
	}

	if !strings.Contains(cfgText3, "server srv3 aegis-server-3.aegis:6443") {
		t.Errorf("regenerated config must route to the new server FQDN:\n%s", cfgText3)
	}
}

// TestNextServerIndex is the BVA on the server set driving AddServer's naming: 0 servers -> 1,
// contiguous 1,2 -> 3, and a gap 1,3 (a removed server-2) -> 4 (max+1, never backfill).
func TestNextServerIndex(t *testing.T) {
	mk := func(name string, role Role) NodeInfo { return NodeInfo{Name: name, Role: role} }

	tests := []struct {
		name  string
		nodes []NodeInfo
		want  int
	}{
		{"no servers (only etcd): next is 1", []NodeInfo{mk("aegis-etcd-1", RoleDatastore)}, 1},
		{"servers 1,2 contiguous: next is 3", []NodeInfo{mk("aegis-server-1", RoleServer), mk("aegis-server-2", RoleServer)}, 3},
		{"servers 1,3 with a gap: next is 4 (max+1)", []NodeInfo{mk("aegis-server-1", RoleServer), mk("aegis-server-3", RoleServer)}, 4},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := nextServerIndex(tt.nodes, "aegis"); got != tt.want {
				t.Errorf("nextServerIndex = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestExistingDatastoreTLSDir verifies AddServer's reuse of the on-disk client bundle: present ->
// the absolute dir; absent (bring-your-own datastore) -> "".
func TestExistingDatastoreTLSDir(t *testing.T) {
	dir := t.TempDir()

	if got := existingDatastoreTLSDir(dir); got != "" {
		t.Errorf("no client TLS dir on disk must yield \"\", got %q", got)
	}

	clientDir := filepath.Join(dir, etcdTLSSubdir, etcdClientSubdir)
	if err := os.MkdirAll(clientDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if got := existingDatastoreTLSDir(dir); got != clientDir {
		t.Errorf("existing client TLS dir must be returned: got %q want %q", got, clientDir)
	}
}

// --- Feature 5: kubeconfig merge ---

// sampleKubeconfig is a minimal k3s-shaped admin kubeconfig (the format k3ac rewrites/delivers).
const sampleKubeconfig = `apiVersion: v1
clusters:
- cluster:
    certificate-authority-data: Q0FEQVRB
    server: https://aegis-api.aegis:6443
  name: default
contexts:
- context:
    cluster: default
    user: default
  name: default
current-context: default
kind: Config
users:
- name: default
  user:
    client-certificate-data: Q0VSVERBVEE=
    client-key-data: S0VZREFUQQ==
`

// TestParseKubeconfigCreds extracts the server URL and three base64 cert blobs; a kubeconfig
// missing any required field is rejected (BVA: complete vs missing).
// TestKubeconfigTarget covers the success-message target resolution (BVA on $KUBECONFIG): unset
// → default, a single path, and a multi-path list where kubectl writes to the FIRST entry.
func TestKubeconfigTarget(t *testing.T) {
	sep := string(os.PathListSeparator)

	t.Run("unset keeps default", func(t *testing.T) {
		t.Setenv("KUBECONFIG", "")
		if got := kubeconfigTarget(); got != "~/.kube/config" {
			t.Errorf("unset: got %q, want ~/.kube/config", got)
		}
	})

	t.Run("single path", func(t *testing.T) {
		t.Setenv("KUBECONFIG", "/tmp/kc.yaml")
		if got := kubeconfigTarget(); got != "/tmp/kc.yaml" {
			t.Errorf("single: got %q, want /tmp/kc.yaml", got)
		}
	})

	t.Run("multi path uses first", func(t *testing.T) {
		t.Setenv("KUBECONFIG", "/tmp/first.yaml"+sep+"/tmp/second.yaml")
		if got := kubeconfigTarget(); got != "/tmp/first.yaml" {
			t.Errorf("multi: got %q, want /tmp/first.yaml", got)
		}
	})
}

func TestParseKubeconfigCreds(t *testing.T) {
	creds, err := parseKubeconfigCreds([]byte(sampleKubeconfig))
	if err != nil {
		t.Fatalf("parseKubeconfigCreds: %v", err)
	}

	if creds.Server != "https://aegis-api.aegis:6443" {
		t.Errorf("server: got %q", creds.Server)
	}

	if creds.CAData != "Q0FEQVRB" || creds.ClientCertData != "Q0VSVERBVEE=" || creds.ClientKeyData != "S0VZREFUQQ==" {
		t.Errorf("cert blobs not extracted: %+v", creds)
	}

	// Missing client-key-data must be rejected.
	missing := strings.ReplaceAll(sampleKubeconfig, "    client-key-data: S0VZREFUQQ==\n", "")
	if _, err := parseKubeconfigCreds([]byte(missing)); err == nil {
		t.Error("a kubeconfig missing client-key-data must be rejected")
	}
}

// TestMergeKubeconfigCommands locks the four kubectl command vectors: set-cluster (server + CA +
// embed), set-credentials (client cert/key + embed), set-context (cluster+user keyed on the cluster
// name), use-context. The cluster-name keying is what de-collides k3s's fixed "default" entries.
func TestMergeKubeconfigCommands(t *testing.T) {
	files := mergeCertFiles{CA: "/m/ca.crt", ClientCert: "/m/client.crt", ClientKey: "/m/client.key"}
	cmds := mergeKubeconfigCommands("aegis", "https://aegis-api.aegis:6443", files)

	if len(cmds) != 4 {
		t.Fatalf("want 4 kubectl commands, got %d", len(cmds))
	}

	joined := func(v []string) string { return strings.Join(v, " ") }

	checks := []struct {
		got  string
		want []string
	}{
		{joined(cmds[0]), []string{"config set-cluster aegis", "--server=https://aegis-api.aegis:6443", "--certificate-authority=/m/ca.crt", "--embed-certs=true"}},
		{joined(cmds[1]), []string{"config set-credentials aegis", "--client-certificate=/m/client.crt", "--client-key=/m/client.key", "--embed-certs=true"}},
		{joined(cmds[2]), []string{"config set-context aegis", "--cluster=aegis", "--user=aegis"}},
		{joined(cmds[3]), []string{"config use-context aegis"}},
	}

	for i, c := range checks {
		for _, want := range c.want {
			if !strings.Contains(c.got, want) {
				t.Errorf("command %d missing %q\ngot: %s", i, want, c.got)
			}
		}
	}
}
