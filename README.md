# aegis-apple-container-provisioner-k3s

A standalone launcher that boots k3s clusters as Apple Silicon micro-VMs via Apple's [`container`](https://github.com/apple/container) tool — **NOT** a k3s upstream provider interface (k3s has none).

> **Status: v0.1.0 — all hardware gates GREEN (2026-06-27).** G1–G7 verified on Apple
> Silicon with `container` 1.0.0 and `rancher/k3s:v1.32.5-k3s1`: k3s boots under `vminitd`,
> multi-node `host-gw` networking, named-volume sqlite persistence across cold restart,
> host-side readiness + bind-mount kubeconfig delivery (no `container cp`), FQDN endpoint
> survives cold-restart IP changes, real workload + Traefik ingress, and no-hang teardown.
> A forker clean-room run (the documented commands, from zero) reproduced the full lifecycle.
> See [`docs/VERIFICATION.md`](docs/VERIFICATION.md) for the first-person runbook and
> [`docs/ADR/0001-kubeconfig-delivery-via-host-bind-mount.md`](docs/ADR/0001-kubeconfig-delivery-via-host-bind-mount.md)
> for the kubeconfig-delivery design.

## What it is (and isn't)

- **Is:** a stdlib-only Go launcher that execs the `container` CLI to boot a single-server
  k3s cluster (1 server + N agents) as per-node micro-VMs, then tears it down. The
  exec-the-CLI pattern is carried over verbatim from the Talos sibling.
- **Isn't:** an implementation of any upstream interface. The Talos sibling satisfies
  `siderolabs/talos/pkg/provision.Provisioner`; **k3s has no equivalent pluggable
  provisioner**, so this is a freestanding launcher with no upstream merge path.

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
| Datastore engine | etcd | **sqlite single-server** — `--cluster-init` deliberately OFF |
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

## Lifecycle

```
validate → ensureNetwork → DNS domain precheck
  → prepareNodeVolumes (create named volumes, stale-state guard)
  → launch SERVER (sqlite + host-gw + tls-san=<server-fqdn> + K3S_TOKEN)
  → waitForIPv4 → exec sysctl ip_forward=1 → TLS dial https://<server-fqdn>:6443 (readiness) → os.Stat /mnt/k3s-out/k3s.yaml via bind-mount (kubeconfig delivery)
  → launch each AGENT (K3S_URL=https://<server-fqdn>:6443 + K3S_TOKEN)
  → waitForIPv4 → exec sysctl → saveState
```

Create **refuses to boot onto stale state**: if a node's named volume already exists
from a prior run, it fails and tells you to destroy first.

```
destroy: stop + rm each node (by FQDN container ID from saved state)
  → delete each node's named volume
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
   `https://127.0.0.1:6443` to the FQDN endpoint (`https://<server-fqdn>:6443`), and
   writes the result to `<stateDir>/<cluster>/kubeconfig`. Zero `container cp` or
   `container exec` involved. The FQDN endpoint survives cold-restart IP changes (gate G5).

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

# Tear it down (removes nodes, named volumes, network, state):
go run ./cmd/k3ac -name aegis -destroy

# Grow / shrink a running cluster (membership ops — no recreate):
go run ./cmd/k3ac -name aegis -add-agents 2               # add 2 agents (auto-join via the FQDN endpoint)
go run ./cmd/k3ac -name aegis -remove-node aegis-agent-2  # drain from Kubernetes, then tear the node down
```

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

| Flag | Default | Description |
|---|---|---|
| `-name` | `aegis` | Cluster name (also the label value and state-dir key) |
| `-dns-domain` | `aegis` | Apple container DNS domain for FQDN node names; `""` = IP-only fallback |
| `-agents` | `1` | Number of agent (worker) nodes |
| `-image` | pinned | `rancher/k3s` image tag (empty = `rancher/k3s:v1.32.5-k3s1`) |
| `-token` | random | K3S_TOKEN (empty = generated with `crypto/rand`) |
| `-server-memory` | `2048` | Server memory in MB |
| `-agent-memory` | `2048` | Agent memory in MB |
| `-state-dir` | `_out/clusters` | Directory for `state.json` |
| `-network` | `default` | apple/container network name |
| `-destroy` | `false` | Destroy the named cluster instead of creating |
| `-add-agents` | `0` | Add N agent nodes to an existing cluster (membership op; auto-join via FQDN) |
| `-remove-node` | `""` | Remove a node by name from an existing cluster (drains it from Kubernetes first; refuses the server) |

## Local checks

```sh
make build      # go build ./...
make vet        # go vet ./...
make test       # go test ./...   (BVA recipe-lock + volume + FQDN tests)
make fmt        # fail if not gofmt-clean
make secrets    # gitleaks secret scan
make check      # all of the above
```

CI (`go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest run ./...`) runs
automatically on push/PR via `.github/workflows/ci.yml`.

## Verified hardware gates (2026-06-27)

All green on Apple Silicon, `container` 1.0.0, `rancher/k3s:v1.32.5-k3s1` — see
[`docs/VERIFICATION.md`](docs/VERIFICATION.md) for the first-person runbook with exact commands:

- **G1** — k3s embedded containerd boots under `vminitd` + `--cap-add ALL`
- **G2** — pod-to-pod across nodes with `--flannel-backend=host-gw` (0% loss both ways)
- **G3** — named-volume sqlite datastore persists across a cold container stop/start
- **G4** — host-side TLS readiness + bind-mount kubeconfig delivery (zero `container cp`)
- **G5** — FQDN endpoint survives a cold restart on a new DHCP IP (zero re-point)
- **G6** — real workload (nginx) + built-in Traefik ingress, host-reachable (200 / 404)
- **G7** — teardown via both `state.json` and label-sweep paths, no daemon hang (~1 s)

## Requirements

- macOS 26+, Apple Silicon
- [`container`](https://github.com/apple/container) >= 1.0.0
- Go 1.26+

## License

[MIT](LICENSE)
