# k3ac roadmap

k3ac provisions HA k3s clusters on Apple's `container` runtime. This file records where k3ac
sits against the obvious reference point — k3d, the de-facto "k3s in containers" tool — and
what has shipped through v0.6.0. It is a living history document, not a spec: the source and
the ADRs are the contract.

## Frame: k3ac is not k3d

k3d runs k3s in Docker on a developer laptop. Its design center is the Docker inner loop: import
a local image, push to a built-in registry, map a host port to a NodePort, iterate. k3ac runs k3s
in Apple `container` micro-VMs, one VM per node, and its design center is HA provisioning on a
substrate where every node gets a real vmnet IP that is routable from the host. Those two centers
pull the feature set in different directions, so "does k3d have it?" is the wrong question. The
right question is "does it serve HA provisioning on vmnet?".

The defining substrate constraint (see ADR-0001, ADR-0002): a cold `container stop`/`start` shifts
every node's DHCP IP. All addressing is therefore by stable FQDN through `container` DNS, never by
raw IP. That single fact drives the HA design and rules out several k3d conveniences as no-ops.

## Gap analysis vs k3d

| Capability                         | k3d            | k3ac                         | Decision |
|------------------------------------|----------------|------------------------------|----------|
| Multi-server control-plane HA      | embedded etcd  | external datastore + FQDN    | ADR-0002: etcd is IP-bound and dies on the vmnet IP shift; external datastore at an FQDN survives it. SHIPPED v0.2.0. |
| API-server load balancer           | built-in LB    | haproxy L4 `<cluster>-api.<domain>` | SHIPPED v0.3.0. Only meaningful with multiple servers; pairs with HA (ADR-0002). |
| Datastore HA (replicated)          | n/a            | 3-node etcd quorum (mutual TLS) | SHIPPED v0.3.0 (ADR-0003). Closes the single-Postgres SPOF ADR-0002 left open. |
| k3s flag passthrough               | `--k3s-arg`    | `-k3s-server-arg` / `-k3s-agent-arg` (repeatable) | SHIPPED v0.4.0. The escape hatch that turns a closed box open. |
| Node labels at create              | `--k3s-node-label` | `-node-label` (repeatable) | SHIPPED v0.4.0. Standard k3s scheduling primitive. |
| Auto-deploy manifests              | `--volume` into the auto-deploy dir | `-manifest` bind-mount (repeatable) | SHIPPED v0.4.0. Same virtio-fs bind-mount path proven for kubeconfig (ADR-0001). |
| Per-node CPU / memory sizing       | yes            | `-server-cpus` / `-agent-cpus`; memory since v0.1.0 | SHIPPED v0.4.0. Routine VM sizing. |
| Cluster / node list                | `k3d cluster list` | `-list` | SHIPPED v0.4.0. Pure read of each cluster's `state.json`; no daemon call. |
| Start / stop lifecycle             | `k3d cluster start/stop` | `-start` / `-stop` | SHIPPED v0.4.0. Ordered across nodes; re-arms `ip_forward` on start. |
| Add a control-plane server         | n/a            | `-add-server` | SHIPPED v0.5.0. Adds one server to a live HA cluster and updates the API LB. |
| Env-var injection                  | n/a            | `-env KEY=VALUE` (repeatable) | SHIPPED v0.5.0. Injected into every k3s node at create or via `-config`. |
| Merge kubeconfig                   | `k3d kubeconfig merge` | `-merge-kubeconfig` | SHIPPED v0.5.0. Merges into `~/.kube/config` via kubectl. |
| Etcd snapshot / restore            | n/a            | `-snapshot` / `-restore` | SHIPPED v0.6.0. Safe read; destructive restore needs `-force`. |
| Rolling image upgrade / rollback   | n/a            | `-upgrade` / `-rollback` | SHIPPED v0.6.0. One node at a time, servers first; rollback to pinned previous image. |
| Cert / token rotation              | n/a            | `-rotate-certs` / `-rotate-token` | SHIPPED v0.6.0. Rotates managed etcd TLS + k3s certs / K3S_TOKEN; needs `-force`. |
| Image import (`k3d image import`)  | yes            | **out of scope**             | vmnet node IPs are host-routable; an image lands via the node's own pull, not a Docker side-load. |
| Built-in local registry            | yes            | **out of scope**             | Same reason: this is a Docker inner-loop convenience, not an HA-provisioning need. Point nodes at any reachable registry. |
| Host port mapping (`-p`)           | yes            | **out of scope**             | Each node already has a routable vmnet IP; reach a NodePort or LoadBalancer IP directly. No host-port plumbing needed. |

The three "out of scope" rows are deliberate. They are Docker-DX features whose entire reason to
exist is that Docker containers are not directly routable. On vmnet they are, so the features have
no problem to solve here. Adding them would be surface area that fights the substrate.

---

## v0.1.0 — single-server baseline (2026-06-27)

First working cut. Proved the full single-server lifecycle on hardware (G1–G7):

- k3s embedded containerd boots under `vminitd` + `--cap-add ALL`.
- Named-volume sqlite datastore persists across cold stop/start.
- FQDN endpoint (`-dns-domain`) via `container` DNS; cert SAN covers the FQDN.
- Host-side TLS readiness + bind-mount kubeconfig delivery (zero `container cp`).
- Real workload + Traefik ingress host-reachable from the Mac.
- Full lifecycle teardown: `state.json` path and label-sweep fallback, no daemon hang.

## v0.2.0 — node membership + config (2026-06-27)

- `-add-agents` / `-remove-node` membership operations (G8).
- HA external-datastore spike on hardware (G9): two k3s servers against a shared Postgres
  VM at a stable FQDN survive the whole-cluster cold-restart DHCP IP shift. Embedded etcd
  does not (IP-bound peer membership — killed on the shift). Promoted to ADR-0002.
- `-config` JSON declarative cluster spec (stdlib-only, no YAML dependency).

## v0.3.0 — HA control plane: API LB + 3-node etcd datastore (ADR-0003)

Closed both open items from ADR-0002:

**API-server load balancer.** haproxy L4 container (`<cluster>-api.<domain>`, `mode tcp`)
fronts the server pool. The kubeconfig endpoint is the LB FQDN; `-add-server` and
`-remove-node` rewrite the haproxy config live.

**Datastore HA.** Replaced the single managed-Postgres VM with an auto-provisioned 3-node
external etcd cluster addressed entirely by FQDN (`<cluster>-etcd-{1,2,3}.<domain>`). Peer
membership is name-bound, not IP-bound, so the quorum survives the DHCP cold-restart IP
shift that kills embedded etcd. Member count is configurable via `-datastore-members` (odd,
≥ 3; default 3). Spike verdict YES on hardware; promoted to ADR-0003. Verified G10 end-to-end.

Done in v0.3.0 (ADR-0003): the "Pending: datastore HA — needs a spike" section from the
earlier planning draft is closed.

## v0.4.0 — operability and tunability

Made the cluster k3ac already provisions controllable and inspectable. All features are also
expressible in `-config` JSON:

1. **k3s-arg passthrough** (`-k3s-server-arg`, `-k3s-agent-arg`, repeatable). Operator args
   are appended verbatim AFTER every built-in k3s flag, so they can override built-ins (k3s
   is last-one-wins). Turns the fixed recipe into an open one — disable traefik, change the
   CNI, set `--cluster-cidr`, enable ServiceLB, etc., without a code change.
2. **`-list`**. Scans `-state-dir` for `*/state.json` and prints name, server count, agent
   count, server URL, and image. Pure file I/O — works with the `container` daemon down.
3. **Node labels at create** (`-node-label KEY=VALUE`, repeatable).
4. **Start / stop lifecycle** (`-start`, `-stop`). Ordered across nodes; re-arms
   `ip_forward` on each k3s node after start.
5. **Auto-deploy manifests** (`-manifest <path>`, repeatable). Delivered by bind-mount into
   the bootstrap server's auto-deploy dir — no guest agent, no `container cp`.
6. **Per-node CPU tuning** (`-server-cpus`, `-agent-cpus`). Default 2 vCPU.

Internal: `applyConfig` and the create-path node set converted to `flagRefs` / `nodeSpec`
structs, removing the positional-argument footgun.

## v0.5.0 — mutual TLS, add-server, datastore tunables, merge-kubeconfig, env

Five features that close the remaining operability gaps:

1. **Managed etcd mutual TLS.** k3ac generates (host-side, stdlib `crypto/x509`) one CA, one
   server cert per etcd member (SAN = member FQDN + localhost + 127.0.0.1), and one client
   cert for the k3s servers. Bundle delivered by host bind-mount (ADR-0001 mechanism) — never
   `container cp`. etcd runs with `--peer-client-cert-auth` and `--client-cert-auth`; k3s
   servers connect with `--datastore-cafile/--datastore-certfile/--datastore-keyfile`. FQDN
   SANs keep TLS valid across the DHCP IP shift. See `provider/apple/etcd_tls.go`.
2. **`-add-server`**. Adds one control-plane server to a live HA cluster and rewrites the
   haproxy config to include it in the LB pool. Supports `-node-label` and `-k3s-server-arg`.
3. **Datastore tunables** (`-datastore-image`, `-datastore-memory`). Override the managed
   etcd member image and memory allocation.
4. **`-merge-kubeconfig`**. Merges the named cluster's kubeconfig into `~/.kube/config` via
   `kubectl` and sets it as the current context.
5. **`-env KEY=VALUE`** (repeatable). Injects an environment variable into every k3s node.
   Also backed by `-config` (`envVars` field).

## v0.6.0 — day-2 operations

Six day-2 operations under a common `-force` guard. Destructive verbs print the plan and
refuse without `-force`; `-snapshot` is safe and needs no confirmation.

1. **`-snapshot`**. Runs an ephemeral etcdctl container that streams `snapshot save` to a host
   path (`<cluster>/snapshots/<cluster>-<timestamp>.db`) via writable bind-mount. Safe — does
   not mutate the cluster.
2. **`-restore`** + **`-snapshot-file`**. Stops the cluster, rebuilds each etcd member's data
   volume from the snapshot (every member restores from the same file with the same
   `--initial-cluster` so the restored quorum is the same FQDN mesh), verifies the marker key
   (`/registry/namespaces/kube-system`) in the restored datastore, then restarts servers and
   agents. Preserves k3s server/agent state volumes and datastore TLS.
3. **`-upgrade`** (+ `-image`). Rolling replace: visits k3s nodes one at a time (servers before
   agents), cordons + drains each, deletes the stale Node object (so the recreated kubelet
   registers on its new DHCP IP), recreates the container on the target image preserving the
   named state volume, waits for Ready, and uncordons. Pins the outgoing image as the rollback
   target. Handles a 3-layer IP re-registration (new container IP → Node InternalIP → flannel
   public-ip annotation).
4. **`-rollback`**. Same rolling orchestration, moving to the image pinned at the last
   `-upgrade`. Itself reversible (pins the image it rolled away from as the new previous).
5. **`-rotate-certs`**. Regenerates the managed etcd TLS bundle (new CA + member/client certs),
   overwrites the same on-disk dirs the members bind-mount (so a restart loads the new
   material), then rotates each k3s server's own certificates offline (`k3s certificate rotate`
   on the stopped server's data volume).
6. **`-rotate-token`**. Generates a new K3S_TOKEN, runs `k3s token rotate` on a live server to
   re-encrypt bootstrap data in the datastore, then recreates every server and agent with the
   new token baked into their `container run` env. Preserves all named state volumes.

---

## References

- ADR-0001 — kubeconfig delivery via host bind-mount (the FQDN/DNS endpoint mechanism reused
  for manifests, haproxy config, and etcd TLS).
- ADR-0002 — HA via an external datastore at a stable FQDN, not embedded etcd.
- ADR-0003 — datastore HA via a 3-node external etcd cluster at stable FQDNs (supersedes
  ADR-0002's single managed-Postgres path).
- docs/VERIFICATION.md — G-gate hardware log (G1–G10+).
