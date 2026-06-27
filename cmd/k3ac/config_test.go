// SPDX-License-Identifier: MIT

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadFileConfig_ValidFile verifies that a complete, well-formed config file
// is parsed into the expected struct. In particular, "dnsDomain": "" (IP-only mode)
// must produce a non-nil pointer to "" — distinguishable from a key that is absent.
func TestLoadFileConfig_ValidFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cluster.json")

	content := `{
		"name": "test-cluster",
		"dnsDomain": "",
		"image": "rancher/k3s:v1.32.5-k3s1",
		"network": "default",
		"token": "secret",
		"datastoreEndpoint": "postgres://kine:pw@db.aegis:5432/kine",
		"serverMemoryMB": 1536,
		"agentMemoryMB": 1024,
		"servers": 2,
		"agents": 2,
		"stateDir": "_out/clusters"
	}`

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	fc, err := loadFileConfig(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if fc.Name != "test-cluster" {
		t.Errorf("Name: got %q, want %q", fc.Name, "test-cluster")
	}

	// "dnsDomain": "" must produce a non-nil pointer to "" (IP-only mode).
	if fc.DNSDomain == nil {
		t.Fatal("DNSDomain: got nil, want non-nil pointer to \"\" (IP-only mode)")
	}

	if *fc.DNSDomain != "" {
		t.Errorf("DNSDomain: got %q, want \"\"", *fc.DNSDomain)
	}

	if fc.Image != "rancher/k3s:v1.32.5-k3s1" {
		t.Errorf("Image: got %q, want %q", fc.Image, "rancher/k3s:v1.32.5-k3s1")
	}

	if fc.Network != "default" {
		t.Errorf("Network: got %q, want %q", fc.Network, "default")
	}

	if fc.Token != "secret" {
		t.Errorf("Token: got %q, want %q", fc.Token, "secret")
	}

	if fc.ServerMemoryMB != 1536 {
		t.Errorf("ServerMemoryMB: got %d, want 1536", fc.ServerMemoryMB)
	}

	if fc.AgentMemoryMB != 1024 {
		t.Errorf("AgentMemoryMB: got %d, want 1024", fc.AgentMemoryMB)
	}

	if fc.Agents != 2 {
		t.Errorf("Agents: got %d, want 2", fc.Agents)
	}

	if fc.DatastoreEndpoint != "postgres://kine:pw@db.aegis:5432/kine" {
		t.Errorf("DatastoreEndpoint: got %q", fc.DatastoreEndpoint)
	}

	if fc.Servers != 2 {
		t.Errorf("Servers: got %d, want 2", fc.Servers)
	}

	if fc.StateDir != "_out/clusters" {
		t.Errorf("StateDir: got %q, want %q", fc.StateDir, "_out/clusters")
	}
}

// TestLoadFileConfig_UnknownField verifies that DisallowUnknownFields causes an
// unknown key to return a clear error rather than being silently ignored.
// A typo like "serverMemoryMb" would otherwise produce a baffling "why is my
// flag ignored?" failure at cluster create time.
func TestLoadFileConfig_UnknownField(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")

	if err := os.WriteFile(path, []byte(`{"name": "test", "unknownKey": "oops"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := loadFileConfig(path); err == nil {
		t.Error("unknown JSON key must return an error (DisallowUnknownFields)")
	}
}

// TestLoadFileConfig_AbsentDNSDomain verifies that a config file that does not
// include the "dnsDomain" key leaves DNSDomain nil — meaning "absent from file,
// preserve the flag default". This is the nil vs non-nil pointer contract.
func TestLoadFileConfig_AbsentDNSDomain(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "no-dns.json")

	if err := os.WriteFile(path, []byte(`{"name": "test"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	fc, err := loadFileConfig(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if fc.DNSDomain != nil {
		t.Errorf("absent dnsDomain key must produce nil pointer, got %v", fc.DNSDomain)
	}
}

// TestLoadFileConfig_MissingFile verifies that an OS-level open error (file not
// found) is surfaced rather than silently swallowed.
func TestLoadFileConfig_MissingFile(t *testing.T) {
	if _, err := loadFileConfig("/nonexistent/path/cluster.json"); err == nil {
		t.Error("missing file must return an error")
	}
}

// TestApplyConfig_FilePrecedence verifies the core merge rule: a value present in
// the config file is applied when the matching flag was NOT set explicitly.
// This covers the common "operator runs k3ac -config cluster.json" path.
func TestApplyConfig_FilePrecedence(t *testing.T) {
	domain := "k3s"
	fc := fileConfig{
		Name:              "from-file",
		DNSDomain:         &domain,
		Image:             "rancher/k3s:v1.0.0",
		Network:           "custom",
		Token:             "file-token",
		DatastoreEndpoint: "postgres://kine:pw@db.aegis:5432/kine",
		ServerMemoryMB:    3072,
		AgentMemoryMB:     2048,
		Servers:           2,
		Agents:            3,
		StateDir:          "file-state",
	}

	// No explicit flags — every field should be taken from the file.
	name := "default-name"
	image := ""
	stateDir := "_out/clusters"
	network := "default"
	dns := "aegis"
	token := ""
	datastore := ""
	serverMem := int64(2048)
	agentMem := int64(2048)
	servers := 1
	agents := 1

	applyConfig(fc, map[string]bool{},
		&name, &image, &stateDir, &network, &dns, &token, &datastore, &serverMem, &agentMem, &servers, &agents)

	if name != "from-file" {
		t.Errorf("name: got %q, want %q", name, "from-file")
	}

	if dns != "k3s" {
		t.Errorf("dns: got %q, want %q", dns, "k3s")
	}

	if image != "rancher/k3s:v1.0.0" {
		t.Errorf("image: got %q, want %q", image, "rancher/k3s:v1.0.0")
	}

	if network != "custom" {
		t.Errorf("network: got %q, want %q", network, "custom")
	}

	if token != "file-token" {
		t.Errorf("token: got %q, want %q", token, "file-token")
	}

	if serverMem != 3072 {
		t.Errorf("serverMemMB: got %d, want 3072", serverMem)
	}

	if agentMem != 2048 {
		t.Errorf("agentMemMB: got %d, want 2048", agentMem)
	}

	if agents != 3 {
		t.Errorf("agents: got %d, want 3", agents)
	}

	if datastore != "postgres://kine:pw@db.aegis:5432/kine" {
		t.Errorf("datastore: got %q", datastore)
	}

	if servers != 2 {
		t.Errorf("servers: got %d, want 2", servers)
	}

	if stateDir != "file-state" {
		t.Errorf("stateDir: got %q, want %q", stateDir, "file-state")
	}
}

// TestApplyConfig_ExplicitFlagWins verifies that an explicit command-line flag
// takes precedence over the matching value in the config file — the top of the
// precedence chain.
func TestApplyConfig_ExplicitFlagWins(t *testing.T) {
	fileDomain := "file-domain"
	fc := fileConfig{
		Name:      "file-name",
		DNSDomain: &fileDomain,
		Image:     "rancher/k3s:v1.0.0",
		Agents:    5,
	}

	// Simulate -name, -dns-domain, -image, -agents all set by the user.
	explicit := map[string]bool{
		"name":       true,
		"dns-domain": true,
		"image":      true,
		"agents":     true,
	}

	name := "flag-name"
	image := "rancher/k3s:v2.0.0"
	stateDir := "_out"
	network := "default"
	dns := "flag-domain"
	token := ""
	datastore := ""
	serverMem := int64(2048)
	agentMem := int64(2048)
	servers := 1
	agents := 2

	applyConfig(fc, explicit,
		&name, &image, &stateDir, &network, &dns, &token, &datastore, &serverMem, &agentMem, &servers, &agents)

	if name != "flag-name" {
		t.Errorf("name: explicit flag must win over file, got %q", name)
	}

	if dns != "flag-domain" {
		t.Errorf("dns-domain: explicit flag must win over file, got %q", dns)
	}

	if image != "rancher/k3s:v2.0.0" {
		t.Errorf("image: explicit flag must win over file, got %q", image)
	}

	if agents != 2 {
		t.Errorf("agents: explicit flag must win over file, got %d", agents)
	}
}

// TestApplyConfig_DNSDomainIPOnlyMode verifies the IP-only mode contract:
// a non-nil pointer to "" in the file (key present, value "") sets dnsDomain
// to "", overriding the "aegis" default and selecting IP-only mode.
func TestApplyConfig_DNSDomainIPOnlyMode(t *testing.T) {
	empty := ""
	fc := fileConfig{DNSDomain: &empty}

	dns := "aegis" // flag default
	name, image, stateDir, network, token := "x", "", "_out", "default", ""
	datastore, serverMem, agentMem, servers, agents := "", int64(2048), int64(2048), 1, 1

	applyConfig(fc, map[string]bool{},
		&name, &image, &stateDir, &network, &dns, &token, &datastore, &serverMem, &agentMem, &servers, &agents)

	if dns != "" {
		t.Errorf("IP-only mode: dns must be \"\", got %q", dns)
	}
}

// TestApplyConfig_AbsentDNSDomainKeepsDefault verifies that a nil DNSDomain (the
// "dnsDomain" key is absent from the file) leaves the flag default ("aegis") intact.
func TestApplyConfig_AbsentDNSDomainKeepsDefault(t *testing.T) {
	fc := fileConfig{} // DNSDomain nil — key absent from file

	dns := "aegis" // flag default
	name, image, stateDir, network, token := "x", "", "_out", "default", ""
	datastore, serverMem, agentMem, servers, agents := "", int64(2048), int64(2048), 1, 1

	applyConfig(fc, map[string]bool{},
		&name, &image, &stateDir, &network, &dns, &token, &datastore, &serverMem, &agentMem, &servers, &agents)

	if dns != "aegis" {
		t.Errorf("absent dnsDomain: default must be preserved, got %q", dns)
	}
}

// TestApplyConfig_AgentsZeroOverridesDefault verifies the documented trade-off:
// when -config is used and -agents is not set explicitly, fc.Agents (even 0) always
// overrides the built-in default of 1. 0 is a valid cluster shape (single-server,
// no agents). Plain int cannot distinguish absent-from-file from "agents":0, so the
// rule is: the file is the source of truth — always write "agents": N in the file.
func TestApplyConfig_AgentsZeroOverridesDefault(t *testing.T) {
	fc := fileConfig{Agents: 0} // either "agents":0 in file or key absent

	name, image, stateDir, network, dns, token := "x", "", "_out", "default", "aegis", ""
	datastore := ""
	serverMem, agentMem := int64(2048), int64(2048)
	servers := 1
	agents := 1 // flag default

	applyConfig(fc, map[string]bool{},
		&name, &image, &stateDir, &network, &dns, &token, &datastore, &serverMem, &agentMem, &servers, &agents)

	// fc.Agents=0 must override the default (1) because -agents was not explicit.
	if agents != 0 {
		t.Errorf("agents from file (0) must override default (1) when -agents not explicit, got %d", agents)
	}
}

// TestApplyConfig_ServersAbsentKeepsDefault locks the deliberate asymmetry from Agents:
// because 0 servers is NOT a valid cluster shape, an absent "servers" key (decoded as 0)
// must NOT override the built-in default of 1 — unlike Agents, where 0 is meaningful and
// does override. This guards against a config file silently producing a 0-server request.
func TestApplyConfig_ServersAbsentKeepsDefault(t *testing.T) {
	fc := fileConfig{Servers: 0} // "servers" key absent → decoded as 0

	name, image, stateDir, network, dns, token := "x", "", "_out", "default", "aegis", ""
	datastore := ""
	serverMem, agentMem := int64(2048), int64(2048)
	servers := 1 // flag default
	agents := 1

	applyConfig(fc, map[string]bool{},
		&name, &image, &stateDir, &network, &dns, &token, &datastore, &serverMem, &agentMem, &servers, &agents)

	if servers != 1 {
		t.Errorf("absent servers key must preserve default (1), got %d", servers)
	}
}

// TestApplyConfig_ServersFromFile verifies an explicit "servers": N in the file is applied
// when -servers was not set on the command line (the HA-from-config path).
func TestApplyConfig_ServersFromFile(t *testing.T) {
	fc := fileConfig{Servers: 3}

	name, image, stateDir, network, dns, token := "x", "", "_out", "default", "aegis", ""
	datastore := ""
	serverMem, agentMem := int64(2048), int64(2048)
	servers := 1 // flag default
	agents := 1

	applyConfig(fc, map[string]bool{},
		&name, &image, &stateDir, &network, &dns, &token, &datastore, &serverMem, &agentMem, &servers, &agents)

	if servers != 3 {
		t.Errorf("servers from file (3) must override default (1), got %d", servers)
	}
}
