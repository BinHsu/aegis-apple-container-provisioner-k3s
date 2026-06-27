# ADR-0002: HA via an external datastore at a stable FQDN, not embedded etcd

**Status:** Accepted, 2026-06-27 (both options verified on hardware)

---

## Context

v0.1.0 ships a single-server cluster: one k3s server on the embedded SQLite datastore.
The control plane is a single point of failure. The HA goal is two or more k3s servers
that keep serving the Kubernetes API when one server is lost or restarted.

k3s offers two HA datastore modes:

- **(a) Embedded etcd** — `--cluster-init` on the first server, the rest join its etcd
  cluster. The datastore lives inside the server nodes.
- **(b) External datastore** — every server runs stateless against a shared network
  database (PostgreSQL, MySQL/MariaDB, or external etcd) via `--datastore-endpoint`.

The deciding constraint is this substrate's addressing. Apple `container` assigns each
node a vmnet IP by DHCP with no static-IP option. A cold `container stop`/`start` shifts
every node to a new IP (verified repeatedly: e.g. `.28/.29/.30` → `.31/.32/.33`). v0.1.0
already solves the *client* endpoint problem with an FQDN: `container` DNS registers each
node as `<name>.aegis` and re-registers the A-record to the new IP on restart, so the
kubeconfig endpoint stays valid (ADR-0001, VERIFICATION G5). The HA question is whether
the *datastore* survives the same IP shift.

---

## Decision

**Use option (b): N stateless k3s servers against an external network datastore addressed
by FQDN.** Default backend PostgreSQL; MySQL/MariaDB and external etcd are also accepted
by `--datastore-endpoint`. The datastore runs as its own micro-VM at a stable FQDN
(e.g. `k3h-db.aegis`); each server is launched with
`--datastore-endpoint=postgres://…@k3h-db.aegis:5432/<db>` and no `--cluster-init`. An
API-server load balancer in front of the servers is optional and only meaningful once
there are multiple servers (k3s lets agents join any server IP directly).

Embedded etcd (a) is rejected.

---

## Rationale and verification (hardware, 2026-06-27)

### Why embedded etcd (a) is ruled out — by experiment, not assumption

A real 3-server embedded-etcd cluster was brought up (manual `container run`, all three
`control-plane,etcd,master` Ready). etcd's `advertise-client-urls` was IP-bound
(`https://192.168.64.21:2379`). A cold restart of all three shifted every IP
(`.21/.22/.23` → `.24/.25/.26`). The FQDN re-registered fine, so the *apiserver* was
reachable — but it returned `ServiceUnavailable` for the full 180 s window and never
recovered. etcd peer membership is baked to the **old** IPs and cannot reform quorum on
the new ones. The FQDN/DNS mechanism fixes the client endpoint; it does **not** touch
etcd's internal IP-bound peer membership. This matches the IP-stability findings in
ADR-0001 and the Talos sibling (G3/G5).

### Why plain SQLite cannot be the shared datastore

k3s's datastore is kine (an etcd-API shim over SQL). Its SQLite backend is a local file
with no network server — multiple servers cannot point at one SQLite file. SQLite is
single-server intrinsically.

### Why the external datastore (b) survives — verified

Brought up a PostgreSQL micro-VM `k3h-db.aegis` plus two k3s servers
`k3h-srv-1.aegis` / `k3h-srv-2.aegis`, each
`--datastore-endpoint=postgres://kine:…@k3h-db.aegis:5432/kine`, `--flannel-backend=host-gw`,
no `--cluster-init`. Both registered `Ready`/`control-plane,master` against the shared
datastore. Seeded a marker ConfigMap.

Cold-restarted all three. Every IP shifted: DB `.28→.31`, srv-1 `.29→.32`, srv-2
`.30→.33` — the exact condition that killed embedded etcd. Result:

- **Control plane recovered** — apiserver `/readyz` OK ~12 s after start; both nodes
  `Ready`/`control-plane`. The apiserver serves on **both** server FQDN endpoints (true
  HA — either server answers).
- **Datastore survived** — servers reconnected to `k3h-db.aegis` by name (FQDN
  re-registered to `.31`); the marker ConfigMap was intact.
- **Workload plane reconverged** — each node's `InternalIP` and flannel
  `public-ip` annotation re-registered to the new addresses, so host-gw cross-node
  routing recovers too (kubelet posts the new IP within seconds of apiserver readiness;
  a read taken in that window briefly shows the old IP).

The root-cause contrast: an external datastore addressed by FQDN has **no IP-bound peer
membership**. The DHCP IP shift that breaks etcd quorum is invisible to it because every
participant reaches the datastore by name.

---

## Consequences

### Positive

- **Control-plane HA on this substrate.** Two or more servers survive losing or
  restarting any one, including the whole-cluster cold-restart IP shift.
- **Reuses v0.1.0 mechanisms.** The FQDN + `container` DNS that stabilizes the client
  endpoint also stabilizes the datastore endpoint — no new addressing machinery.
- **Backend choice.** PostgreSQL, MySQL/MariaDB, or external etcd, per the k3s support
  matrix (verified against k3s v1.36 docs; spike ran v1.32.5+k3s1).

### Limits and watch items

- **The datastore is the new single point of failure.** The control plane is HA; a single
  Postgres VM is not. Datastore HA (replicated Postgres, or external etcd as the backend)
  is a separate, larger piece of work — out of scope for the first HA cut. State this
  plainly to operators: HA servers, non-HA datastore, until the datastore is itself
  replicated.
- **Resource cost.** Each server is a full control plane (~620 MB steady; 2 GB VM in the
  spike) plus the datastore VM. Two servers + DB ≈ 5 GB.
- **PGDATA on a named volume.** An Apple `container` named volume is block-backed ext4 and
  ships a `lost+found`, so `initdb` refuses the mount root as PGDATA. Point
  `PGDATA=/var/lib/postgresql/data/pgdata` at a subdirectory. (Cost the spike one
  iteration.)
- **`container start` takes one ID.** `container stop` accepts multiple container IDs;
  `container start` accepts only one — start each node individually in any restart path.
- **TLS to the datastore.** The spike used `sslmode=disable` for speed. k3s supports
  `--datastore-cafile/--datastore-certfile/--datastore-keyfile` for a TLS-secured
  datastore connection; harden this before any non-spike use.

---

## Alternatives considered

### Embedded etcd (`--cluster-init`)

**Rejected — empirically.** etcd peer membership is IP-bound and cannot reform quorum
after the DHCP cold-restart IP shift; the apiserver returned `ServiceUnavailable` for the
full window and never recovered. See the verification above.

### Standalone kine + SQLite exposed as a network etcd endpoint

Run kine as its own process on `:2379` over a SQLite file, with servers pointing at it as
an external-etcd endpoint. **Not chosen.** It is lighter than Postgres, but (1) it is not
a documented k3s HA topology — kine speaks etcd-v3 on `:2379` and k3s accepts that wire
format, so it is *technically* feasible but untested in the k3s support matrix; and (2)
the kine+SQLite tier is itself a single point of failure with no replication path. For the
first HA cut, an officially-supported backend (Postgres) is the lower-risk choice and
gives a clean signal. Revisit kine+SQLite only if datastore footprint becomes a real
constraint.

### API-server load balancer first

A k3d-style API LB in front of the servers. **Deferred, not rejected.** It only becomes
meaningful with multiple servers (agents can join any server IP directly), so it pairs
with HA rather than preceding it. Add it after multi-server is in place.

---

## Implementation notes for k3ac

- `validateClusterConfig` currently hard-rejects `servers ≥ 2` (the single-server SQLite
  guard, create.go). HA must relax this: `servers ≥ 2` is valid **when an external
  datastore endpoint is configured**, and still rejected otherwise.
- The create flow gains an external-datastore path: provision (or accept an existing) DB
  endpoint, then launch N servers with `--datastore-endpoint` and a shared API `--tls-san`
  (e.g. `k3h-api.aegis`) so one kubeconfig endpoint covers any server.
- Drop `--cluster-init` from the HA path; it is the embedded-etcd switch this ADR rejects.

---

## References

- ADR-0001 — kubeconfig delivery via host bind-mount (the FQDN/DNS endpoint mechanism this
  ADR reuses for the datastore address).
- VERIFICATION G5 — FQDN endpoint survives cold restart on a new DHCP IP.
- VERIFICATION G9 — HA external-datastore cold-restart survival (this ADR's verification).
- k3s docs: High Availability External DB (`docs.k3s.io/datastore/ha`), Cluster Datastore
  (`docs.k3s.io/datastore`), server CLI (`docs.k3s.io/cli/server`). Verified against k3s
  v1.36 (stable) on 2026-06-27.
