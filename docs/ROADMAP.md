# k3ac roadmap

k3ac provisions HA k3s clusters on Apple's `container` runtime. This file records where k3ac
sits against the obvious reference point — k3d, the de-facto "k3s in containers" tool — and what
ships in v0.4.0. It is a planning document, not a spec: the source and the ADRs are the contract.

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
| Multi-server control-plane HA      | embedded etcd  | external datastore + FQDN    | ADR-0002: etcd is IP-bound and dies on the vmnet IP shift; external datastore at an FQDN survives it. |
| API-server load balancer           | built-in LB    | deferred (v0.3.0)            | Only meaningful with multiple servers; pairs with HA, does not precede it (ADR-0002). |
| Datastore HA (replicated)          | n/a            | pending spike → ADR-0003     | The managed Postgres datastore is currently a single point of failure (see below). |
| k3s flag passthrough               | `--k3s-arg`    | `--k3s-server-arg` / `--k3s-agent-arg` (v0.4.0) | The escape hatch that turns a closed box open. |
| Node labels at create              | `--k3s-node-label` | `--node-label` (v0.4.0)  | Standard k3s scheduling primitive. |
| Auto-deploy manifests              | `--volume` into the auto-deploy dir | `-manifest` bind-mount (v0.4.0) | Same virtio-fs bind-mount path proven for kubeconfig (ADR-0001). |
| Per-node CPU / memory sizing       | yes            | `-server-cpus` / `-agent-cpus` (CPU, v0.4.0); memory since v0.1.0 | Routine VM sizing. |
| Cluster / node list                | `k3d cluster list` | `-list` (v0.4.0)         | Pure read of each cluster's `state.json`; no daemon call. |
| Start / stop lifecycle             | `k3d cluster start/stop` | `-start` / `-stop` (v0.4.0) | Ordered across nodes; re-arms `ip_forward` on start (see below). |
| Image import (`k3d image import`)  | yes            | **out of scope**             | vmnet node IPs are host-routable, so an image lands via the node's own pull, not a Docker side-load. |
| Built-in local registry            | yes            | **out of scope**             | Same reason: this is a Docker inner-loop convenience, not an HA-provisioning need. Point nodes at any reachable registry. |
| Host port mapping (`-p`)           | yes            | **out of scope**             | Each node already has a routable vmnet IP; reach a NodePort or LoadBalancer IP directly. No host-port plumbing needed. |

The three "out of scope" rows are deliberate. They are Docker-DX features whose entire reason to
exist is that Docker containers are not directly routable. On vmnet they are, so the features have
no problem to solve here. Adding them would be surface area that fights the substrate.

## v0.4.0 scope — operability and tunability

v0.4.0 does not touch the HA axis. It makes the cluster k3ac already provisions controllable and
inspectable. Six features, all stdlib-only, all with unit tests on the pure logic:

1. **k3s-arg passthrough** (`--k3s-server-arg`, `--k3s-agent-arg`, repeatable). Operator args are
   appended verbatim AFTER every built-in k3s flag, so they both add new flags and override
   built-ins (k3s is last-one-wins). This is the highest-value item: it turns the fixed recipe
   into an open one — disable traefik, change the flannel backend / CNI, set `--cluster-cidr`,
   enable ServiceLB, and so on, without a code change.
2. **`-list`**. Scans `-state-dir` for `*/state.json` and prints name, server count, agent count,
   server URL, and image. Pure file I/O — it works with the `container` daemon down.
3. **Node labels at create** (`--node-label KEY=VALUE`, repeatable). Emitted as `--node-label` on
   each node's k3s subcommand. Pairs with item 1 for scheduling control.
4. **Start / stop lifecycle** (`-start`, `-stop`). Orders the operation across a cluster's nodes:
   stop is agents → servers → datastore; start is the exact reverse. `container start` takes one
   id per call, so nodes start one at a time. After each k3s node boots onto its new DHCP IP,
   `net.ipv4.ip_forward=1` is re-armed (there is no systemd to do it, and it is lost across a cold
   stop/start). The FQDN endpoints re-register through `container` DNS, which is what lets the
   control plane reconverge after the whole-cluster IP shift (ADR-0002, VERIFICATION G9).
5. **Auto-deploy manifests** (`-manifest <path>`, repeatable). Each host manifest file is
   bind-mounted INDIVIDUALLY into the bootstrap server's `/var/lib/rancher/k3s/server/manifests/`
   via the same virtio-fs bind-mount proven for kubeconfig delivery (ADR-0001) — no guest agent,
   no `container cp`. Individual-file mounts (not a directory mount) leave k3s's own generated
   manifests in the named volume intact. k3s auto-deploys whatever lands there.
6. **Per-node CPU tuning** (`-server-cpus`, `-agent-cpus`). Exposes the `NanoCPUs` already threaded
   through `buildRunArgs`. Default stays 2 vCPU.

All six are also expressible in the `-config` JSON so a cluster is described once rather than as a
long flag list.

### Internal change carried in this release

`applyConfig` grew from a single-server flag merge to ~13 positional arguments and was about to
take six more. v0.4.0 converts it to a `flagRefs` struct (and the create-time node set to a
`nodeSpec` struct). This removes the argument-order footgun where a misplaced `&var` would silently
misroute a flag, and keeps the call sites self-documenting (`field: value`).

## Pending: datastore HA — needs a spike (ADR-0003, not yet written)

The HA cut (v0.2.0, ADR-0002) makes the control plane HA: N stateless k3s servers against one
external datastore at a stable FQDN, verified to survive the cold-restart IP shift. The honest
limit is that **the datastore itself is a single Postgres VM — a single point of failure**. The
servers are HA; the thing they depend on is not.

Closing that gap is a larger piece of work and needs a spike before it gets an ADR:

- **Replicated Postgres** (primary + standby, or a Patroni-style managed topology) reachable at a
  stable FQDN, with failover that does not depend on a fixed IP — the same vmnet constraint that
  shaped ADR-0002 applies to the replica set.
- **External etcd as the datastore backend** — k3s supports it; it trades the Postgres SPOF for an
  etcd cluster, which reintroduces the IP-bound-membership question ADR-0002 found fatal for
  *embedded* etcd. Whether a *standalone* etcd cluster addressed by FQDN avoids that has to be
  measured on hardware, not assumed.
- **kine + replicated SQLite** — lighter, but not a documented k3s HA topology; spike-only.

The spike must answer one question on real hardware: which datastore-HA topology reconverges after
the whole-cluster DHCP IP shift, the same gate G9 applied to the single-datastore case. The outcome
becomes ADR-0003. Until then, state the limit to operators plainly: **HA servers, non-HA datastore**.

## References

- ADR-0001 — kubeconfig delivery via host bind-mount (the FQDN/DNS endpoint mechanism reused for
  manifests and the datastore address).
- ADR-0002 — HA via an external datastore at a stable FQDN, not embedded etcd.
- docs/VERIFICATION.md — G-gate hardware log (G1 boot, G5 FQDN-survives-restart, G9 HA survival).
