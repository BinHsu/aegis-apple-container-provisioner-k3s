# Verification runbook — k3s on Apple container provisioner (v0.1.0)

I ran every gate below on hardware on 2026-06-27 across two sessions (macOS Apple Silicon,
16 GB RAM; Apple `container` CLI 1.0.0; `rancher/k3s:v1.32.5-k3s1`; single-control-plane
sqlite; `-dns-domain aegis`; server and agent VMs at 1536 MB). This is the release-gating
verification for v0.1.0. Session A (single server) proved readiness delivery, real workloads,
Traefik ingress, and lifecycle teardown. Session B (server + agent) proved cross-node
networking, datastore persistence, and FQDN endpoint stability across a DHCP IP change.
Every gate below carries the concrete evidence from those runs; verdicts are not predictions.

---

## Summary

| Gate | What it proves | Status |
|------|----------------|--------|
| G1 | k3s embedded containerd boots under `vminitd` with `--cap-add ALL` | ✅ PASS (2026-06-26, re-confirmed 2026-06-27) |
| G2 | `flannel host-gw` gives working pod-to-pod networking across nodes on vmnet | ✅ PASS (2026-06-27) |
| G3 | Named-volume sqlite datastore persists across cold container stop/start | ✅ PASS (2026-06-27) |
| G4 | Host-side TLS dial readiness + bind-mount kubeconfig delivery | ✅ PASS (2026-06-27) |
| G5 | FQDN endpoint survives cold restart when DHCP shifts the IP | ✅ PASS (2026-06-27) |
| G6 | Real workload + k3s built-in Traefik ingress reachable from the host | ✅ PASS (2026-06-27) |
| G7 | Full lifecycle teardown — no daemon hang, both `state.json` and label-sweep paths | ✅ PASS (2026-06-27) |
| G8 | Node membership — add/remove agents on a running cluster (v0.2.0) | ✅ PASS (2026-06-27) |
| G9 | HA external-datastore: 2 servers survive a cold-restart DHCP IP shift (v0.2.0 spike) | ✅ PASS (2026-06-27) |
| G10 | k3ac one-command HA: `-servers 2` provisions managed Postgres + 2 servers; survives cold restart; clean teardown (v0.2.0) | ✅ PASS (2026-06-27) |

---

## Execution order

Gate numbers are canonical cross-references (README, code comments, ADR-0001). Actual run
order follows the dependency graph, not the gate numbers:

- **Session A (single server, baseline):** G4 provision → G6 workload + ingress → G7 destroy (with `state.json`)
- **Session B (server + agent, network and persistence):** G2 pod-to-pod → G3 persistence → G5 FQDN cold-restart → G7 destroy (label-sweep, no `state.json`)

---

## Setup (from zero)

A forker reproduces every gate below with these commands. The provisioner is this repo's
launcher, `k3ac` (run from source with `go run ./cmd/k3ac`, or `go build -o k3ac ./cmd/k3ac`).
Prerequisites: Apple `container` >= 1.0.0, Go 1.26+, and `kubectl` on PATH.

```sh
# Register the FQDN search domain (once per macOS boot — the pf redirect is wiped on reboot):
sudo container system dns create aegis

# --- Session A: single server (verifies G4 delivery, G6 workload+ingress, G7 destroy path A)
go run ./cmd/k3ac -name k3x -agents 0 -server-memory 1536 -dns-domain aegis
export KUBECONFIG=_out/clusters/k3x/kubeconfig
kubectl get nodes                      # -> k3x-server-1 Ready
# ... run G4 / G6 checks ...
go run ./cmd/k3ac -name k3x -destroy    # G7 path A (state.json present)

# --- Session B: server + agent (verifies G2 networking, G3 persistence, G5 cold-restart)
go run ./cmd/k3ac -name k3x -agents 1 -server-memory 1536 -agent-memory 1536 -dns-domain aegis
export KUBECONFIG=_out/clusters/k3x/kubeconfig
kubectl get nodes                      # -> k3x-server-1 + k3x-agent-1 Ready
# ... run G2 / G3 / G5 checks ...
rm _out/clusters/k3x/state.json         # force the G7 path B label-sweep
go run ./cmd/k3ac -name k3x -destroy
```

Default node memory is 2048 MB; the runbook used 1536 MB (k3s steady-state is ~620 MB — see
Boundary & sizing). A separate forker clean-room run used the documented defaults
(`go run ./cmd/k3ac -name aegis -agents 1`, 2048 MB) end to end with identical results — the
cluster name is just a parameter (`aegis-server-1` vs `k3x-server-1`).

---

## G1 — k3s embedded containerd boots under Apple `vminitd` with `--cap-add ALL` ✅ PASS 2026-06-26 (re-confirmed 2026-06-27)

**What I ran.** Manual `container run` with `--cap-add ALL`, `--tmpfs /run`, `--tmpfs /tmp`,
a named volume for `/var/lib/rancher/k3s`, and
`rancher/k3s:v1.32.5-k3s1 server --flannel-backend=host-gw --tls-san <fqdn>`.

```sh
container run --detach --name k3sg1.aegis --cap-add ALL \
  --tmpfs /run --tmpfs /tmp \
  --volume aegis-k3sg1-k3s:/var/lib/rancher/k3s \
  --label k3s.cluster.name=aegis --label k3s.owned=true \
  --env K3S_TOKEN=<token> \
  rancher/k3s:v1.32.5-k3s1 server \
  --flannel-backend=host-gw \
  --tls-san k3sg1.aegis
container exec k3sg1.aegis k3s kubectl get nodes
```

**What I expected.** Embedded containerd starts, coredns pod comes up Running, and
`k3s kubectl get nodes` reports the node as Ready.

**What I saw.** Clean boot. Embedded containerd ran. Full control plane came up. Coredns pod
Running. Node name derived cleanly from the container FQDN with the DNS domain suffix dropped.
Cluster accessible from the host via kubeconfig.

**What surprised me.** `container exec` mangles entrypoint args for the rancher/k3s multi-call
binary: exec prepends the entrypoint again, so `container exec <fqdn> k3s kubectl ...` becomes
`k3s k3s kubectl ...` and the outer `k3s` does not recognize `kubectl` as its own subcommand.
Only `sysctl` (a standalone system binary, not a k3s symlink) passes cleanly through exec. This
observation fed directly into the G4 dead-end history. Use host-side kubeconfig for all kubectl
operations; do not rely on `container exec` for k3s subcommands.

**Verdict.** PASS. Image tag confirmed: `rancher/k3s:v1.32.5-k3s1`. Re-confirmed as the base
of every Session A and Session B run on 2026-06-27.

---

## G2 — `flannel host-gw` gives working pod-to-pod networking across nodes ✅ PASS 2026-06-27

**What I ran.** Session B: provisioned a server and an agent (both at 1536 MB). The agent
joined via `K3S_URL=https://k3x-server-1.aegis:6443` (FQDN, not an IP). After both nodes were
Ready I verified `ip_forward` on each VM, then launched two busybox pods — one constrained to
the server node, one to the agent node — and ran cross-node pings in both directions.

**What I expected.** vmnet places all node VMs on one L2 segment (consistent with what the
Talos sibling observed), so flannel `host-gw` L2 routes should work without `br_netfilter`.
The `sysctl -w net.ipv4.ip_forward=1` exec in the provisioner must have landed — a k3s spike
confirmed that cross-node flannel traffic silently drops without it. Expected: `ip_forward=1`
on both nodes; both pods reach each other at 0% loss.

**What I saw.**
- Provision exit 0. Both nodes Ready immediately: server at `.10`, agent at `.11`.
- `ip_forward=1` on both nodes — the provisioner's `sysctl` exec landed correctly.
- Pod `a` scheduled to the server at `10.42.0.7`; pod `b` scheduled to the agent at `10.42.2.2`.
- Cross-node ping `a → b`: 0% loss.
- Cross-node ping `b → a`: 0% loss.

**What surprised me.** Nothing unexpected. The L2-flat vmnet assumption held exactly as the
Talos sibling had seen it. `host-gw` required zero tuning; VXLAN is not needed and its
`br_netfilter` dependency is not a concern in this environment.

**Verdict.** PASS. Multi-node `flannel host-gw` networking is operational on Apple container
vmnet. A two-node cluster is network-functional from first provision.

---

## G3 — Named-volume sqlite datastore persists across a cold container stop/start ✅ PASS 2026-06-27

**What I ran.** Session B, after G2: created namespace `g8-marker` in the running cluster.
Stopped both the server and agent containers (`container stop` on each). Started both back up.
Checked whether `g8-marker` survived.

**What I expected.** The namespace survives the stop/start cycle — proving state lives in the
named volume (block-backed ext4, owned by guest root) and not the container's writable layer.

**What I saw.** Both containers stopped in 0 seconds (no hang). Both started cleanly. Namespace
`g8-marker` was `Active` after restart. Named-volume sqlite datastore intact.

**Why named volumes and not host-path bind-mounts — corrected reasoning.** An earlier draft of
this section stated that host-path bind-mounts (virtio-fs) were unsuitable because guest `chmod`
failed with `EPERM` — the same issue the Talos sibling hit on `/system/state`. **That reasoning
is wrong and should not be propagated.** A 2026-06-27 spike confirmed that a guest root process
can `write` and `chmod 600` / `chmod 0644` a file on a virtio-fs bind-mount, and the host reads
it back correctly. Plain sequential writes to virtio-fs work fine — as confirmed independently by
the kubeconfig delivery mechanism in G4 (k3s writes `k3s.yaml` to a virtio-fs bind-mount every
provision cycle).

The actual reason the sqlite datastore lives on a named volume is **sqlite WAL and advisory
file-locking semantics over virtio-fs**. sqlite WAL mode uses POSIX advisory locks and
shared-memory side-files (`-shm`, `-wal`). The interaction of those locking primitives with
virtio-fs's FUSE-based implementation is untested and potentially unreliable. A named volume is
block-backed ext4 with native POSIX semantics; that concern does not apply. See
[`docs/ADR/0001-kubeconfig-delivery-via-host-bind-mount.md`](ADR/0001-kubeconfig-delivery-via-host-bind-mount.md)
for the write-path analysis.

**What surprised me.** The 0-second stop time: both containers stopped instantly, confirming that
no vsock connections were held open. Under the old `container cp` design, a stop after a wedged
cp call hung for two-plus minutes. The clean stop here is a secondary indicator that the new
design left nothing dangling.

**Verdict.** PASS. Named-volume sqlite datastore survives a cold container stop/start. Cluster
state is durable across restarts.

---

## G4 — Readiness probe and kubeconfig delivery ✅ PASS 2026-06-27

### History: two dead ends before the current design

**Dead end 1 — exec-based `/readyz` (INVALIDATED 2026-06-26).** I tried
`container exec <id> k3s kubectl get --raw /readyz`. It fails with
`unknown command "kubectl" for "kubectl"`. k3s is a multi-call binary; `container exec`
prepends the entrypoint again, so the effective invocation is `k3s k3s kubectl ...`. The outer
`k3s` does not recognize `kubectl` as its own subcommand. The cluster came up healthy; `Create`
exited 1 every time.

Note: the G1 observation that "`container exec` works for k3s subcommands" was imprecise.
`container exec <id> sysctl ...` passes because `sysctl` is a standalone system binary, not a
k3s multi-call symlink.

**Dead end 2 — `container cp` polling (SUPERSEDED 2026-06-27).** I replaced the exec-based
probe with polling `container cp <server-fqdn>:/etc/rancher/k3s/k3s.yaml`. This worked in
initial testing but faulted on hardware during k3s cold boot: both `container cp` and
`container exec` ride the guest agent (`vminitd`) over vsock. During boot, the guest is
saturated extracting bundled images (coredns, traefik, metrics-server, local-path) — sustained
disk and vsock I/O. A `container cp` issued in that window **faults the vsock channel**, and
the cp process is killed externally. Verified at a 180 s per-attempt timeout with no parent
deadline; container uptime at failure was 182 s, meaning cp ran under 180 s and was killed by
the platform. Once the vsock faults, `container stop` and `container rm` also hang — two-plus
minutes observed. Recovery required force-killing the per-container helper:

```sh
pkill -9 -f "container-runtime-linux.<container-id>"
```

A separate bug ([apple/container #1738](https://github.com/apple/container/issues/1738)) caused
`container cp` with a relative host destination to fail with
`NSCocoaErrorDomain Code=642 "Read-only file system"` — the runtime resolves the path against
the container root. The fix (PR #1741, merged 2026-06-22) is not in the 1.0.0 release. The
bind-mount design sidesteps the bug entirely. See
[`docs/ADR/0001-kubeconfig-delivery-via-host-bind-mount.md`](ADR/0001-kubeconfig-delivery-via-host-bind-mount.md)
for the full root-cause analysis and the alternatives considered.

### Current mechanism (implemented and verified 2026-06-27)

**Readiness:** the provisioner dials `https://<server-IP>:6443` directly from the host. The
kube-apiserver answers TLS on that port; no guest agent involved.

**Kubeconfig delivery:** the server container bind-mounts the host cluster state directory at
`/mnt/k3s-out` inside the VM. k3s is launched with:

```
--write-kubeconfig /mnt/k3s-out/k3s.yaml --write-kubeconfig-mode 0644
```

k3s writes the kubeconfig straight to the host filesystem via virtio-fs. The provisioner polls
`os.Stat` for the file, rewrites the server URL from `https://127.0.0.1:6443` to the FQDN
endpoint, and writes the result to `<stateDir>/<cluster>/kubeconfig`. Zero `container cp` or
`container exec` in the critical path for readiness or kubeconfig delivery.

**What I ran.** Session A: provisioned a single server with the bind-mount design.

```sh
# after provision exits 0:
export KUBECONFIG=_out/clusters/k3x/kubeconfig
kubectl get nodes
# optional: verify apiserver TLS reachable from host
curl -ks https://k3x-server-1.aegis:6443/readyz
```

**What I expected.** Provision exits 0. Kubeconfig written with endpoint
`https://k3x-server-1.aegis:6443` (FQDN, not `127.0.0.1`). `kubectl get nodes` returns the
server as Ready. Zero `container cp` in the provision log.

**What I saw.** Provision exited 0. Kubeconfig written to `_out/clusters/k3x/kubeconfig` with
endpoint `https://k3x-server-1.aegis:6443`. `kubectl get nodes` returned
`k3x-server-1 Ready (control-plane,master, v1.32.5+k3s1)`. Destroy exited 0 in 1 second.
Zero `container cp` used.

**Verdict.** PASS. The bind-mount design eliminates the vsock fault window from the critical
provision path entirely. The force-kill recovery runbook is no longer needed during normal
Create/Destroy cycles.

---

## G5 — FQDN endpoint survives cold restart on a new DHCP IP ✅ PASS 2026-06-27

**What I ran.** Session B, after G3 persistence: cold-stopped and cold-started both the server
and agent containers. Checked the new IP assignments. Accessed the cluster via the unchanged
FQDN kubeconfig without editing or re-pointing it.

```sh
container stop k3x-server-1.aegis k3x-agent-1.aegis
container start k3x-server-1.aegis k3x-agent-1.aegis
container inspect k3x-server-1.aegis | grep ipv4Address    # confirm the IP changed
kubectl get nodes                                           # via unchanged FQDN kubeconfig
curl -ks https://k3x-server-1.aegis:6443/readyz
```

**What I expected.** vmnet DHCP issues a new IP after cold restart. Apple container DNS
auto-re-registers `k3x-server-1.aegis` to the new IP. The `--tls-san k3x-server-1.aegis` cert
covers the FQDN, so the TLS handshake stays valid regardless of the new IP. Agent nodes joined
via `K3S_URL=https://k3x-server-1.aegis:6443` — this URL is stable across IP changes — should
rejoin with no re-configuration. sqlite has no IP-bound membership, so the datastore is intact
on restart.

**What I saw.**
- Server IP shifted from `.10` to `.12`; agent IP shifted from `.11` to `.13`.
- Container DNS auto-re-registered `k3x-server-1.aegis → .12`.
- The kubeconfig (server URL `https://k3x-server-1.aegis:6443`, unchanged since provision)
  brought both nodes Ready with zero re-point — no manual kubeconfig edit, no provider
  re-provision.
- FQDN TLS: no cert mismatch. The SAN matched exactly what was baked in at create time.

**What surprised me — a cosmetic caveat to record honestly.** The agent kubelet's `INTERNAL-IP`
showed the old `.11` address after the DHCP shift. The node was `Ready` and the cluster was
fully operational — pod scheduling, flannel routing, and kube-apiserver reachability all worked
normally. The INTERNAL-IP is the kubelet's self-reported address from last registration; it does
not auto-update on DHCP change. This is consistent with the documented DHCP-shift limitation.
An operator who needs the displayed IP to match the live address should
`kubectl delete node k3x-agent-1` and let it re-register after the next restart. This is a
cosmetic display issue, not a functional one.

**Verdict.** PASS. FQDN endpoint + sqlite (no IP-bound membership) delivers zero-re-point
cold restart across DHCP IP changes. The agent INTERNAL-IP lag is cosmetic and documented.

---

## G6 — Real workload + k3s built-in Traefik ingress reachable from the host ✅ PASS 2026-06-27

**What I ran.** Session A, after G4 provision: first confirmed all k3s system pods were in
their expected states, then ran two sub-gates — in-cluster service DNS and Traefik host routing.

**System pods (baseline for a usable cluster):**

```sh
kubectl get pods -A
```

**Sub-gate A — in-cluster CoreDNS + kube-proxy + CNI:**

```sh
kubectl apply -f examples/nginx.yaml    # nginx Deployment + ClusterIP Service
kubectl wait --for=condition=available deployment/nginx --timeout=120s
kubectl run probe --image=busybox:1.36 --restart=Never \
  --command -- wget -qO- http://nginx.default.svc.cluster.local
kubectl logs probe                       # expect: nginx welcome HTML
```

**Sub-gate B — Traefik host-based ingress routing:**

```sh
kubectl apply -f examples/ingress.yaml   # Ingress: host demo.local -> nginx Service port 80
sleep 10                                  # let Traefik load the route
NODE_IP=$(kubectl get node k3x-server-1 \
  -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}')
curl -s -o /dev/null -w '%{http_code}\n' -H 'Host: demo.local' http://${NODE_IP}/   # expect 200
curl -s -o /dev/null -w '%{http_code}\n' -H 'Host: nope.local' http://${NODE_IP}/   # expect 404
```

**What I expected.** System pods all Running or Completed (helm-install jobs Completed is
expected). Sub-gate A: in-cluster wget returns nginx welcome HTML — proving CoreDNS, kube-proxy,
ClusterIP Service, and CNI are all wired up. Sub-gate B: `demo.local` returns HTTP 200 nginx;
`nope.local` returns HTTP 404 — real host routing, not a catch-all.

**What I saw.**
- **System pods:** coredns `1/1 Running`, traefik `1/1 Running`, metrics-server `1/1 Running`,
  local-path `1/1 Running`, svclb-traefik `2/2 Running`, helm-install-traefik `Completed`. A
  usable cluster, not just a Ready node.
- **Sub-gate A:** in-cluster busybox `wget http://nginx.default.svc.cluster.local` returned
  nginx welcome HTML. The probe pod status was `Completed` — expected for a one-shot pod; it
  exited 0 after the wget succeeded, which is correct behavior.
- **Sub-gate B:** `curl -H 'Host: demo.local'` → HTTP 200 nginx welcome page.
  `curl -H 'Host: nope.local'` → HTTP 404. Real host routing confirmed; Traefik is not a
  catch-all.

**What surprised me.** Nothing unexpected. k3s bundles a production-grade ingress controller
(Traefik) out of the box, and it was host-reachable immediately after first provision — no extra
NodePort configuration or `kubectl port-forward` needed. The system-pod readiness (all Running
within the normal provision window) confirms the k3s embedded image bundle is extracted cleanly
under the `vminitd` + `--cap-add ALL` environment.

**Verdict.** PASS. CoreDNS, kube-proxy, CNI, ClusterIP service DNS, and Traefik host-based
ingress are all functional from first provision. The cluster is immediately usable for real
workloads, not just an empty node.

---

## G7 — Full lifecycle teardown: no daemon hang, both destroy paths ✅ PASS 2026-06-27

**What I ran.** Two destroy paths across the two sessions, covering both the normal path and
the fallback.

**Path A — destroy with `state.json` present (Session A):** after G6, ran the provisioner
destroy command. The provisioner read `state.json`, found the server container FQDN, issued
`container stop` then `container rm`, and removed the named volume. Log line: "destroying
node k3x-server-1". Exited 0.

**Path B — destroy without `state.json` (Session B):** deleted `state.json` before running
destroy. The provisioner fell back to a label sweep (the `container` CLI has no native
`--label` filter on `ls`/`volume ls`, so the sweep lists `--format json` and matches
`k3s.cluster.name=k3x` client-side). It found both the server and agent containers plus
both named volumes, and removed all four. Log: `no state.json for cluster "k3x"; sweeping
by label k3s.cluster.name=k3x`. Exited 0.

**What I expected.** Both paths: exit 0, no lingering containers or volumes, no hang. The
1-second completion time is the headline contrast to the old cp-wedge design, where a wedged
vsock held `container stop` for two-plus minutes.

**What I saw.**
- **Path A:** `container stop` returned in 0 seconds; `container rm` clean; provisioner exited
  0 in approximately 1 second. "destroying node" log confirmed the `state.json` path executed.
- **Path B:** label sweep identified both containers (`k3x-server-1`, `k3x-agent-1`) and both
  named volumes. All four removed. Provisioner exited 0 in approximately 1 second. No leftovers
  confirmed by `container ls` and `container volume ls` post-destroy.

**What surprised me.** The 1-second destroys, even knowing the root cause, are striking. The
old `container cp` design held the vsock open during the k3s boot I/O window; any subsequent
`container stop` then had to wait for vsock recovery before the VM could receive the stop
signal. The new design never issues a `container cp` during provision, so the vsock is idle at
destroy time and `container stop` returns immediately. The fallback label-sweep path also proved
robust: dropping `state.json` is a realistic field scenario (corrupted state, manual cleanup),
and it exited as cleanly as the normal path.

**Verdict.** PASS. Both destroy paths — `state.json` and label-sweep fallback — exit cleanly
in approximately 1 second with no daemon hang and no leftover resources.

---

## G8 — node membership: add / remove on a running cluster ✅ PASS 2026-06-27 (v0.2.0)

**What I ran.** Provisioned `k3x` with one server + one agent (1536 MB each). Then exercised the
membership operations against the running cluster:

```sh
go run ./cmd/k3ac -name k3x -add-agents 1 -agent-memory 1536 -dns-domain aegis      # add k3x-agent-2
export KUBECONFIG=_out/clusters/k3x/kubeconfig
kubectl wait --for=condition=Ready node/k3x-agent-2 --timeout=90s
go run ./cmd/k3ac -name k3x -remove-node k3x-agent-2 -dns-domain aegis              # drain + tear down
go run ./cmd/k3ac -name k3x -remove-node k3x-server-1 -dns-domain aegis             # server guard (must refuse)
```

**What I expected.** `add-agents` launches the next-indexed agent, which auto-joins via the saved
`K3S_URL` FQDN + `K3S_TOKEN` (no separate join step), reusing the image recorded in `state.json`.
`remove-node` drains the node from Kubernetes, tears down its container + named volume, and drops it
from `state.json`. Removing the server is refused (that is `-destroy`).

**What I saw.**
- **add-agents PASS:** `k3x-agent-2` launched at `192.168.64.18`; joined via `https://k3x-server-1.aegis:6443`; `kubectl wait` reported it Ready; `kubectl get nodes` showed all three Ready (server + agent-1 + agent-2). `state.json` gained the node; its named volume `k3x-k3x-agent-2-k3s` was created; the stored `image` (`rancher/k3s:v1.32.5-k3s1`) was reused.
- **remove-node PASS:** `kubectl delete node` drained it first; container stopped + removed and the named volume deleted in ~1 s; `kubectl get nodes` back to two; no leftover container or volume; `state.json` nodes = `[k3x-server-1, k3x-agent-1]`.
- **server guard PASS:** `-remove-node k3x-server-1` exited non-zero with `node "k3x-server-1" is the cluster server; removing it would destroy the cluster — use -destroy instead`. No teardown attempted.

**What surprised me.** Nothing. `add-agents` reuses Create's exact building blocks (named-volume
stale-state guard, `launchNode`, `enableIPForward`, `assertDistinctIPs`) and `remove-node` reuses
Destroy's per-node teardown (`stop`/`remove`/`volumeDelete`), so the membership paths cannot drift
from create/destroy. The agent index is `max(existing)+1` (gaps from a removed agent are not
backfilled), so a re-added agent never reuses a name whose datastore volume might still linger.

**Verdict.** PASS. Agents can be added to and removed from a running cluster without recreate; the
server is protected; teardown leaves no orphans.

---

## G9 — HA external-datastore: 2 servers survive a cold-restart DHCP IP shift ✅ PASS 2026-06-27 (v0.2.0 spike)

This is the HA design spike behind ADR-0002. It is a **manual** experiment (hand-run
`container run`, not yet a k3ac code path), recorded here for traceability. It proves the HA
direction empirically before the k3ac multi-server implementation lands.

**What I ran.** A PostgreSQL micro-VM `k3h-db.aegis` (named volume, `PGDATA` at a subdirectory)
plus two k3s servers `k3h-srv-1.aegis` / `k3h-srv-2.aegis`, each launched with
`--datastore-endpoint=postgres://kine:…@k3h-db.aegis:5432/kine`, `--flannel-backend=host-gw`,
a shared `K3S_TOKEN`, and **no** `--cluster-init`. Confirmed both `Ready`/`control-plane,master`
against the shared datastore, then seeded a marker ConfigMap (`ha-spike-marker`). Cold-stopped
all three and restarted each individually (`container start` takes one ID), forcing the DHCP IP
shift, then re-checked the control plane and the marker.

**What I expected.** Unlike embedded etcd (ruled out in the prior session — etcd peer membership
is IP-bound and cannot reform quorum after the IP shift, apiserver `ServiceUnavailable` for the
full 180 s), an external datastore reached by FQDN has no IP-bound peer membership, so the control
plane should reconnect by name and recover.

**What I saw.**
- **IP shift confirmed:** DB `.28→.31`, srv-1 `.29→.32`, srv-2 `.30→.33` — every node moved, the
  exact condition that killed embedded etcd.
- **Control plane recovered:** apiserver `/readyz` OK ~12 s after start; both nodes `Ready`/
  `control-plane`. The apiserver answered on **both** server FQDN endpoints (`k3h-srv-1.aegis` and
  `k3h-srv-2.aegis`) — true HA, either server serves.
- **Datastore survived:** servers reconnected to `k3h-db.aegis` by name (A-record re-registered to
  `.31`); the `ha-spike-marker` ConfigMap was intact with identical data.
- **Workload plane reconverged:** each node's `InternalIP` and flannel `public-ip` annotation
  re-registered to the new `.32`/`.33` (host-gw cross-node routing recovers too). A `kubectl get
  nodes -o wide` issued in the first seconds after readiness briefly showed the old IPs — kubelet
  posts the new IP a few seconds later; the proper jsonpath read confirmed `.32`/`.33`.

**What surprised me.** The workload plane recovered without intervention — I expected to have to
re-wire node IPs by hand. kubelet re-registers the new `InternalIP` and flannel updates its
`public-ip` annotation on its own, so the host-gw routes reconverge. The only transient is the
few-second window where the node object still carries the pre-restart IP.

**Verdict.** PASS. Two stateless k3s servers on an external Postgres datastore at a stable FQDN
survive the whole-cluster cold-restart DHCP IP shift that embedded etcd could not. This validates
ADR-0002 option (b). The datastore itself is a single Postgres VM (not HA) — control plane is HA,
datastore is not, until the datastore is separately replicated. Teardown removed all three
containers and their named volumes with no daemon hang.

---

## G10 — k3ac one-command HA: managed datastore + multi-server, end to end ✅ PASS 2026-06-27 (v0.2.0)

G9 proved the HA topology by hand. G10 proves k3ac's CODE drives it: the managed-datastore
provisioning and the multi-server create path, plus cold-restart survival and clean teardown of
the datastore node.

**What I ran.** `k3ac -name hav -servers 2 -agents 1 -server-memory 1536 -agent-memory 1536`
(no `-datastore-endpoint`, so k3ac provisions the datastore itself). Then `kubectl get nodes`,
seeded a marker ConfigMap, cold-restarted all four containers (DB started first, one at a time),
re-checked, and finally `k3ac -name hav -destroy`.

**What I expected.** One command brings up `hav-db` (Postgres) + two servers wired to it via
`--datastore-endpoint=postgres://…@hav-db.aegis:5432/kine` + one agent. The control plane survives
the cold-restart IP shift (G9 property), and destroy reclaims the datastore node and its volume.

**What I saw.**
- **Bring-up PASS:** four VMs — `hav-db` (RoleDatastore), `hav-server-1`, `hav-server-2`,
  `hav-agent-1`. `kubectl get nodes`: both servers `Ready`/`control-plane,master`, agent `Ready`.
  `state.json` recorded `datastoreEndpoint` (password and all) and the four nodes with roles
  `[datastore, server, server, agent]`.
- **Cold-restart survival PASS:** every IP shifted (`.34/.35/.36/.37` → `.38/.39/.40/.41`).
  apiserver `/readyz` answered within 3 s on the FQDN endpoint (re-resolved to the new IP); all
  nodes `Ready` with internal IPs updated to the new addresses; the marker ConfigMap survived;
  querying via the `hav-server-2.aegis` endpoint directly also served (HA — either server answers).
- **Teardown PASS:** `-destroy` removed all four nodes (the datastore included) in 3.5 s with no
  daemon hang; no leftover containers or volumes; the state dir was removed. The datastore volume
  (`hav-db-pg`, off the k3s `nodeVolumeName` scheme) was reclaimed by the role-aware delete plus
  the label sweep.

**What surprised me.** Nothing new beyond G9 — the code path behaved exactly as the manual spike
predicted. The managed-datastore stale-state guard, the PGDATA-subdir handling, and the role-aware
teardown all worked first try on hardware because they mirror the create/destroy building blocks
the single-server path already proved.

**Verdict.** PASS. `k3ac -servers N` (N≥2) stands up a full HA control plane on a managed external
datastore with one command, survives the cold-restart IP shift, and tears down cleanly. The
datastore is a single Postgres VM (not itself HA) — control plane HA, datastore not, per ADR-0002.

---

## Boundary and sizing checks

These are not hardware re-runs. They are recorded here for completeness and traceability to
the test suite.

**Server count BVA (boundary value analysis).** The provider rejects `servers=0` (no control
plane) and `servers≥2` (HA sqlite is not a supported configuration in v0.1.0) at validation
time; `servers=1` is the only accepted value. Covered by
`TestValidateClusterConfig_ServerCountBoundaries` in `provider_test.go` at boundary values
`{0, 1, 2}` (B−1, B, B+1 where B=1). The unit layer is the appropriate layer for this
input-domain check; it was not re-run on hardware.

**Memory sizing.** Measured k3s real in-VM usage at approximately 620 MB at steady state (full
control plane + all system pods Running). Ran successfully at `-server-memory 1536`. The
default is 2048 MB. Host memory was an amplifier of the now-removed cp-wedge fault window:
tighter host memory extended the k3s boot I/O window, widening the interval during which a
`container cp` could fault the vsock. The bind-mount design removes that dependency entirely —
1536 MB ran a full usable cluster with no resource-related issues in either session.
