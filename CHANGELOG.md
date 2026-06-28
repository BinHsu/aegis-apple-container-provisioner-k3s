# Changelog

All notable changes to k3ac are documented here. Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

---

## [v0.6.0] — day-2 operations

### Added

- **`-snapshot`**: saves an etcd snapshot of the managed datastore to a host path via writable
  bind-mount (ephemeral etcdctl container, mutual TLS). Safe — no `-force` needed.
- **`-restore` + `-snapshot-file`**: stops the cluster, rebuilds every etcd member's data volume
  from the snapshot (each member restores from the same file with the same `--initial-cluster`
  so the restored quorum is the same FQDN mesh as create), verifies the marker key
  (`/registry/namespaces/kube-system`), and restarts k3s servers/agents. Preserves server/agent
  state volumes and datastore TLS. Requires `-force`.
- **`-upgrade` + `-image`**: rolling k3s version upgrade — visits k3s nodes one at a time
  (servers before agents), cordon → drain → delete stale Node object → recreate container on
  target image (state volume preserved) → wait for Ready → uncordon. Pins the outgoing image for
  `-rollback`. Requires `-force`.
- **`-rollback`**: rolls the cluster back to the image pinned at the last `-upgrade`, using the
  same rolling orchestration. Itself reversible (pins the image it rolled away from). Requires
  `-force`.
- **`-rotate-certs`**: regenerates the managed etcd CA + every member/client cert (stdlib
  `crypto/x509`), overwrites the existing bind-mount dirs, then rotates each k3s server's own
  certificates offline (`k3s certificate rotate` on the stopped data volume). Requires `-force`.
- **`-rotate-token`**: generates a new K3S_TOKEN, runs `k3s token rotate` on a live server to
  re-encrypt the cluster's bootstrap data in etcd, then recreates every server and agent with the
  new token in `K3S_TOKEN` env (state volumes preserved). Requires `-force`.
- **`-force`**: safety gate for all destructive day-2 verbs. Without it the command prints the
  plan and exits non-zero.

---

## [v0.5.0] — mutual TLS, add-server, datastore tunables, merge-kubeconfig, env

### Added

- **Managed etcd mutual TLS** (`etcd_tls.go`): k3ac generates (host-side, stdlib `crypto/x509`,
  no external tooling) one CA, one server cert per etcd member (SAN = member FQDN + localhost +
  127.0.0.1), and one client cert for the k3s servers. Bundle delivered by host bind-mount
  (ADR-0001). etcd runs with `--peer-client-cert-auth` and `--client-cert-auth`; k3s servers
  connect with `--datastore-cafile/--datastore-certfile/--datastore-keyfile`. FQDN SANs keep TLS
  valid across the DHCP cold-restart IP shift.
- **`-add-server`**: adds one control-plane server to a live HA cluster and rewrites the haproxy
  backend config to include it. Supports `-server-memory`, `-server-cpus`, `-node-label`, and
  `-k3s-server-arg`.
- **`-datastore-image`**: overrides the managed etcd member image (managed-etcd path only).
- **`-datastore-memory`**: overrides the managed etcd member memory in MB (managed-etcd path only).
- **`-merge-kubeconfig`**: merges the named cluster's kubeconfig into `~/.kube/config` via
  `kubectl` and sets it as the current context.
- **`-env KEY=VALUE`** (repeatable): injects an environment variable into every k3s node. Also
  backed by `-config` (`envVars` field).
- `-add-agents` now threads `-agent-memory`, `-agent-cpus`, `-node-label`, and `-k3s-agent-arg`
  so a post-create agent is configured like a create-time one.

---

## [v0.4.0] — operability and tunability

### Added

- **`-k3s-server-arg`** (repeatable): extra k3s flag appended verbatim to every server after
  all built-in flags (last-one-wins; turns the fixed recipe open).
- **`-k3s-agent-arg`** (repeatable): same for agents.
- **`-node-label KEY=VALUE`** (repeatable): k3s node label applied to every node at create.
- **`-manifest <path>`** (repeatable): host-side manifest file auto-deployed via the bootstrap
  server's auto-deploy directory, delivered by bind-mount — no `container cp`.
- **`-server-cpus` / `-agent-cpus`**: per-role vCPU count (default 2).
- **`-list`**: prints name, server/agent counts, URL, and image for each cluster under
  `-state-dir`. Pure file I/O — works with the `container` daemon down.
- **`-start` / `-stop`**: ordered lifecycle across a cluster's nodes (datastore → servers →
  agents on start; reverse on stop). Re-arms `net.ipv4.ip_forward=1` on each k3s node after
  start.

### Internal

- `applyConfig` converted from positional arguments to a `flagRefs` struct; node set from
  positional to `nodeSpec` struct. Removes the argument-order footgun and keeps call sites
  self-documenting.

---

## [v0.3.0] — HA control plane: API load balancer + 3-node etcd datastore (ADR-0003)

### Added

- **haproxy L4 API load balancer**: an haproxy container (`<cluster>-api.<domain>`, `mode tcp`)
  fronts the k3s server pool. The kubeconfig endpoint is the LB FQDN; `-add-server` and
  `-remove-node` rewrite the haproxy config live.
- **3-node external etcd cluster as the managed datastore**: replaces the single managed-Postgres
  VM. Each etcd member runs as its own micro-VM with FQDN naming (`<cluster>-etcd-<i>.<domain>`);
  peer membership is name-bound, not IP-bound, so the quorum survives the DHCP cold-restart IP
  shift that kills embedded etcd (ADR-0002 proven). Member count is configurable via
  `-datastore-members` (must be odd and ≥ 3; default 3). Spike-verified on hardware; promoted to
  ADR-0003.
- **`-datastore-members`**: sets the managed etcd cluster size.

### Changed

- The single managed-Postgres auto-provision path is retired. Bring-your-own
  `-datastore-endpoint` (Postgres, MySQL/MariaDB, external etcd) is unchanged.

---

## [v0.2.0] — node membership ops, config file, HA spike

### Added

- **`-add-agents N`**: add N agent nodes to a running cluster (auto-join via the saved FQDN +
  token).
- **`-remove-node <name>`**: drain a node from Kubernetes, tear down its container and named
  volume, and remove it from `state.json`. Refuses to remove the last server (use `-destroy`).
- **`-config <path>`**: declarative JSON cluster spec (stdlib-only, no YAML dependency). Explicit
  flags override file values, which override built-in defaults. Unknown keys are rejected.
- HA external-datastore spike on hardware (G9): two k3s servers against a shared Postgres VM at
  a stable FQDN survive the whole-cluster cold-restart DHCP IP shift. Embedded etcd does not.
  Promoted to ADR-0002.

---

## [v0.1.0] — single-server baseline

### Added

- k3s cluster bring-up as Apple Silicon micro-VMs via the `container` CLI: one k3s server +
  N agents, each on its own named volume.
- **Named-volume sqlite datastore**: `<cluster>-<node>-k3s:/var/lib/rancher/k3s` (block-backed
  ext4; sqlite WAL semantics safe on native POSIX, not virtio-fs).
- **FQDN endpoint** (`-dns-domain`): every node named `<node>.<domain>`; the API server cert
  covers the FQDN via `--tls-san`. Endpoint survives cold-restart DHCP IP changes (G5).
- **Host-side TLS readiness + bind-mount kubeconfig delivery**: the provisioner dials
  `https://<server-IP>:6443` directly (no `container cp`, no guest agent). k3s writes the
  kubeconfig via `--write-kubeconfig /mnt/k3s-out/k3s.yaml`; the provisioner rewrites the
  server URL to the FQDN and saves it to `<stateDir>/<cluster>/kubeconfig` (ADR-0001).
- **Full lifecycle teardown**: `state.json` path and label-sweep fallback
  (`k3s.cluster.name=<name>`). Destroy completes in ~1 s with no daemon hang.
- **`-destroy`**, **`-token`**, **`-server-memory`**, **`-agent-memory`**, **`-network`**,
  **`-state-dir`**, **`-image`**, **`-name`**, **`-agents`**, **`-dns-domain`** flags.
- Hardware-verified: G1–G7 on Apple Silicon, `container` 1.0.0, `rancher/k3s:v1.32.5-k3s1`.
