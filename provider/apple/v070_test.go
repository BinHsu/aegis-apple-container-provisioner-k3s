package apple

import (
	"os"
	"path/filepath"
	"testing"
)

// TestBuildRunArgs_DNSAndCADir locks the v0.7.0 TLS-interception / silent-gateway-DNS options.
func TestBuildRunArgs_DNSAndCADir(t *testing.T) {
	cfg := recipeCfg()
	cfg.DNSServers = []string{"1.1.1.1", "8.8.8.8"}
	cfg.CADir = "/abs/ca"
	node := NodeConfig{Name: "aegis-agent-1", Role: RoleAgent}

	args := buildRunArgs(cfg, node, "https://aegis-server-1.aegis:6443", "aegis", "")

	for _, c := range []struct {
		ok   bool
		what string
	}{
		{hasPair(args, "--dns", "1.1.1.1"), "first --dns"},
		{hasPair(args, "--dns", "8.8.8.8"), "second --dns"},
		{hasPair(args, "--volume", "/abs/ca:"+caMount+":ro"), "CA dir bind-mounted read-only"},
		{hasPair(args, "--env", "SSL_CERT_FILE="+caMount+"/"+caBundleFile), "SSL_CERT_FILE points at the bundle"},
	} {
		if !c.ok {
			t.Errorf("missing: %s; args=%v", c.what, args)
		}
	}
}

// TestBuildRunArgs_NoDNSNoCADirUnchanged: without the options nothing new is emitted (1.0.0 path unchanged).
func TestBuildRunArgs_NoDNSNoCADirUnchanged(t *testing.T) {
	args := buildRunArgs(recipeCfg(), NodeConfig{Name: "aegis-agent-1", Role: RoleAgent}, "https://x:6443", "aegis", "")
	for _, a := range args {
		if a == "--dns" {
			t.Fatalf("unexpected --dns in %v", args)
		}
	}
}

func TestValidateCADir(t *testing.T) {
	if err := validateCADir(""); err != nil {
		t.Fatalf("empty must be allowed: %v", err)
	}

	if err := validateCADir("rel/dir"); err == nil {
		t.Fatal("relative dir must be rejected")
	}

	dir := t.TempDir()
	if err := validateCADir(dir); err == nil {
		t.Fatal("dir without ca-bundle.pem must be rejected")
	}

	if err := os.WriteFile(filepath.Join(dir, caBundleFile), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := validateCADir(dir); err != nil {
		t.Fatalf("valid dir rejected: %v", err)
	}
}
