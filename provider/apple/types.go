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
	// RoleDatastore is a managed external-datastore micro-VM that backs an HA control plane. As
	// of v0.3.0 the managed datastore is an N-member etcd cluster (docs/ADR-0003), so MULTIPLE
	// nodes carry this role — one per etcd member (the single-Postgres path of ADR-0002 is
	// retired; bring-your-own -datastore-endpoint still accepts Postgres/MySQL/etcd). A
	// RoleDatastore node is NOT a k3s node: it never goes through buildRunArgs or the k3s
	// subcommand path. It is provisioned by Create when ManageDatastore is set and tracked in
	// ClusterState.Nodes so the teardown label sweep reclaims it.
	RoleDatastore
	// RoleLB is the API-server load balancer micro-VM (haproxy mode tcp) that fronts the HA
	// control plane at the shared FQDN <cluster>-api.<domain> (docs/ADR/0002, v0.3.0). Like
	// RoleDatastore it is NOT a k3s node: it never goes through buildRunArgs or the k3s
	// subcommand path. It is provisioned by Create when ProvisionAPILB is set (more than one
	// server) and tracked in ClusterState.Nodes so the teardown label sweep reclaims it. It is
	// stateless (no named volume): its only config is an haproxy.cfg delivered via host
	// bind-mount, regenerated on every create.
	RoleLB
)

// String renders the role. For server/agent it is the k3s subcommand appended to
// `container run ... <image> <role>`; "datastore" and "lb" are label values only (a
// RoleDatastore / RoleLB node never reaches the k3s subcommand path).
func (r Role) String() string {
	switch r {
	case RoleServer:
		return "server"
	case RoleAgent:
		return "agent"
	case RoleDatastore:
		return "datastore"
	case RoleLB:
		return "lb"
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
	// Labels are k3s node labels (KEY=VALUE), each emitted as a --node-label on the node's
	// k3s server/agent subcommand (v0.4.0). Applied to whichever role this node is. Empty =
	// no extra labels. The caller (cmd driver) sets the same list on every node it builds.
	Labels []string
	// ExtraArgs are operator-supplied k3s flags appended VERBATIM to this node's k3s
	// subcommand AFTER every built-in flag (v0.4.0; -k3s-server-arg / -k3s-agent-arg). k3s
	// is last-one-wins on repeated flags, so these can override built-ins as well as add new
	// ones — disable traefik, change the CNI/flannel backend, set --cluster-cidr, enable
	// ServiceLB, etc. The caller resolves the per-role list (server args onto server nodes,
	// agent args onto agent nodes). Empty = none.
	ExtraArgs []string
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
	// network datastore — bring-your-own (e.g. postgres://user:pass@db.aegis:5432/kine, or a
	// comma-separated etcd client URL list). It switches the cluster into multi-server HA mode:
	// every server runs stateless against this shared datastore — no EMBEDDED etcd (embedded
	// etcd's IP-bound peer membership cannot survive the vmnet DHCP IP shift; see docs/ADR/0002).
	// The managed HA path (ManageDatastore) instead auto-provisions an EXTERNAL etcd cluster whose
	// members are FQDN-addressed and so DO survive the shift (docs/ADR-0003). Empty = single-server
	// embedded sqlite (v0.1.x), UNLESS ManageDatastore asks Create to provision one.
	// validateClusterConfig requires either this or ManageDatastore whenever Nodes has >1 server.
	DatastoreEndpoint string
	// ManageDatastore asks Create to provision a managed etcd cluster (the one-command HA path,
	// docs/ADR-0003) and fill DatastoreEndpoint automatically. Requires a DNS domain (every etcd
	// member and every server addresses the others by stable FQDN). Ignored when DatastoreEndpoint
	// is already set (bring-your-own datastore). The members' image and memory are fixed defaults
	// (see node.go defaultEtcdImage / defaultEtcdMemoryBytes).
	ManageDatastore bool
	// DatastoreMembers is the managed etcd cluster size. MUST be odd and ≥3 (3 or 5);
	// validateEtcdMemberCount enforces that. 0 means "use defaultEtcdMembers" (3). Only meaningful
	// with ManageDatastore (a bring-your-own DatastoreEndpoint ignores it).
	DatastoreMembers int
	// DatastoreImage overrides the managed etcd member image (-datastore-image, v0.5.0). Empty =
	// defaultEtcdImage. Only meaningful for the managed etcd path; a bring-your-own endpoint
	// ignores it (the operator owns their datastore's image).
	DatastoreImage string
	// DatastoreMemoryBytes overrides each managed etcd member's memory in bytes (-datastore-memory,
	// v0.5.0). 0 = defaultEtcdMemoryBytes (512 MiB). Same managed-only scope as DatastoreImage.
	DatastoreMemoryBytes int64
	// DatastoreTLSDir is NOT a request field — Create populates it (like DatastoreEndpoint) with the
	// absolute host dir holding the k3s datastore CLIENT TLS bundle (ca.crt/client.crt/client.key)
	// it generated for the managed etcd cluster (v0.5.0). When non-empty, buildRunArgs bind-mounts it
	// read-only into every server and passes --datastore-cafile/certfile/keyfile so the servers reach
	// etcd over mutual TLS. Empty = plain datastore connection (bring-your-own endpoint, or no managed
	// etcd). See etcd_tls.go.
	DatastoreTLSDir string
	// ProvisionAPILB asks Create to provision an API-server load balancer micro-VM (haproxy
	// mode tcp) at the shared FQDN <cluster>-api.<domain>, and to point the kubeconfig + agents
	// at that one endpoint instead of the bootstrap server (docs/ADR/0002, v0.3.0). It is
	// meaningful only with more than one server (a single server IS the endpoint) and requires a
	// DNS domain (the LB is FQDN-addressed and the cert SAN <cluster>-api.<domain> exists only in
	// FQDN mode). The cmd driver infers it as serverCount > 1; setupAPILB gates it on the DNS
	// domain and skips gracefully (no LB, keep pointing at the bootstrap server) when absent. The
	// LB node is NOT listed in Nodes — Create provisions it separately.
	ProvisionAPILB bool
	// EnvVars are operator-supplied environment variables (each "KEY=VALUE") injected into EVERY
	// k3s node's `container run` as a --env flag (v0.5.0; -env, repeatable). They sit alongside the
	// built-in K3S_TOKEN / K3S_URL env and are create-time only (AddAgents/AddServer do not thread
	// them). Empty = none. Mirrors the -k3s-server-arg stringList pattern.
	EnvVars []string
	// Manifests are host-side Kubernetes manifest file paths bind-mounted into the BOOTSTRAP
	// server's k3s auto-deploy dir (/var/lib/rancher/k3s/server/manifests) so k3s applies them
	// at startup (v0.4.0). Each file is mounted INDIVIDUALLY (not the whole directory) so k3s's
	// own generated manifests living in the named-volume datastore are not shadowed. Create
	// resolves these to absolute paths (the `container` runtime requires absolute bind sources)
	// and rejects basename collisions before launch. Empty = none.
	Manifests []string
	// Nodes are the nodes to launch (server first is enforced by Create's ordering). The
	// managed datastore and the API LB are NOT listed here — Create provisions them separately.
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
