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
	// RoleDatastore is the managed external datastore (Postgres) micro-VM that backs an HA
	// control plane (docs/ADR/0002). It is NOT a k3s node: it never goes through buildRunArgs
	// or the k3s subcommand path. It is provisioned by Create when ManageDatastore is set and
	// tracked in ClusterState.Nodes so the teardown label sweep reclaims it.
	RoleDatastore
)

// String renders the role. For server/agent it is the k3s subcommand appended to
// `container run ... <image> <role>`; "datastore" is a label value only (a RoleDatastore
// node never reaches the k3s subcommand path).
func (r Role) String() string {
	switch r {
	case RoleServer:
		return "server"
	case RoleAgent:
		return "agent"
	case RoleDatastore:
		return "datastore"
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
	// DatastoreEndpoint, when non-empty, is the k3s --datastore-endpoint for an EXTERNAL
	// network datastore (e.g. postgres://user:pass@db.aegis:5432/kine). It switches the
	// cluster into multi-server HA mode: every server runs stateless against this shared
	// datastore — no embedded etcd (etcd's IP-bound peer membership cannot survive the
	// vmnet DHCP IP shift; see docs/ADR/0002). Empty = single-server embedded sqlite
	// (v0.1.x), UNLESS ManageDatastore asks Create to provision one. validateClusterConfig
	// requires either this or ManageDatastore whenever Nodes has >1 server.
	DatastoreEndpoint string
	// ManageDatastore asks Create to provision a managed Postgres datastore micro-VM (the
	// one-command HA path) and fill DatastoreEndpoint automatically. Requires a DNS domain
	// (HA needs a stable FQDN for the datastore endpoint). Ignored when DatastoreEndpoint is
	// already set (bring-your-own datastore). The managed node's image and memory are fixed
	// defaults (see node.go defaultDatastoreImage / defaultDatastoreMemoryBytes).
	ManageDatastore bool
	// Nodes are the nodes to launch (server first is enforced by Create's ordering). The
	// managed datastore is NOT listed here — Create provisions it separately.
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
	Image string `json:"image"`
	// DatastoreEndpoint is the external --datastore-endpoint the cluster runs on, recorded
	// for visibility and so HA context survives in state.json. Empty for single-server
	// sqlite clusters; pre-v0.2.0 states omit the field and unmarshal to "".
	DatastoreEndpoint string     `json:"datastoreEndpoint,omitempty"`
	ServerURL         string     `json:"serverURL"` // https://<server-fqdn>:6443 (FQDN or IP)
	Nodes             []NodeInfo `json:"nodes"`
}
