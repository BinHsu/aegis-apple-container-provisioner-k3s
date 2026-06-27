// SPDX-License-Identifier: MIT

package main

import (
	"encoding/json"
	"fmt"
	"os"
)

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
	// DatastoreEndpoint enables multi-server HA (external datastore; see docs/ADR/0002).
	// "" = absent → single-server embedded sqlite.
	DatastoreEndpoint string `json:"datastoreEndpoint"`
	ServerMemoryMB    int64  `json:"serverMemoryMB"`
	AgentMemoryMB     int64  `json:"agentMemoryMB"`
	// Servers: control-plane node count. Unlike Agents, 0 is NOT a valid shape (a cluster
	// needs at least one server), so absent-from-file (0) leaves the built-in default (1)
	// in place — see applyConfig. Set "servers": N (with "datastoreEndpoint") for HA.
	Servers int `json:"servers"`
	// Agents: 0 is a valid cluster shape (single-server, no agents). A plain int
	// cannot distinguish absent-from-file vs explicitly-zero in JSON — see applyConfig.
	Agents   int    `json:"agents"`
	StateDir string `json:"stateDir"`
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
func applyConfig(
	fc fileConfig, explicit map[string]bool,
	clusterName *string, k3sImage *string, stateDir *string, network *string,
	dnsDomain *string, token *string, datastore *string,
	serverMemMB *int64, agentMemMB *int64, serverCount *int, agentCount *int,
) {
	if !explicit["name"] && fc.Name != "" {
		*clusterName = fc.Name
	}

	// DNSDomain uses a pointer so "" (IP-only) is distinguishable from absent.
	if !explicit["dns-domain"] && fc.DNSDomain != nil {
		*dnsDomain = *fc.DNSDomain
	}

	if !explicit["image"] && fc.Image != "" {
		*k3sImage = fc.Image
	}

	if !explicit["network"] && fc.Network != "" {
		*network = fc.Network
	}

	if !explicit["token"] && fc.Token != "" {
		*token = fc.Token
	}

	if !explicit["datastore-endpoint"] && fc.DatastoreEndpoint != "" {
		*datastore = fc.DatastoreEndpoint
	}

	if !explicit["server-memory"] && fc.ServerMemoryMB != 0 {
		*serverMemMB = fc.ServerMemoryMB
	}

	if !explicit["agent-memory"] && fc.AgentMemoryMB != 0 {
		*agentMemMB = fc.AgentMemoryMB
	}

	// Servers: 0 is NOT a valid cluster shape, so (unlike Agents) absent-from-file (0)
	// leaves the built-in default (1) in place rather than overriding it to 0.
	if !explicit["servers"] && fc.Servers != 0 {
		*serverCount = fc.Servers
	}

	// Agents: always apply from file (even 0) unless the flag was explicitly set.
	// Documented trade-off: if "agents" is absent from the file, the decoder leaves
	// it as 0, so the built-in default (1) is replaced by 0. Always declare "agents"
	// in a config file if you want a specific count.
	if !explicit["agents"] {
		*agentCount = fc.Agents
	}

	if !explicit["state-dir"] && fc.StateDir != "" {
		*stateDir = fc.StateDir
	}
}
