// SPDX-License-Identifier: MIT

package apple

import "net/netip"

// This file defines the launcher's own request/state types. The Talos sibling got
// these for free from siderolabs/talos/pkg/provision (ClusterRequest, NodeRequest,
// NodeInfo, ClusterInfo). k3s has no such framework, so we define the minimal
// equivalents here.

// Role is a k3s node role. k3s nodes are either a server (control plane + datastore)
// or an agent (worker). This replaces the Talos sibling's machine.Type.
type Role int

const (
	// RoleServer is a k3s server node (runs the API server and owns the datastore).
	RoleServer Role = iota
	// RoleAgent is a k3s agent node (worker; joins via K3S_URL + K3S_TOKEN).
	RoleAgent
)

// String renders the role as the k3s subcommand string ("server" / "agent"), which
// is exactly what gets appended to `container run ... <image> <role>`.
func (r Role) String() string {
	switch r {
	case RoleServer:
		return "server"
	case RoleAgent:
		return "agent"
	default:
		return "unknown"
	}
}

// NodeConfig describes one k3s node to launch. Memory is in bytes (converted to the
// CLI's required "MB" suffix in buildRunArgs); NanoCPUs mirrors the Talos sibling's
// nano-CPU unit so the cmd driver code reads the same.
type NodeConfig struct {
	Name     string
	Role     Role
	Memory   int64 // bytes
	NanoCPUs int64 // nano-CPUs (1e9 == 1 vCPU)
}

// ClusterConfig is the full request to Create.
type ClusterConfig struct {
	// Name is the cluster name; also the per-cluster state-dir and label value.
	Name string
	// Image is the rancher/k3s image (see node.go defaultK3sImage for the pinned tag).
	Image string
	// Network is the apple/container network name ("" or "default" = built-in).
	Network string
	// StateDir is the host directory under which state.json lives. Named volumes (not
	// host directories) back the per-node k3s datastore — see node.go nodeVolumeName.
	StateDir string
	// Token is the shared K3S_TOKEN. Generated with crypto/rand if empty (see token.go).
	Token string
	// Nodes are the nodes to launch (server first is enforced by Create's ordering).
	Nodes []NodeConfig
}

// NodeInfo is a launched node's discovered state.
type NodeInfo struct {
	ID   string       `json:"id"`   // container name (FQDN when dns-domain is set)
	Name string       `json:"name"` // bare node name; used for volume naming in Destroy
	Role Role         `json:"role"`
	IPs  []netip.Addr `json:"ips"`
}

// ClusterState is what Create persists to <statedir>/<name>/state.json and Destroy
// reads back. The Talos sibling got persistence from provision.State; here it is a
// plain JSON file.
type ClusterState struct {
	Provisioner string `json:"provisioner"`
	ClusterName string `json:"clusterName"`
	Network     string `json:"network"`
	Token       string `json:"token"`
	StateDir    string `json:"stateDir"`
	// Image is the RESOLVED rancher/k3s image the cluster was created with (the
	// empty->defaultK3sImage step already applied). Persisted so a later AddAgents
	// launches new agents on the exact same image as the original nodes. Pre-v0.2.0
	// states predate this field and unmarshal to ""; AddAgents falls back to
	// defaultK3sImage in that case.
	Image     string     `json:"image"`
	ServerURL string     `json:"serverURL"` // https://<server-fqdn>:6443 (FQDN or IP)
	Nodes     []NodeInfo `json:"nodes"`
}
