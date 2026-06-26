# aegis-apple-container-provisioner-k3s

A standalone launcher that boots k3s clusters as Apple Silicon micro-VMs via Apple's [`container`](https://github.com/apple/container) tool — **NOT** a k3s upstream provider interface (k3s has none).

> **Status: UNVERIFIED SPIKE DRAFT — not expected to run as-is.** This module was written
> by analogy from its verified sibling, [aegis-talos-apple-container-provisioner](https://github.com/BinHsu/aegis-talos-apple-container-provisioner),
> applying the Apple-`container` launch recipe to k3s instead of Talos. **Not one gate has
> been executed.** Every launch-recipe and lifecycle assumption is a hypothesis until the
> G-gates in [`docs/VERIFICATION.md`](docs/VERIFICATION.md) are run. Search the tree for
> `UNVERIFIED` and `PLACEHOLDER` to find every such assumption. Do not build anything on it.

## What it is (and isn't)

- **Is:** a stdlib-only Go launcher that execs the `container` CLI to boot a single-server
  k3s cluster (1 server + N agents) as per-node micro-VMs, then tears it down. The
  exec-the-CLI pattern is carried over verbatim from the Talos sibling.
- **Isn't:** an implementation of any upstream interface. The Talos sibling satisfies
  `siderolabs/talos/pkg/provision.Provisioner` (a directory-move-to-upstream contract);
  **k3s has no equivalent pluggable provisioner**, so this is a freestanding launcher with
  no compile-time interface assertion and no upstream merge path.

## Sibling relationship

This is the **k3s sibling of the Talos provisioner**. Same skeleton (`provider/apple` +
`cmd` driver + G-gate `VERIFICATION.md` + BVA recipe-lock tests), different guest. The
key recipe deltas from Talos:

| Aspect | Talos sibling (verified) | k3s (this draft, UNVERIFIED) |
|---|---|---|
| Image | `ghcr.io/siderolabs/talos` (machined/`vminitd`) | `rancher/k3s` (entrypoint runs `k3s` directly; **no systemd/openrc**) |
| Caps | `--cap-add ALL` | `--cap-add ALL` (same; k3s embedded containerd needs CAP_SYS_ADMIN) |
| tmpfs set | `/run /system /tmp /var /system/state` + CNI/k8s overlays, **not** `/opt` | **only `/run` and `/tmp`** — `/var` is NOT tmpfs (datastore), `/opt` still off tmpfs |
| Datastore | tmpfs `/var` (ephemeral) | host **bind-mount** `<statedir>/<node>/k3s:/var/lib/rancher/k3s` (survives stop/start + rm) |
| Datastore engine | etcd (Talos default) | **sqlite single-server** — embedded etcd (`--cluster-init`) deliberately OFF (no IP-bound membership → survives vmnet IP changes) |
| Networking | flannel via machine config | `--flannel-backend=host-gw` + `--tls-san <stable-name>` (L2 routes; cert stable across IP changes) |
| Memory unit | `MB` suffix (bare `M` rejected) | `MB` suffix (carried over) |
| Cluster secret | baked into machine config | shared `K3S_TOKEN` (crypto/rand if unset); agents also get `K3S_URL` |
| Labels | `talos.owned/cluster.name/type` | `k3s.owned/cluster.name/role` |
| `PLATFORM`/`TALOSSKU` env | set | **removed** (Talos-only) |
| IP forward | hidden inside machined | explicit `container exec <node> sysctl -w net.ipv4.ip_forward=1` |

## Lifecycle

`validate → ensureNetwork → run SERVER (sqlite + host-gw + tls-san + K3S_TOKEN) →
waitForIPv4 → exec sysctl ip_forward=1 → poll /readyz via in-node k3s kubectl →
run each AGENT (K3S_URL + K3S_TOKEN) → waitForIPv4 → exec sysctl → saveState`.

`destroy`: stop + rm each node → delete network → **remove the host state dir**. Removing
the bind-mounted datastore dir is what makes destroy *clean* vs. leaving it for a restart.

## Open risks (the G-gates — all UNVERIFIED)

- **G1 (highest risk):** does k3s's embedded containerd start under Apple `vminitd` with
  `--cap-add ALL`? The **kiac** spike chose `kindest/node` + `kubeadm` over k3s, possibly
  for exactly this reason. If G1 fails, the whole approach is moot.
- **G2:** does `--flannel-backend=host-gw` give working pod-to-pod across vmnet node-VMs?
  Fallback: is `br_netfilter` present for VXLAN?
- **G3:** does the `--volume` datastore bind-mount persist `/var/lib/rancher/k3s` across
  stop/start AND rm?
- **G4:** readiness — does `/readyz` answer (via in-node `k3s kubectl`) before the
  kubeconfig is host-fetchable? There is no systemd to query.
- **G5:** after a cold restart on a new DHCP IP, does persisted sqlite + `--tls-san` let
  the single-server cluster come back, and do agents need re-pointing?

See [`docs/VERIFICATION.md`](docs/VERIFICATION.md) for hypotheses and test commands.

## Usage (once G-gates pass — not before)

```sh
# create a 1-server + 1-agent cluster
go run ./cmd/aegis-k3s -name aegis -agents 1

# tear it down (removes nodes, network, and the host datastore dir)
go run ./cmd/aegis-k3s -name aegis -destroy
```

## Requirements

- macOS 26+, Apple Silicon
- [`container`](https://github.com/apple/container) >= 1.0.0
- Go 1.26+ (toolchain pin is itself UNVERIFIED — see `go.mod`)

## License

[MIT](LICENSE)
