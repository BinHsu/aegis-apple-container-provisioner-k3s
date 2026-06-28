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
		"datastoreMembers": 5,
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

	if fc.DatastoreMembers != 5 {
		t.Errorf("DatastoreMembers: got %d, want 5", fc.DatastoreMembers)
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

// applyInputs bundles the pointer targets applyConfig writes into, so each test states
// only the values it cares about and the call site stays readable after the signature grew
// a datastore-members parameter. Defaults mirror the cmd/k3ac flag defaults.
type applyInputs struct {
	name, image, stateDir, network, dns, token, datastore string
	serverMem, agentMem                                   int64
	servers, agents, datastoreMembers                     int
}

func defaultApplyInputs() applyInputs {
	return applyInputs{
		name: "default-name", image: "", stateDir: "_out/clusters", network: "default",
		dns: "aegis", token: "", datastore: "",
		serverMem: 2048, agentMem: 2048,
		servers: 1, agents: 1, datastoreMembers: 3,
	}
}

// apply runs applyConfig against in's fields and returns the mutated copy.
func apply(fc fileConfig, explicit map[string]bool, in applyInputs) applyInputs {
	applyConfig(fc, explicit,
		&in.name, &in.image, &in.stateDir, &in.network, &in.dns, &in.token, &in.datastore,
		&in.serverMem, &in.agentMem, &in.servers, &in.agents, &in.datastoreMembers)

	return in
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
		DatastoreMembers:  5,
		ServerMemoryMB:    3072,
		AgentMemoryMB:     2048,
		Servers:           2,
		Agents:            3,
		StateDir:          "file-state",
	}

	got := apply(fc, map[string]bool{}, defaultApplyInputs())

	if got.name != "from-file" {
		t.Errorf("name: got %q, want %q", got.name, "from-file")
	}

	if got.dns != "k3s" {
		t.Errorf("dns: got %q, want %q", got.dns, "k3s")
	}

	if got.image != "rancher/k3s:v1.0.0" {
		t.Errorf("image: got %q, want %q", got.image, "rancher/k3s:v1.0.0")
	}

	if got.network != "custom" {
		t.Errorf("network: got %q, want %q", got.network, "custom")
	}

	if got.token != "file-token" {
		t.Errorf("token: got %q, want %q", got.token, "file-token")
	}

	if got.serverMem != 3072 {
		t.Errorf("serverMemMB: got %d, want 3072", got.serverMem)
	}

	if got.agentMem != 2048 {
		t.Errorf("agentMemMB: got %d, want 2048", got.agentMem)
	}

	if got.agents != 3 {
		t.Errorf("agents: got %d, want 3", got.agents)
	}

	if got.datastore != "postgres://kine:pw@db.aegis:5432/kine" {
		t.Errorf("datastore: got %q", got.datastore)
	}

	if got.datastoreMembers != 5 {
		t.Errorf("datastoreMembers: got %d, want 5", got.datastoreMembers)
	}

	if got.servers != 2 {
		t.Errorf("servers: got %d, want 2", got.servers)
	}

	if got.stateDir != "file-state" {
		t.Errorf("stateDir: got %q, want %q", got.stateDir, "file-state")
	}
}

// TestApplyConfig_ExplicitFlagWins verifies that an explicit command-line flag
// takes precedence over the matching value in the config file — the top of the
// precedence chain.
func TestApplyConfig_ExplicitFlagWins(t *testing.T) {
	fileDomain := "file-domain"
	fc := fileConfig{
		Name:             "file-name",
		DNSDomain:        &fileDomain,
		Image:            "rancher/k3s:v1.0.0",
		Agents:           5,
		DatastoreMembers: 5,
	}

	// Simulate -name, -dns-domain, -image, -agents, -datastore-members all set by the user.
	explicit := map[string]bool{
		"name":              true,
		"dns-domain":        true,
		"image":             true,
		"agents":            true,
		"datastore-members": true,
	}

	in := defaultApplyInputs()
	in.name, in.image, in.dns, in.stateDir = "flag-name", "rancher/k3s:v2.0.0", "flag-domain", "_out"
	in.agents, in.datastoreMembers = 2, 3
	got := apply(fc, explicit, in)

	if got.name != "flag-name" {
		t.Errorf("name: explicit flag must win over file, got %q", got.name)
	}

	if got.dns != "flag-domain" {
		t.Errorf("dns-domain: explicit flag must win over file, got %q", got.dns)
	}

	if got.image != "rancher/k3s:v2.0.0" {
		t.Errorf("image: explicit flag must win over file, got %q", got.image)
	}

	if got.agents != 2 {
		t.Errorf("agents: explicit flag must win over file, got %d", got.agents)
	}

	if got.datastoreMembers != 3 {
		t.Errorf("datastore-members: explicit flag must win over file, got %d", got.datastoreMembers)
	}
}

// TestApplyConfig_DNSDomainIPOnlyMode verifies the IP-only mode contract:
// a non-nil pointer to "" in the file (key present, value "") sets dnsDomain
// to "", overriding the "aegis" default and selecting IP-only mode.
func TestApplyConfig_DNSDomainIPOnlyMode(t *testing.T) {
	empty := ""
	fc := fileConfig{DNSDomain: &empty}

	got := apply(fc, map[string]bool{}, defaultApplyInputs())

	if got.dns != "" {
		t.Errorf("IP-only mode: dns must be \"\", got %q", got.dns)
	}
}

// TestApplyConfig_AbsentDNSDomainKeepsDefault verifies that a nil DNSDomain (the
// "dnsDomain" key is absent from the file) leaves the flag default ("aegis") intact.
func TestApplyConfig_AbsentDNSDomainKeepsDefault(t *testing.T) {
	fc := fileConfig{} // DNSDomain nil — key absent from file

	got := apply(fc, map[string]bool{}, defaultApplyInputs())

	if got.dns != "aegis" {
		t.Errorf("absent dnsDomain: default must be preserved, got %q", got.dns)
	}
}

// TestApplyConfig_AgentsZeroOverridesDefault verifies the documented trade-off:
// when -config is used and -agents is not set explicitly, fc.Agents (even 0) always
// overrides the built-in default of 1. 0 is a valid cluster shape (single-server,
// no agents). Plain int cannot distinguish absent-from-file from "agents":0, so the
// rule is: the file is the source of truth — always write "agents": N in the file.
func TestApplyConfig_AgentsZeroOverridesDefault(t *testing.T) {
	fc := fileConfig{Agents: 0} // either "agents":0 in file or key absent

	got := apply(fc, map[string]bool{}, defaultApplyInputs())

	// fc.Agents=0 must override the default (1) because -agents was not explicit.
	if got.agents != 0 {
		t.Errorf("agents from file (0) must override default (1) when -agents not explicit, got %d", got.agents)
	}
}

// TestApplyConfig_ServersAbsentKeepsDefault locks the deliberate asymmetry from Agents:
// because 0 servers is NOT a valid cluster shape, an absent "servers" key (decoded as 0)
// must NOT override the built-in default of 1 — unlike Agents, where 0 is meaningful and
// does override. This guards against a config file silently producing a 0-server request.
func TestApplyConfig_ServersAbsentKeepsDefault(t *testing.T) {
	fc := fileConfig{Servers: 0} // "servers" key absent → decoded as 0

	got := apply(fc, map[string]bool{}, defaultApplyInputs())

	if got.servers != 1 {
		t.Errorf("absent servers key must preserve default (1), got %d", got.servers)
	}
}

// TestApplyConfig_ServersFromFile verifies an explicit "servers": N in the file is applied
// when -servers was not set on the command line (the HA-from-config path).
func TestApplyConfig_ServersFromFile(t *testing.T) {
	fc := fileConfig{Servers: 3}

	got := apply(fc, map[string]bool{}, defaultApplyInputs())

	if got.servers != 3 {
		t.Errorf("servers from file (3) must override default (1), got %d", got.servers)
	}
}

// TestApplyConfig_DatastoreMembersAbsentKeepsDefault mirrors the Servers asymmetry for the
// managed etcd cluster size: 0 is NOT a valid member count (must be odd >=3), so an absent
// "datastoreMembers" key (decoded as 0) must NOT override the built-in default of 3.
func TestApplyConfig_DatastoreMembersAbsentKeepsDefault(t *testing.T) {
	fc := fileConfig{DatastoreMembers: 0} // key absent → decoded as 0

	got := apply(fc, map[string]bool{}, defaultApplyInputs())

	if got.datastoreMembers != 3 {
		t.Errorf("absent datastoreMembers key must preserve default (3), got %d", got.datastoreMembers)
	}
}

// TestApplyConfig_DatastoreMembersFromFile verifies an explicit "datastoreMembers": 5 in the
// file is applied when -datastore-members was not set on the command line (HA-from-config).
func TestApplyConfig_DatastoreMembersFromFile(t *testing.T) {
	fc := fileConfig{DatastoreMembers: 5}

	got := apply(fc, map[string]bool{}, defaultApplyInputs())

	if got.datastoreMembers != 5 {
		t.Errorf("datastoreMembers from file (5) must override default (3), got %d", got.datastoreMembers)
	}
}
