# aegis-apple-container-provisioner-k3s

A standalone launcher that boots k3s clusters as Apple Silicon micro-VMs via Apple's [`container`](https://github.com/apple/container) tool — **NOT** a k3s upstream provider interface (k3s has none).

> **Status: v0.7.0 — runs on Apple `container` 1.5.0 (v0.6.0 and earlier: 1.0.0).** G1–G10 (v0.1.0–v0.2.0) plus additional v0.3.0–v0.6.0
> gates verified on Apple Silicon with `container` 1.0.0 and `rancher/k3s:v1.32.5-k3s1`:
> k3s boots under `vminitd`, multi-node `host-gw` networking, named-volume persistence,
> host-side readiness + bind-mount kubeconfig delivery (no `container cp`), FQDN endpoint
> survives cold-restart IP changes, real workload + Traefik ingress, no-hang teardown,
> live membership ops (add/remove agents, add server), **HA control plane on a 3-node
> mutual-TLS etcd cluster** fronted by an haproxy L4 API load balancer, and a full suite of
> day-2 operations (snapshot/restore, rolling upgrade/rollback, cert/token rotation).
> See [`docs/VERIFICATION.md`](docs/VERIFICATION.md) for the first-person runbook,
> [`docs/ADR/0001-kubeconfig-delivery-via-host-bind-mount.md`](docs/ADR/0001-kubeconfig-delivery-via-host-bind-mount.md)
> for kubeconfig delivery, and
> [`docs/ADR/0002-ha-via-external-datastore-not-embedded-etcd.md`](docs/ADR/0002-ha-via-external-datastore-not-embedded-etcd.md)
> for the HA design.

## Quickstart

1. **Prerequisites:** macOS 26+, Apple Silicon, [`container`](https://github.com/apple/container) >= 1.0.0, Go 1.26+, kubectl (any recent version)
2. **One-time DNS** (once per macOS boot — does not survive a reboot):
   ```sh
   sudo container system dns create aegis
   ```
3. **Create a cluster** (1 server + 1 agent, FQDN mode):
   ```sh
   go run ./cmd/k3ac -name aegis -agents 1
   ```
4. **Access:**
   ```sh
   export KUBECONFIG=_out/clusters/aegis/kubeconfig
   kubectl get nodes
   ```
5. **HA control plane** (3 servers on a managed 3-node etcd cluster + 1 agent):
   ```sh
   go run ./cmd/k3ac -name hav -servers 3 -agents 1
   ```
   **Destroy** any cluster when done:
   ```sh
   go run ./cmd/k3ac -name aegis -destroy
   ```

## Release notes — which Apple `container` each release targets

| k3ac release | Apple `container` verified | k3s image | Notes |
|---|---|---|---|
| *unreleased (v0.8.0)* | 1.5.0 | `rancher/k3s:v1.32.5-k3s1` | Opt-in `-egress`: host-side `ac-egress` relay for hosts where vmnet NAT is dead (Cloudflare WARP connected). `-start` refreshes cluster `/etc/hosts`. Nodes now mount debugfs / tracefs / bpffs (default, idempotent) for eBPF agents. See CHANGELOG |
| **v0.7.0** | **1.5.0** | `rancher/k3s:v1.32.5-k3s1` | Required for 1.5.0: `/proc/sys` remount, `/` rshared, TSO/GSO off on node NIC. New opt-in `-dns` / `-ca-dir` for TLS-interception networks (e.g. Cloudflare WARP). See CHANGELOG |
| v0.6.0 | 1.0.0 | `rancher/k3s:v1.32.5-k3s1` | day-2 operations |
| v0.5.0 | 1.0.0 | `rancher/k3s:v1.32.5-k3s1` | |
| v0.4.0 | 1.0.0 | `rancher/k3s:v1.32.5-k3s1` | |
| v0.3.0 | 1.0.0 | `rancher/k3s:v1.32.5-k3s1` | |
| v0.2.0 | 1.0.0 | `rancher/k3s:v1.32.5-k3s1` | |

Releases up to v0.6.0 do **not** start on Apple `container` 1.5.0 (the first node fails at
`sysctl net.ipv4.ip_forward` with "Read-only file system"). Full per-release details: [`CHANGELOG.md`](CHANGELOG.md).

## Requirements

- macOS 26+, Apple Silicon
- [`container`](https://github.com/apple/container) >= 1.0.0 (1.5.0 needs v0.7.0 or later — see Release notes)
- Go 1.26+
- kubectl (any recent version; required for `-merge-kubeconfig` and all cluster access)

## What it is (and isn't)

- **Is:** a stdlib-only Go launcher that execs the `container` CLI to boot a k3s cluster as
  per-node micro-VMs, then tears it down. Single-server embedded sqlite by default; an opt-in
  **multi-server HA control plane** on a 3-node external etcd cluster (auto-provisioned) or a
  bring-your-own datastore (`-servers N`, see below). The exec-the-CLI pattern is carried over
  verbatim from the Talos sibling.
- **Isn't:** an implementation of any upstream interface. The Talos sibling satisfies
  `siderolabs/talos/pkg/provision.Provisioner`; **k3s has no equivalent pluggable
  provisioner**, so this is a freestanding launcher with no upstream merge path. When `-servers
  N` is used with no `-datastore-endpoint`, k3ac auto-provisions a 3-node etcd cluster with
  mutual TLS — both the control plane and the datastore are HA (ADR-0002, ADR-0003).

## Sibling relationship

This is the **k3s sibling of the Talos provisioner**. Same skeleton (`provider/apple` +
`cmd` driver + G-gate `VERIFICATION.md` + BVA recipe-lock tests), different guest. The
key recipe deltas from Talos:

| Aspect | Talos sibling (verified) | k3s (this launcher) |
|---|---|---|
| Image | `ghcr.io/siderolabs/talos` | `rancher/k3s:v1.32.5-k3s1` (G1 VERIFIED) |
| Caps | `--cap-add ALL` | `--cap-add ALL` (same; k3s embedded containerd needs CAP_SYS_ADMIN) |
| tmpfs set | `/run /system /tmp` + overlays, NOT `/opt` | **only `/run` and `/tmp`** — `/var` is NOT tmpfs (named-volume datastore) |
| Datastore | Named volumes `/var` + `/system/state` | Named volume `<cluster>-<node>-k3s:/var/lib/rancher/k3s` (one per node) |
| Datastore engine | etcd | **sqlite single-server** (default), or **auto-provisioned 3-node external etcd (mutual TLS) for N-server HA** (`-servers N`) — embedded etcd / `--cluster-init` deliberately OFF (IP-bound; see ADR-0002) |
| API load balancer | built-in LB | **haproxy L4 (mode tcp) at `<cluster>-api.<domain>`** (HA path only; v0.3.0) |
| Networking | flannel via machine config | `--flannel-backend=host-gw` + `--tls-san <server-fqdn>` |
| Endpoint | FQDN via `-dns-domain` | FQDN via `-dns-domain` (same pattern, mirrored from v0.2.0) |
| Memory unit | `MB` suffix | `MB` suffix (carried over) |
| Cluster secret | baked into machine config | shared `K3S_TOKEN` (crypto/rand if unset) |
| Labels | `talos.owned/cluster.name/type` | `k3s.owned/cluster.name/role` |

## Datastore: named volumes (not host paths)

`/var/lib/rancher/k3s` is backed by an Apple `container` **named volume**
(`<cluster>-<node>-k3s`) rather than a host-path bind-mount. Named volumes are
block-backed ext4 owned by the guest root. The reason for avoiding a virtio-fs
bind-mount here is sqlite's WAL and advisory file-locking semantics, which are not
well-defined over virtio-fs shares. Plain sequential file writes to virtio-fs work fine
(spiked 2026-06-27); it is the sqlite-specific locking paths that are the concern. See
`docs/VERIFICATION.md` G3.

## FQDN endpoint (`-dns-domain`)

By default, every node is named `<node>.<dns-domain>` (e.g. `aegis-server-1.aegis`).
Apple's container DNS registers this FQDN, so it resolves from the host and the record
follows the container IP across cold restarts. The server API cert covers the FQDN via
`--tls-san`, so host kubeconfig access via the FQDN stays valid even after a DHCP IP
change. This is the v0.2.0 lesson from the Talos sibling.

### Prerequisites

```sh
# Run once per macOS boot session (DNS registration does not survive macOS reboot):
sudo container system dns create aegis
```

This registers the `aegis` search domain with the host resolver. Without it, Create
fails early with a clear error telling you to run the above command.

To disable FQDN naming and fall back to IP-based naming (v0.1.x behaviour):

```sh
go run ./cmd/k3ac -dns-domain "" -name aegis -agents 1
```

## High availability (`-servers N`)

By default the cluster is one server on embedded sqlite. Pass `-servers N` (N≥2) for an HA
control plane: every server runs **stateless against a shared external datastore** — there is
no embedded etcd. With no `-datastore-endpoint`, k3ac **auto-provisions a managed 3-node
external etcd cluster** (with mutual TLS) and fronts the servers with an **haproxy L4 API
load balancer** at `<cluster>-api.<domain>` (one command):

```sh
# Three servers + one agent; k3ac provisions a 3-node etcd cluster (<cluster>-etcd-{1,2,3})
# and an haproxy LB (<cluster>-api.<domain>), then wires all servers to them:
go run ./cmd/k3ac -name hav -servers 3 -agents 1
```

Or bring your own datastore (Postgres, MySQL/MariaDB, or external etcd via kine):

```sh
go run ./cmd/k3ac -name hav -servers 2 \
  -datastore-endpoint 'postgres://user:pass@db.aegis:5432/kine'
```

**Why external datastore, not embedded etcd:** etcd's peer membership is IP-bound and cannot
reform quorum after the vmnet DHCP IP shift a cold restart causes — verified dead on this
substrate. An external datastore addressed by FQDN has no IP-bound membership, so the control
plane reconnects by name and survives the shift (verified, G9/G10). See
[`docs/ADR/0002-ha-via-external-datastore-not-embedded-etcd.md`](docs/ADR/0002-ha-via-external-datastore-not-embedded-etcd.md).

**Why a 3-node etcd cluster, not a single datastore:** a single-VM datastore is itself a
single point of failure. A 3-node etcd quorum tolerates losing one member and keeps serving.
Member count is configurable via `-datastore-members` (must be odd and ≥ 3). See
[`docs/ADR/0003-datastore-ha.md`](docs/ADR/0003-datastore-ha.md).

**Mutual TLS:** k3ac generates (host-side, stdlib `crypto/x509`, no external tooling) one CA,
one server cert per etcd member (SAN = member FQDN + localhost + 127.0.0.1), and one client
cert for the k3s servers. Every etcd member and k3s server mount the bundle via the same host
bind-mount mechanism as the kubeconfig (ADR-0001). The FQDN SANs keep TLS valid across the
vmnet DHCP IP shift — no IP is ever a SAN.

**API load balancer:** the haproxy LB container (`<cluster>-api.<domain>`) runs in `mode tcp`
in front of the server pool. The kubeconfig endpoint is `https://<cluster>-api.<domain>:6443`
and the API server cert covers that SAN. Adding or removing a server (via `-add-server` or
`-remove-node`) rewrites the haproxy config in place.

**Scope:** managed HA requires `-dns-domain` (all components are reached by FQDN).

## Architecture

Single-node default runs sqlite embedded in the k3s server VM — no extra infrastructure.
HA mode auto-provisions a 3-node etcd quorum and an haproxy API load balancer, then
wires every server node to both (one `go run` command; see above).

```
aegis-apple-container-provisioner-k3s  (k3ac CLI)
k3s on apple/container  (macOS 26+ · Apple Silicon)

Single-node default: sqlite (k3s built-in) — no etcd or haproxy.
HA mode (shown below): external etcd quorum + haproxy API LB.

┌─────────────────────────────────────────────────────────────────┐
│                macOS 26+  ·  Apple Silicon host                 │
│                                                                 │
│  ┌───────────────────────────────────────────────────────────┐  │
│  │      apple/container runtime  (vmnet · per-VM DHCP)       │  │
│  │                                                           │  │
│  │  ┌──────────── etcd quorum  (HA only) ─────────────────┐  │  │
│  │  │  ┌───────────┐     ┌───────────┐     ┌───────────┐  │  │  │
│  │  │  │hav-etcd-1 │◄───►│hav-etcd-2 │◄───►│hav-etcd-3 │  │  │  │
│  │  │  │192.168.x.a│     │192.168.x.b│     │192.168.x.c│  │  │  │
│  │  │  │ micro-VM  │     │ micro-VM  │     │ micro-VM  │  │  │  │
│  │  │  └───────────┘     └───────────┘     └───────────┘  │  │  │
│  │  │     external datastore — k3s pluggable backend      │  │  │
│  │  └─────────────────────────────────────────────────────┘  │  │
│  │                             │                             │  │
│  │                             ▼                             │  │
│  │  ┌──────────── haproxy API LB  (HA only) ──────────────┐  │  │
│  │  │        hav-api  ·  192.168.x.d  ·  micro-VM         │  │  │
│  │  │     load-balances kube-apiserver across servers     │  │  │
│  │  └─────────────────────────────────────────────────────┘  │  │
│  │                             │                             │  │
│  │                             ▼                             │  │
│  │  ┌──────────── k3s Server Nodes ───────────────────────┐  │  │
│  │  │  ┌───────────┐  ┌───────────┐        ┌───────────┐  │  │  │
│  │  │  │ k3s-srv-1 │  │ k3s-srv-2 │ . . .  │ k3s-srv-N │  │  │  │
│  │  │  │192.168.x.e│  │192.168.x.f│        │    ...    │  │  │  │
│  │  │  │ micro-VM  │  │ micro-VM  │        │ micro-VM  │  │  │  │
│  │  │  └───────────┘  └───────────┘        └───────────┘  │  │  │
│  │  └─────────────────────────────────────────────────────┘  │  │
│  │                                                           │  │
│  │  ┌──────────── k3s Agent Nodes  (workers) ─────────────┐  │  │
│  │  │  ┌───────────┐  ┌───────────┐        ┌───────────┐  │  │  │
│  │  │  │ worker-1  │  │ worker-2  │ . . .  │ worker-N  │  │  │  │
│  │  │  │192.168.x.g│  │192.168.x.h│        │    ...    │  │  │  │
│  │  │  │ micro-VM  │  │ micro-VM  │        │ micro-VM  │  │  │  │
│  │  │  └───────────┘  └───────────┘        └───────────┘  │  │  │
│  │  └─────────────────────────────────────────────────────┘  │  │
│  │                                                           │  │
│  └───────────────────────────────────────────────────────────┘  │
│                                                                 │
└─────────────────────────────────────────────────────────────────┘

Substrate is identical to the Talos lane:
  · Each node = its own micro-VM on apple/container
  · vmnet gives each node a separate DHCP-assigned IP

HA datastore contrast (the lane differentiator):
  ┌────────────────────────────┬─────────────────────────────┐
  │  Talos lane                │  k3s lane (this project)    │
  ├────────────────────────────┼─────────────────────────────┤
  │  embedded etcd inside      │  external etcd quorum       │
  │  control-plane nodes       │  (3 dedicated etcd VMs)     │
  │  (no extra VMs needed)     │  + haproxy API LB VM        │
  └────────────────────────────┴─────────────────────────────┘
```

## Lifecycle

```
validate → ensureNetwork → DNS domain precheck
  → [HA only] generate etcd TLS bundle (CA + per-member server certs + k3s client cert, host-side)
  → [HA only] provision 3-node etcd cluster (<cluster>-etcd-{1,2,3}, FQDN-named, TLS) → wait for each member's client port
  → [HA only] provision haproxy L4 API LB (<cluster>-api.<domain>, mode tcp, bind-mount config)
  → prepareNodeVolumes (create named volumes, stale-state guard)
  → launch BOOTSTRAP server (host-gw + tls-san=<server-fqdn> + tls-san=<cluster>-api.<domain> + K3S_TOKEN + --datastore-endpoint when HA)
  → waitForIPv4 → exec sysctl ip_forward=1 → TLS dial https://<server-fqdn>:6443 (readiness) → os.Stat /mnt/k3s-out/k3s.yaml via bind-mount (kubeconfig delivery)
  → [HA only] launch each ADDITIONAL server against the shared etcd datastore (no --cluster-init)
  → launch each AGENT (K3S_URL=https://<cluster>-api.<domain>:6443 when HA, or server FQDN for single-server)
  → waitForIPv4 → exec sysctl → saveState
```

**Node preparation** (the "exec sysctl" step above; also run by `-start`, `-add-agents`, `-add-server`, `-upgrade`/`-rollback`,
`-rotate-*` and `-restore`): remount `/proc/sys` rw, make `/` rshared, mount debugfs on `/sys/kernel/debug`,
tracefs on `/sys/kernel/tracing` and bpffs on `/sys/fs/bpf` when the kernel offers them and they are not
already mounted (eBPF agents such as DataKit eBPF or OBI need them; see
[ADR-0005](docs/ADR/0005-node-ebpf-pseudo-filesystems.md)), turn off TSO/GSO on `eth0`, set `ip_forward=1`.
Every step is idempotent, and there is no flag: an existing cluster picks the mounts up on its next `-start`.

Create **refuses to boot onto stale state**: if a node's named volume already exists
from a prior run, it fails and tells you to destroy first.

```
destroy: stop + rm each node (by FQDN container ID from saved state)
  → delete each node's named volume (including etcd member volumes)
  → label sweep (k3s.cluster.name=<name>) to reclaim orphaned containers/volumes
    from a Create that failed before saveState
  → delete network → remove state dir (state.json)
```

## Access pattern: host kubeconfig

The provisioner writes a ready-to-use kubeconfig to `<stateDir>/<cluster>/kubeconfig`
(default: `_out/clusters/<cluster>/kubeconfig`) during create — no manual fetch or
server-URL rewrite needed:

```sh
export KUBECONFIG=_out/clusters/aegis/kubeconfig
kubectl get nodes
```

**What the provisioner does:** readiness and kubeconfig delivery use the host filesystem
and network directly — no guest agent involved.

1. **Readiness:** the provisioner dials `https://<server-IP>:6443` from the host. The
   kube-apiserver answers TLS on that port; no `container exec` or `container cp` needed.
2. **Kubeconfig delivery:** the server container bind-mounts the host cluster state
   directory at `/mnt/k3s-out` inside the VM. k3s writes its kubeconfig there via
   `--write-kubeconfig /mnt/k3s-out/k3s.yaml --write-kubeconfig-mode 0644`. The
   provisioner polls `os.Stat` for the file, rewrites the server address from
   `https://127.0.0.1:6443` to the FQDN endpoint (`https://<cluster>-api.<domain>:6443`
   for HA, or `https://<server-fqdn>:6443` for single-server), and writes the result to
   `<stateDir>/<cluster>/kubeconfig`. Zero `container cp` or `container exec` involved.
   The FQDN endpoint survives cold-restart IP changes (gate G5).

**Why not `container cp` or `container exec`:** both ride the guest agent (vminitd) over
vsock. During k3s cold boot the guest is saturated extracting bundled images; this faults
the vsock channel and the cp process is killed externally — verified at 180 s timeout on
hardware. A faulted vsock also wedges `container stop` and `container rm` for two or more
minutes. The bind-mount + TLS-probe design avoids the guest agent entirely for the
critical path. See
[`docs/ADR/0001-kubeconfig-delivery-via-host-bind-mount.md`](docs/ADR/0001-kubeconfig-delivery-via-host-bind-mount.md)
for the full analysis. (`container exec` is still used for one early, standalone-binary
call: `sysctl net.ipv4.ip_forward=1`.)

## Usage

`k3ac` is **this repo's launcher** — not a k3s subcommand. k3s has no pluggable
provisioner interface (unlike Talos's `pkg/provision.Provisioner`), so cluster bring-up is
driven by this freestanding binary, which execs the `container` CLI to boot k3s nodes as
micro-VMs. Run it from source with `go run`, or build it once and call the binary:

```sh
go build -o k3ac ./cmd/k3ac
./k3ac -name aegis -agents 1
```

```sh
# Prerequisites (once per macOS boot — the pf DNS redirect does not survive a reboot):
sudo container system dns create aegis

# Create a 1-server + 1-agent cluster (FQDN mode, default):
go run ./cmd/k3ac -name aegis -agents 1

# Create with a custom domain:
go run ./cmd/k3ac -name aegis -agents 1 -dns-domain myk3s

# Create in IP-only mode (no FQDN, no DNS prereq):
go run ./cmd/k3ac -name aegis -agents 1 -dns-domain ""

# Create an HA control plane: 3 servers on a managed 3-node etcd cluster + 1 agent (one command):
go run ./cmd/k3ac -name hav -servers 3 -agents 1

# Create an HA cluster with a bring-your-own external datastore:
go run ./cmd/k3ac -name hav -servers 2 -agents 1 \
  -datastore-endpoint 'postgres://user:pass@db.aegis:5432/kine'

# Tear it down (removes nodes, etcd members, LB, named volumes, network, state):
go run ./cmd/k3ac -name aegis -destroy

# List all clusters under -state-dir:
go run ./cmd/k3ac -list

# Start / stop a cluster (ordered: datastore → servers → agents on start; reverse on stop):
go run ./cmd/k3ac -start aegis
go run ./cmd/k3ac -stop aegis

# Grow / shrink a running cluster (membership ops — no recreate):
go run ./cmd/k3ac -name aegis -add-agents 2               # add 2 agents (auto-join via FQDN)
go run ./cmd/k3ac -name aegis -remove-node aegis-agent-2  # drain from Kubernetes, then tear down
go run ./cmd/k3ac -name hav -add-server                   # add one control-plane server to an HA cluster

# Merge the cluster kubeconfig into ~/.kube/config and set-context:
go run ./cmd/k3ac -merge-kubeconfig aegis
```

### Day-2 operations (v0.6.0)

Destructive day-2 operations require `-force`. Without it, the command prints what it
WOULD do and exits non-zero — a dry-run guard.

```sh
# Snapshot the managed etcd datastore (safe — no -force needed):
go run ./cmd/k3ac -snapshot aegis
# -> prints: etcd snapshot saved to _out/clusters/aegis/snapshots/aegis-20260628T120000Z.db

# Restore from a snapshot (DESTRUCTIVE — rebuilds all etcd data volumes):
go run ./cmd/k3ac -restore aegis -snapshot-file _out/clusters/aegis/snapshots/aegis-20260628T120000Z.db -force

# Rolling upgrade to a new k3s image, one node at a time (servers first, then agents):
go run ./cmd/k3ac -upgrade aegis -image rancher/k3s:v1.33.0-k3s1 -force

# Roll back to the image pinned at the last -upgrade:
go run ./cmd/k3ac -rollback aegis -force

# Rotate the managed etcd TLS (regenerates CA + all member/client certs):
go run ./cmd/k3ac -rotate-certs aegis -force

# Generate a new K3S_TOKEN and re-register all servers + agents:
go run ./cmd/k3ac -rotate-token aegis -force
```

### Declarative config file (`-config`)

Describe a cluster once in JSON (stdlib-only — no YAML dependency) instead of repeating
flags. Explicit flags override file values, which override built-in defaults:

```sh
go run ./cmd/k3ac -config examples/cluster.json              # all settings from the file
go run ./cmd/k3ac -config examples/cluster.json -agents 3    # file, but override the agent count
```

Unknown keys are rejected (a typo like `serverMemoryMb` fails fast rather than being
ignored). See [`examples/cluster.json`](examples/cluster.json) (single-server) and
[`examples/cluster-ha.json`](examples/cluster-ha.json) (`"servers": 2` → managed-HA). Note:
because JSON cannot distinguish an absent `agents` key from `"agents": 0`, always declare
`agents` in the file. (`servers` is the opposite: absent → the default `1`, since 0 servers
is never valid.)

### Smoke test (optional — prove the cluster actually serves traffic)

```sh
export KUBECONFIG=_out/clusters/aegis/kubeconfig
kubectl apply -f examples/nginx.yaml       # nginx Deployment + ClusterIP Service
kubectl apply -f examples/ingress.yaml     # Traefik Ingress: host demo.local
kubectl wait --for=condition=available deployment/nginx --timeout=120s

# Host-side ingress check via Traefik's node port 80 (k3s ships Traefik by default):
NODE_IP=$(kubectl get node aegis-server-1 \
  -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}')
curl -s -o /dev/null -w '%{http_code}\n' -H 'Host: demo.local' http://${NODE_IP}/   # 200
curl -s -o /dev/null -w '%{http_code}\n' -H 'Host: nope.local' http://${NODE_IP}/   # 404
```

## Flags

All 38 flags are listed below. Flags marked *repeatable* may be passed more than once;
each occurrence adds one value to the list. Explicit flags always override `-config` file
values, which override built-in defaults.

### Create — cluster shape

| Flag | Default | Description |
|---|---|---|
| `-name` | `aegis` | Cluster name (also the label value and state-dir key) |
| `-image` | pinned | `rancher/k3s` image tag (empty = `rancher/k3s:v1.32.5-k3s1`) |
| `-state-dir` | `_out/clusters` | Directory for `state.json` and the cluster kubeconfig |
| `-network` | `default` | apple/container network name (default = built-in vmnet) |
| `-dns-domain` | `aegis` | Apple container DNS domain for FQDN node names; `""` = IP-only fallback |
| `-token` | random | K3S_TOKEN (empty = generated with `crypto/rand`) |
| `-servers` | `1` | Number of server (control-plane) nodes. `>1` = HA; with no `-datastore-endpoint` k3ac auto-provisions a managed 3-node etcd cluster (needs `-dns-domain`) |
| `-agents` | `1` | Number of agent (worker) nodes |
| `-server-memory` | `2048` | Server memory in MB |
| `-agent-memory` | `2048` | Agent memory in MB |
| `-server-cpus` | `2` | vCPUs per server node |
| `-agent-cpus` | `2` | vCPUs per agent node |

### Create — managed datastore

| Flag | Default | Description |
|---|---|---|
| `-datastore-endpoint` | `""` | Bring-your-own external datastore for HA (e.g. `postgres://user:pass@db.aegis:5432/kine`). Empty + `-servers>1` = k3ac auto-provisions a 3-node etcd cluster; empty + `-servers=1` = embedded sqlite |
| `-datastore-members` | `3` | Managed etcd cluster size (must be odd and ≥ 3). Ignored with a bring-your-own `-datastore-endpoint` |
| `-datastore-image` | pinned | Managed etcd member image (empty = `quay.io/coreos/etcd:v3.5.16`). Managed-etcd path only |
| `-datastore-memory` | `512` | Managed etcd member memory in MB. Managed-etcd path only |

### Create — node configuration (repeatable)

| Flag | Default | Description |
|---|---|---|
| `-k3s-server-arg` | — | Extra k3s flag appended verbatim to every server (repeatable). Appended after built-in flags, so it can override them (k3s is last-one-wins). Examples: `--disable=traefik`, `--cluster-cidr=10.96.0.0/16` |
| `-k3s-agent-arg` | — | Extra k3s flag appended verbatim to every agent (repeatable) |
| `-node-label` | — | k3s node label `KEY=VALUE` applied to every node at create (repeatable) |
| `-manifest` | — | Host-side manifest file auto-deployed via the bootstrap server's auto-deploy dir (repeatable). Delivered by bind-mount — no `container cp` |
| `-env` | — | Environment variable `KEY=VALUE` injected into every k3s node (repeatable) |

### Declarative config

| Flag | Default | Description |
|---|---|---|
| `-config` | `""` | Load cluster settings from a JSON file (explicit flags override file values). Unknown keys are rejected |

### Lifecycle

| Flag | Default | Description |
|---|---|---|
| `-list` | — | List all clusters under `-state-dir` (name, server/agent counts, URL, image) and exit. Pure file I/O — works with the `container` daemon down |
| `-start` | `""` | Start every node of the named cluster in dependency-safe order (datastore → servers → agents). Re-arms `ip_forward` on each k3s node |
| `-stop` | `""` | Stop every node of the named cluster in reverse order (agents → servers → datastore) |
| `-egress` | `false` | Run the `ac-egress` relay for this cluster and steer node + pod egress through it (create or `-start`; recorded in state so later `-start`s re-enable it; `-stop`/`-destroy` stop it). See [Egress relay](#egress-relay-ac-egress) |
| `-destroy` | `false` | Destroy the named cluster (nodes, etcd members, LB, named volumes, network, state dir) |
| `-merge-kubeconfig` | `""` | Merge the named cluster's kubeconfig into `~/.kube/config` (via `kubectl`) and `set-context`, then exit |

### Membership

| Flag | Default | Description |
|---|---|---|
| `-add-agents` | `0` | Add N agent nodes to an existing cluster (auto-join via the saved FQDN + token) |
| `-add-server` | `false` | Add one control-plane server to an existing HA cluster and update the API LB config |
| `-remove-node` | `""` | Remove a node by name from an existing cluster (drains it from Kubernetes first; refuses the last server — use `-destroy` instead) |

### Day-2 operations (v0.6.0)

All destructive verbs refuse to run without `-force`. Without it the command prints the
plan and exits non-zero.

| Flag | Default | Description |
|---|---|---|
| `-snapshot` | `""` | Save an etcd snapshot of the named cluster's managed datastore to a host path (safe — no `-force` needed) |
| `-restore` | `""` | Restore the named cluster's managed datastore from `-snapshot-file` (DESTRUCTIVE, needs `-force`) |
| `-snapshot-file` | `""` | Snapshot file path for `-restore` |
| `-upgrade` | `""` | Roll the named cluster's k3s nodes onto `-image`, one at a time, servers first (DESTRUCTIVE, needs `-force`) |
| `-rollback` | `""` | Roll the named cluster back to the image pinned at the last `-upgrade` (DESTRUCTIVE, needs `-force`) |
| `-rotate-certs` | `""` | Regenerate + redeliver the managed etcd TLS bundle and rotate each server's k3s certs (DESTRUCTIVE, needs `-force`) |
| `-rotate-token` | `""` | Generate a new K3S_TOKEN and re-register the named cluster's servers + agents (DESTRUCTIVE, needs `-force`) |
| `-force` | `false` | Confirm a DESTRUCTIVE day-2 operation (`-restore`/`-upgrade`/`-rollback`/`-rotate-certs`/`-rotate-token`) |

## Egress relay (`ac-egress`)

On a Mac with Cloudflare WARP connected, Apple `container` VMs get **no egress at all**: DNS to
public resolvers, TCP to literal IPs and ICMP to the vmnet gateway all time out, while ordinary
host processes reach the internet fine. The VMs *can* still open TCP to the host's own bridge
address (e.g. `192.168.200.1`). `ac-egress` uses exactly that path: a small userspace relay
bound only to the bridge address, whose outbound connections are ordinary host sockets.

It is a separate component (`egress/` package + `cmd/ac-egress`), stdlib-only, with no k3s
knowledge — it serves any VM on the bridge it is bound to. k3ac only consumes it.

| Port (bridge IP only) | Service |
|---|---|
| `15080/tcp` | transparent HTTP (routes by `Host`), plus explicit HTTP proxy (`CONNECT`, absolute-form) |
| `15443/tcp` | transparent TLS passthrough routed by ClientHello SNI (never decrypts) |
| `15053/udp+tcp` | DNS forwarder to the host's own resolvers (`/etc/resolv.conf`; WARP's 127.0.2.2/3) |

Guests are steered to it by an iptables `nat` chain `AC-EGRESS` (hooked into `OUTPUT` and
`PREROUTING`, so pods behind the node are covered too) that DNATs outbound tcp/80, tcp/443 and
dns/53 to non-private destinations. Private ranges (pod/service CIDRs, node IPs, LAN) are never
redirected, so no `HTTP_PROXY`/`NO_PROXY` is needed in any pod.

Safety properties: refuses to bind anything but an address on a `bridge*` interface (never
`0.0.0.0`, Wi-Fi, LAN, VPN or loopback); serves only sources inside that bridge's subnet; dials only
public unicast upstreams (checked on the resolved IP, so a guest cannot reach host loopback or LAN
services through it). TLS is passed through untouched, so a TLS-inspecting network still needs its
CA in the guests (`-ca-dir`, and the CA in pods that verify TLS).

```sh
# k3ac: create or start a cluster behind the relay (state remembers it; -stop kills it by pid)
go build -o _out/k3ac ./cmd/k3ac && go build -o _out/ac-egress ./cmd/ac-egress
_out/k3ac -state-dir _out/clusters -start tw-poc -egress

# standalone, for any Apple container VM on a network whose gateway is 192.168.200.1:
_out/ac-egress -listen 192.168.200.1 &
_out/ac-egress -listen 192.168.200.1 -guest-script | container exec -i <vm> sh
```

k3ac looks for `ac-egress` next to the `k3ac` binary, then on `PATH`. The relay runs detached
(pid in `<state-dir>/<name>/egress.pid`, log in `egress.log`) and lives until `-stop`/`-destroy`.
Not covered: HTTPS to literal IPs (no SNI), plain TCP on other ports, UDP other than DNS, IPv6.
Design and alternatives: [`docs/ADR/0004-egress-relay.md`](docs/ADR/0004-egress-relay.md).

## Local checks

```sh
make build      # go build ./...
make vet        # go vet ./...
make test       # go test ./...   (BVA recipe-lock + volume + FQDN + TLS tests)
make fmt        # fail if not gofmt-clean
make secrets    # gitleaks secret scan
make check      # all of the above
```

CI (`go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest run ./...`) runs
automatically on push/PR via `.github/workflows/ci.yml`.

## Verified hardware gates

All green on Apple Silicon, `container` 1.0.0, `rancher/k3s:v1.32.5-k3s1` — see
[`docs/VERIFICATION.md`](docs/VERIFICATION.md) for the first-person runbook with exact commands:

- **G1** — k3s embedded containerd boots under `vminitd` + `--cap-add ALL`
- **G2** — pod-to-pod across nodes with `--flannel-backend=host-gw` (0% loss both ways)
- **G3** — named-volume sqlite datastore persists across a cold container stop/start
- **G4** — host-side TLS readiness + bind-mount kubeconfig delivery (zero `container cp`)
- **G5** — FQDN endpoint survives a cold restart on a new DHCP IP (zero re-point)
- **G6** — real workload (nginx) + built-in Traefik ingress, host-reachable (200 / 404)
- **G7** — teardown via both `state.json` and label-sweep paths, no daemon hang (~1 s)
- **G8** — node membership: add/remove agents on a running cluster (server removal refused)
- **G9** — HA external-datastore: 2 servers survive a cold-restart DHCP IP shift (spike behind ADR-0002)
- **G10** — HA end to end: managed 3-node etcd cluster + haproxy LB + multi-server; cold-restart survival; clean teardown
- **G11+** — etcd mutual TLS, API LB bring-up, `-add-server`, v0.4.0 operability, and v0.6.0 day-2 ops (see VERIFICATION.md)

## License

[MIT](LICENSE)
