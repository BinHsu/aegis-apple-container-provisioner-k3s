// SPDX-License-Identifier: MIT

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// stringList is a flag.Value that ACCUMULATES every occurrence of a repeated flag into a
// slice, so -k3s-server-arg / -k3s-agent-arg / -node-label / -manifest can each be passed more
// than once. Each Set appends one value; flag.Visit reports the flag name once if it was given
// at least once, which is exactly the "explicit" signal applyConfig needs (see precedence).
type stringList []string

// String renders the accumulated values; required by flag.Value. Comma-joined for help/echo.
func (s *stringList) String() string { return strings.Join(*s, ",") }

// Set appends one occurrence of the flag's value (the repeated-flag behaviour).
func (s *stringList) Set(v string) error {
	*s = append(*s, v)

	return nil
}

// fileConfig is the schema for a -config JSON cluster specification.
// It mirrors the create-time flags so a forker can describe a cluster once
// rather than repeating a long flag list on every invocation.
//
// DNSDomain is a pointer because "" (empty string) carries semantic meaning:
// it selects IP-only mode (no stable FQDN, falls back to container IP).
// nil means "key absent from the file — leave the flag default in effect".
// All other fields treat the JSON zero value ("", 0) as "not specified";
// see applyConfig for the full precedence rules and the Agents special case.
type fileConfig struct {
	Name      string  `json:"name"`
	DNSDomain *string `json:"dnsDomain"` // nil = absent; non-nil "" = IP-only mode
	Image     string  `json:"image"`
	Network   string  `json:"network"`
	Token     string  `json:"token"`
	// DatastoreEndpoint enables multi-server HA with a bring-your-own external datastore (see
	// docs/ADR/0002). "" = absent → single-server embedded sqlite, OR the managed etcd path when
	// Servers>1 (docs/ADR-0003).
	DatastoreEndpoint string `json:"datastoreEndpoint"`
	// DatastoreMembers is the managed etcd cluster size (odd, >=3) for the auto-provisioned HA
	// datastore. 0 = absent → the built-in default (3) stays in effect (see applyConfig). Only
	// meaningful when Servers>1 and DatastoreEndpoint is empty.
	DatastoreMembers int   `json:"datastoreMembers"`
	ServerMemoryMB   int64 `json:"serverMemoryMB"`
	AgentMemoryMB    int64 `json:"agentMemoryMB"`
	// Servers: control-plane node count. Unlike Agents, 0 is NOT a valid shape (a cluster
	// needs at least one server), so absent-from-file (0) leaves the built-in default (1)
	// in place — see applyConfig. Set "servers": N (with "datastoreEndpoint") for HA.
	Servers int `json:"servers"`
	// Agents: 0 is a valid cluster shape (single-server, no agents). A plain int
	// cannot distinguish absent-from-file vs explicitly-zero in JSON — see applyConfig.
	Agents   int    `json:"agents"`
	StateDir string `json:"stateDir"`
	// ServerCPUs / AgentCPUs are per-node vCPU counts (v0.4.0). 0 = "not specified" → the
	// built-in default (2) stays in effect, same zero-value handling as the memory fields.
	ServerCPUs int `json:"serverCPUs"`
	AgentCPUs  int `json:"agentCPUs"`
	// K3sServerArgs / K3sAgentArgs are extra k3s flags appended VERBATIM to the server / agent
	// subcommand (v0.4.0; the -k3s-server-arg / -k3s-agent-arg passthrough). Empty/absent = none.
	K3sServerArgs []string `json:"k3sServerArgs"`
	K3sAgentArgs  []string `json:"k3sAgentArgs"`
	// NodeLabels are k3s node labels (KEY=VALUE) applied to every node at create (v0.4.0).
	NodeLabels []string `json:"nodeLabels"`
	// Manifests are host-side manifest file paths auto-deployed via the bootstrap server (v0.4.0).
	Manifests []string `json:"manifests"`
}

// flagRefs bundles pointers to every flag variable applyConfig may overwrite. It replaces the
// former long positional parameter list (the handoff flagged that ~13 positional args had to
// become a struct before v0.4.0 added more): grouping them keeps applyConfig's signature stable
// as config-backed flags grow, and makes the call site self-documenting (field: value) instead
// of a wall of unlabeled &vars where an argument-order mistake would silently misroute a flag.
type flagRefs struct {
	clusterName *string
	image       *string
	stateDir    *string
	network     *string
	dnsDomain   *string
	token       *string
	datastore   *string
	serverMemMB *int64
	agentMemMB  *int64
	serverCount *int
	agentCount  *int
	// datastoreMembers is the managed etcd cluster size (v0.3.0); folded into the struct during
	// the v0.4.0 rebase so the applyConfig refactor and the etcd HA path coexist.
	datastoreMembers *int
	serverCPUs       *int
	agentCPUs        *int
	serverArgs       *stringList
	agentArgs        *stringList
	nodeLabels       *stringList
	manifests        *stringList
}

// loadFileConfig reads a JSON cluster specification from path and decodes it into
// a fileConfig. Unknown JSON keys are rejected via DisallowUnknownFields so a typo
// like "serverMemoryMb" produces a clear error rather than being silently ignored.
func loadFileConfig(path string) (fileConfig, error) {
	f, err := os.Open(path)
	if err != nil {
		return fileConfig{}, fmt.Errorf("opening config file %q: %w", path, err)
	}
	defer f.Close() //nolint:errcheck

	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()

	var fc fileConfig
	if err := dec.Decode(&fc); err != nil {
		return fileConfig{}, fmt.Errorf("parsing config file %q: %w", path, err)
	}

	return fc, nil
}

// applyConfig copies config-file values into the flag variables for any flag the
// user did NOT set explicitly on the command line.
//
// explicit is built by the caller from flag.Visit (which visits only the flags the
// user actually passed), so this function is pure and testable without touching the
// flag package.
//
// Precedence: defaults < config file < explicit flags.
//
// Per-field zero-value handling:
//   - string fields (name, image, network, token, stateDir): "" = "not specified" → not applied.
//   - int64 fields (serverMemoryMB, agentMemoryMB): 0 = "not specified" → not applied.
//   - DNSDomain (*string): nil = absent (key not in file); non-nil "" = IP-only mode.
//     The pointer is the only way to distinguish these two cases with encoding/json.
//   - Agents (int): 0 IS valid (single-server cluster). Because a plain int can't
//     distinguish absent-from-file from explicitly-zero, the rule is: whenever -config
//     is supplied and -agents was not set explicitly, the file value (even 0) overrides
//     the built-in default (1). Include "agents": N in every config file you write.
func applyConfig(fc fileConfig, explicit map[string]bool, r flagRefs) {
	if !explicit["name"] && fc.Name != "" {
		*r.clusterName = fc.Name
	}

	// DNSDomain uses a pointer so "" (IP-only) is distinguishable from absent.
	if !explicit["dns-domain"] && fc.DNSDomain != nil {
		*r.dnsDomain = *fc.DNSDomain
	}

	if !explicit["image"] && fc.Image != "" {
		*r.image = fc.Image
	}

	if !explicit["network"] && fc.Network != "" {
		*r.network = fc.Network
	}

	if !explicit["token"] && fc.Token != "" {
		*r.token = fc.Token
	}

	if !explicit["datastore-endpoint"] && fc.DatastoreEndpoint != "" {
		*r.datastore = fc.DatastoreEndpoint
	}

	if !explicit["server-memory"] && fc.ServerMemoryMB != 0 {
		*r.serverMemMB = fc.ServerMemoryMB
	}

	if !explicit["agent-memory"] && fc.AgentMemoryMB != 0 {
		*r.agentMemMB = fc.AgentMemoryMB
	}

	// Servers: 0 is NOT a valid cluster shape, so (unlike Agents) absent-from-file (0)
	// leaves the built-in default (1) in place rather than overriding it to 0.
	if !explicit["servers"] && fc.Servers != 0 {
		*r.serverCount = fc.Servers
	}

	// DatastoreMembers: like Servers, 0 is NOT a valid value (the managed etcd cluster must be
	// odd and >=3), so absent-from-file (0) keeps the built-in default (3); only a non-zero file
	// value overrides it. An invalid non-zero value is rejected later by the provider.
	if !explicit["datastore-members"] && fc.DatastoreMembers != 0 {
		*r.datastoreMembers = fc.DatastoreMembers
	}

	// Agents: always apply from file (even 0) unless the flag was explicitly set.
	// Documented trade-off: if "agents" is absent from the file, the decoder leaves
	// it as 0, so the built-in default (1) is replaced by 0. Always declare "agents"
	// in a config file if you want a specific count.
	if !explicit["agents"] {
		*r.agentCount = fc.Agents
	}

	if !explicit["state-dir"] && fc.StateDir != "" {
		*r.stateDir = fc.StateDir
	}

	applyV040Config(fc, explicit, r)
}

// applyV040Config applies the v0.4.0 config-backed flags (per-node CPUs and the four repeated
// lists). Split out of applyConfig so neither function trips the cognitive-complexity gate; the
// precedence rules are identical to the rest of applyConfig (file value used only when the
// matching flag was not set explicitly).
func applyV040Config(fc fileConfig, explicit map[string]bool, r flagRefs) {
	// CPUs: 0 = "not specified" → keep the built-in default (2), mirroring the memory fields
	// (a 0-vCPU node makes no sense, so 0 never overrides).
	if !explicit["server-cpus"] && fc.ServerCPUs != 0 {
		*r.serverCPUs = fc.ServerCPUs
	}

	if !explicit["agent-cpus"] && fc.AgentCPUs != 0 {
		*r.agentCPUs = fc.AgentCPUs
	}

	// Repeated-list fields: a non-empty file list overrides the (empty) default when the
	// matching repeatable flag was not given. len == 0 = "not specified" → not applied.
	if !explicit["k3s-server-arg"] && len(fc.K3sServerArgs) > 0 {
		*r.serverArgs = fc.K3sServerArgs
	}

	if !explicit["k3s-agent-arg"] && len(fc.K3sAgentArgs) > 0 {
		*r.agentArgs = fc.K3sAgentArgs
	}

	if !explicit["node-label"] && len(fc.NodeLabels) > 0 {
		*r.nodeLabels = fc.NodeLabels
	}

	if !explicit["manifest"] && len(fc.Manifests) > 0 {
		*r.manifests = fc.Manifests
	}
}
