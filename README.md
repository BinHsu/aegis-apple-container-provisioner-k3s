# aegis-apple-container-provisioner-k3s

A standalone launcher that boots k3s clusters as Apple Silicon micro-VMs via Apple's [`container`](https://github.com/apple/container) tool — **NOT** a k3s upstream provider interface (k3s has none).

> **Status: G1 VERIFIED — G2/G3/G5 are the next hardware gates.** G1 (does k3s's embedded
> containerd start under Apple `vminitd` with `--cap-add ALL`?) passed 2026-06-26 with
> `rancher/k3s:v1.32.5-k3s1`. G2 (pod networking), G3 (named-volume datastore persistence
> across cold restart), and G5 (FQDN endpoint survives cold-restart IP change) are the
> remaining hardware gates — verified by the operator; see
> [`docs/VERIFICATION.md`](docs/VERIFICATION.md).

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
(`<cluster>-<node>-k3s`) rather than a host-path bind-mount. This is the v0.1.0 lesson
from the Talos sibling: host-path bind-mounts are virtio-fs shares the guest cannot chmod
(EPERM). Named volumes are block-backed ext4 owned by the guest root, so chmod succeeds
and the sqlite datastore writes correctly. See `docs/VERIFICATION.md` G3.

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
go run ./cmd/aegis-k3s -dns-domain "" -name aegis -agents 1
```

## Lifecycle

```
validate → ensureNetwork → DNS domain precheck
  → prepareNodeVolumes (create named volumes, stale-state guard)
  → launch SERVER (sqlite + host-gw + tls-san=<server-fqdn> + K3S_TOKEN)
  → waitForIPv4 → exec sysctl ip_forward=1 → poll k3s.yaml via container cp (readiness + kubeconfig delivery)
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

**What the provisioner does:** after the server starts, it polls `container cp
<server-fqdn>:/etc/rancher/k3s/k3s.yaml` until k3s writes the file (a reliable
initialization signal — k3s only creates `k3s.yaml` once the API server is up and the CA
has been issued). It then rewrites the server address from the loopback
(`https://127.0.0.1:6443`) to the FQDN endpoint (`https://<server-fqdn>:6443`), which is
valid because `--tls-san` covers the FQDN. The FQDN endpoint survives cold-restart IP
changes (gate G5).

**Why `container cp` and not `container exec`:** `container exec` mangles the rancher/k3s
multi-call binary's args. `k3s kubectl`, `k3s crictl`, etc. are all symlinks to the same
`k3s` binary; `container exec <id> k3s kubectl ...` produces
`unknown command "kubectl" for "kubectl"` — verified 2026-06-26. `container cp` has no
such restriction and copies the file directly from the container filesystem.

## Usage

```sh
# Prerequisites (once per macOS boot):
sudo container system dns create aegis

# Create a 1-server + 1-agent cluster (FQDN mode, default):
go run ./cmd/aegis-k3s -name aegis -agents 1

# Create with a custom domain:
go run ./cmd/aegis-k3s -name aegis -agents 1 -dns-domain myk3s

# Create in IP-only mode (no FQDN, no DNS prereq):
go run ./cmd/aegis-k3s -name aegis -agents 1 -dns-domain ""

# Tear it down (removes nodes, named volumes, network, state):
go run ./cmd/aegis-k3s -name aegis -destroy
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

## Open hardware gates

- **G2:** pod-to-pod networking across vmnet node-VMs with `--flannel-backend=host-gw`
- **G3:** named-volume datastore persists across `container stop/start` and `rm` + relaunch
- **G5:** FQDN endpoint + named volume survives a cold restart on a new DHCP IP

See [`docs/VERIFICATION.md`](docs/VERIFICATION.md) for hypotheses, test commands, and
fill-in sections.

## Requirements

- macOS 26+, Apple Silicon
- [`container`](https://github.com/apple/container) >= 1.0.0
- Go 1.26+

## License

[MIT](LICENSE)
