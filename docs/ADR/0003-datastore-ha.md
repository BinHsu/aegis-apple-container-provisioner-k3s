# ADR-0003: Datastore HA via an auto-provisioned external etcd cluster at stable FQDNs

**Status:** Accepted, 2026-06-28 (spike verdict YES; hardware-verified)

Supersedes ADR-0002 on one point: the *managed* HA datastore. ADR-0002's control-plane
decision (N stateless k3s servers against an external datastore addressed by FQDN, never
embedded etcd) stands. This ADR replaces ADR-0002's single managed **Postgres** VM with an
auto-provisioned **3-node external etcd cluster** as the default managed datastore, closing
the datastore single-point-of-failure ADR-0002 left open.

---

## Context

ADR-0002 shipped control-plane HA: two or more k3s servers run stateless against one shared
external datastore reached by FQDN, so the whole control plane survives the vmnet DHCP
cold-restart IP shift that kills embedded etcd. But ADR-0002's managed datastore was a
**single Postgres micro-VM** — explicitly called out there as "the new single point of
failure ... HA servers, non-HA datastore." The control plane was HA; the thing it depends on
was not.

The defining substrate constraint is unchanged: Apple `container` assigns each node a vmnet
IP by DHCP with no static option, and a cold `container stop`/`start` shifts every node to a
new IP. v0.1.0's FQDN mechanism (`container` DNS registers each node as `<name>.<domain>` and
re-registers the A-record to the new IP on restart — ADR-0001) keeps any name-addressed
endpoint valid across the shift. The open question for datastore HA: can a **multi-node** etcd
cluster — run as the *external* datastore, addressed entirely by FQDN — form quorum and
survive the same IP shift, where ADR-0002 proved *embedded, IP-bound* etcd could not?

---

## Decision

**Auto-provision a 3-node external etcd cluster, addressed entirely by FQDN, as the default
managed HA datastore.** Member count is configurable but MUST be odd and ≥ 3 (3 or 5);
default 3.

- Each etcd member runs as its own micro-VM with the container `--name` set to its FQDN
  `<cluster>-etcd-<i>.<domain>`, so `container` DNS registers it. This is mandatory — a bare
  name leaves every peer at NXDOMAIN and quorum never forms (see Gotchas).
- Peer, advertise, and `--initial-cluster` URLs are ALL FQDNs:
  `--initial-advertise-peer-urls http://<cluster>-etcd-<i>.<domain>:2380`,
  `--initial-cluster <m1>=http://<m1>.<domain>:2380,...`, `--initial-cluster-state new`. Peer
  membership is therefore **name-bound**, not IP-bound.
- Member data lives on a named volume mounted at `/data` with `--data-dir /data/etcd` (a
  subdir — the ext4 `lost+found` at the mount root blocks etcd).
- Image pinned to `quay.io/coreos/etcd:v3.5.16` (spike-proven).
- The k3s servers point at the cluster with a comma-separated client-URL list:
  `--datastore-endpoint=http://<m1>.<domain>:2379,http://<m2>.<domain>:2379,...`. Servers are
  otherwise UNCHANGED from the ADR-0002 / API-LB recipe — still no `--cluster-init`.

The single managed-Postgres auto-provision path is **retired**. Bring-your-own
`-datastore-endpoint` (Postgres, MySQL/MariaDB, or external etcd) is unchanged: an operator
who wants a different backend still passes it directly.

Embedded etcd (`--cluster-init`) remains rejected, for the reason ADR-0002 proved.

---

## Rationale and verification (spike, hardware)

### Why an external etcd cluster works where embedded etcd did not

The failure ADR-0002 documented is specific: *embedded* etcd bakes its peer membership to the
node IPs (`advertise-client-urls https://192.168.64.21:2379`), and the DHCP cold-restart shift
moves every IP, so quorum cannot reform — the apiserver returned `ServiceUnavailable` for the
full window and never recovered.

An *external* etcd cluster addressed by FQDN has no IP-bound membership. Every member's peer
and client URLs are names (`<cluster>-etcd-<i>.<domain>`); `container` DNS re-registers each
name to its new IP on restart, exactly as it does for the k3s servers and the API LB. The IP
shift is invisible to etcd because every participant — peers and k3s clients alike — reaches
the others by name. This is the same mechanism that already stabilizes the client endpoint
(ADR-0001) and the Postgres datastore endpoint (ADR-0002), now applied to a replicated quorum.

### Why this closes the SPOF

A 3-member etcd cluster tolerates the loss of one member and keeps serving. The control plane
(HA since ADR-0002) and the datastore it depends on are now both replicated, so no single VM
loss takes the cluster down. 3 members tolerate 1 failure; 5 tolerate 2.

### Verification

Spike verdict YES (hardware): a 3-member FQDN-addressed etcd cluster forms quorum under Apple
`container`, k3s servers come up stateless against the comma-separated client-URL endpoint, and
the cluster survives a whole-stack cold-restart DHCP IP shift — every member and server
re-registers its FQDN to the new IP and quorum reforms. The make-or-break detail was naming
each etcd container by its FQDN so `container` DNS registers it; with bare names the peers
NXDOMAIN each other and quorum never forms.

---

## Consequences

### Positive

- **Datastore HA on this substrate.** The managed datastore is now a quorum, not one VM; it
  survives losing any single member and the whole-cluster cold-restart IP shift.
- **One HA story.** Managed HA is now uniformly etcd (control plane + datastore both
  FQDN-addressed quorums). No separate Postgres recipe to maintain.
- **Reuses every v0.1.0/v0.2.0 mechanism.** The FQDN + `container` DNS that stabilizes the
  client endpoint and the Postgres datastore endpoint stabilizes etcd peer membership too — no
  new addressing machinery.

### Limits and watch items

- **TLS is deferred (not dropped).** etcd runs over plain HTTP on the private vmnet for now,
  matching ADR-0002's `sslmode=disable` precedent. k3s supports
  `--datastore-cafile/--datastore-certfile/--datastore-keyfile` and etcd supports peer/client
  TLS; harden with FQDN-SAN certs before any non-spike use.
  `// TODO(v0.3.x): etcd peer/client TLS with FQDN SANs + k3s --datastore-cafile/certfile/keyfile`.
- **Resource cost.** Three etcd micro-VMs (512 MiB each here) plus the k3s servers. Datastore
  HA trades memory for fault tolerance vs ADR-0002's single Postgres VM.
- **etcd data-dir must be a subdir of the mount** (`--data-dir /data/etcd`): an ext4 named
  volume ships a `lost+found` at the mount root that etcd refuses as a data dir. Mirrors the
  Postgres PGDATA-subdir guard (ADR-0002).
- **Container `--name` MUST be the FQDN.** `--dns-domain` alone does not register the
  container; the `--name` carries the `.<domain>` suffix that `container` DNS registers. A
  name without the suffix → every peer NXDOMAINs → quorum never forms. The make-or-break
  detail of the spike.
- **In-VM resolver negative-cache lag (~30 s).** Right after startup a member may negative-
  cache a peer's NXDOMAIN for ~30 s before the peer's A-record is registered. etcd and k3s both
  retry, so the cluster converges; provisioning therefore does NOT assert immediate cross-node
  quorum health — it gates only on each member's own client port binding (a TCP dial), and lets
  the k3s servers retry their datastore connection until quorum settles.
- **`container start` accepts one id.** `container stop` accepts multiple ids; `container
  start` accepts only one — start each member individually in any restart path. (Provisioning
  here is create-only.)

---

## Alternatives considered

### Keep the single managed Postgres (ADR-0002)

**Rejected — it is the SPOF this ADR exists to close.** ADR-0002 itself flagged the single
Postgres VM as the open datastore SPOF and named "replicated Postgres, or external etcd as the
backend" as the follow-up. External etcd is the lighter, officially-supported quorum and reuses
the FQDN mechanism directly, so it is the chosen close. Bring-your-own Postgres (replicated by
the operator) remains available via `-datastore-endpoint`.

### Replicated Postgres (primary + replicas)

**Not chosen for the managed path.** Streaming replication plus failover/promotion is
materially more orchestration than an etcd quorum, and k3s already treats etcd as a
first-class datastore. Operators who want it can still bring it via `-datastore-endpoint`.

### Embedded etcd (`--cluster-init`)

**Rejected — empirically, in ADR-0002.** Embedded etcd's peer membership is IP-bound and
cannot reform quorum after the DHCP cold-restart shift. The external-etcd cluster here avoids
this precisely because membership is name-bound.

---

## Implementation notes for k3ac

- `RoleDatastore` generalizes from one node to N: each etcd member is a `RoleDatastore`
  `NodeInfo` in `ClusterState.Nodes`, provisioned separately from `ClusterConfig.Nodes` (like
  the retired single datastore and the API LB), so the destroy label sweep and recorded-node
  pass both reclaim every member + its per-member volume.
- `validateEtcdMemberCount` enforces odd-and-≥3 (BVA: 1/2 reject, 3 accept, 4 reject, 5
  accept). `validateClusterConfig` applies it when a managed etcd cluster is requested with an
  explicit non-default member count; 0 defaults to 3 in `provisionEtcdCluster`.
- `etcdInitialCluster` / `etcdDatastoreEndpoint` are the single source of truth for the two
  FQDN string constructions, shared by the member run-args and the k3s servers.
- Per-member volume name is `etcdVolumeName(<member-name>)` = `<cluster>-etcd-<i>-data`;
  create and destroy both derive it, so teardown can never target a different volume than
  create made.
- The members are cluster-scoped (`<cluster>-etcd-<i>`), matching every other node
  (`<cluster>-server-N`, `<cluster>-api`), so two HA clusters sharing a DNS domain never
  collide on the same etcd FQDN. An `--initial-cluster-token <cluster>-etcd` further isolates
  each cluster's quorum.

---

## References

- ADR-0001 — kubeconfig delivery via host bind-mount (the FQDN/DNS endpoint mechanism reused
  here for etcd peer membership).
- ADR-0002 — HA via an external datastore at a stable FQDN, not embedded etcd (the control-plane
  decision this ADR builds on; supersedes its single-Postgres managed datastore).
- VERIFICATION G5 — FQDN endpoint survives cold restart on a new DHCP IP.
- VERIFICATION G9 — HA external-datastore cold-restart survival (ADR-0002's verification).
- k3s docs: High Availability External DB (`docs.k3s.io/datastore/ha`), Cluster Datastore
  (`docs.k3s.io/datastore`), server CLI (`docs.k3s.io/cli/server`).
- etcd docs: Clustering Guide — static bootstrap (`etcd.io/docs/v3.5/op-guide/clustering`).
