# Verification runbook — G-gates (SPIKE, NOTHING VERIFIED YET)

An ordered, on-hardware runbook. Run the gates **top to bottom**: each is the precondition
for the next. The order is execution order, not gate-number order — the canonical numbers
(G1, G2, G3, G4, G5) are kept stable so they cross-reference the README and the Talos
sibling, but you run them as: **G1 → G4 → G2 → G3 → G5**.

Each gate states: *hypothesis · exact commands · pass/fail criteria · what a fail means.*
Fill in a first-person observation (what you ran · what you saw · what surprised you ·
verdict) when you actually execute it.

> **STATUS: every gate below is UNVERIFIED.** This is a spike draft written from the Talos
> sibling's verified recipe by analogy; not one k3s gate has been executed. The entries are
> **hypotheses + test commands**, not observations. Per the sibling's rule: *don't pre-fill
> gates not yet run* — each is marked `⛔ NOT RUN`. Replace with a real observation when run.

> **Serialized after Talos.** Verification runs on a single MacBook Air; Apple `container`
> boots one VM front at a time, so two VM-heavy spikes cannot run concurrently. The Talos
> sibling's on-hardware pass runs first; this k3s runbook is queued behind it. Where a Talos
> finding de-risks a k3s gate it is cited inline, but a Talos pass is **not** a k3s pass —
> re-confirm on the k3s image.

---

## G1 — does k3s's embedded containerd start under Apple `vminitd` with `--cap-add ALL`? ⛔ NOT RUN

**BLOCKING — run this first. If G1 fails, STOP: do not run G2–G5, the whole k3s approach is
dead, and the kiac choice (kubeadm + `kindest/node` over k3s) is vindicated.**

- **Hypothesis:** the Talos sibling proved `--cap-add ALL` is the `Privileged: true`
  equivalent and lets containerd/machined run under `vminitd`. k3s bundles its OWN
  containerd (not the host's), which needs CAP_SYS_ADMIN for mount/pivot_root/cgroup setup.
  Hypothesis: ALL caps are likewise sufficient for k3s's containerd. **This is the central
  unknown** — and the one kiac may have hit when it picked kubeadm over k3s.
- **Commands:**
  ```sh
  mkdir -p "$PWD/g1"
  container run --detach --name g1 --cap-add ALL \
    --tmpfs /run --tmpfs /tmp \
    --volume "$PWD/g1:/var/lib/rancher/k3s" \
    rancher/k3s:<tag> server --flannel-backend=host-gw --tls-san aegis-k3s.local
  container logs g1                      # watch boot
  container exec g1 k3s kubectl get nodes
  container exec g1 k3s crictl info      # confirms the embedded containerd is up
  ```
- **Pass:** `container logs g1` shows containerd coming up and k3s reaching "Running
  kube-apiserver" (no `failed to mount` / `operation not permitted` loop); `k3s crictl info`
  returns runtime info; `k3s kubectl get nodes` shows the node `Ready`.
- **Fail:** a mount/permission loop in the logs, `crictl` cannot reach containerd, or the
  node never reaches `Ready`.
- **On fail:** the approach is dead. k3s's embedded containerd cannot run under Apple
  `vminitd` even with full caps. Stop here; do not run G2–G5.

## G4 — readiness probe: does `/readyz` answer (via in-node `k3s kubectl`) before the kubeconfig is host-fetchable? ⛔ NOT RUN

Run second: bring-up orchestration (Create) blocks on this probe before it joins agents, so
confirm the probe is correct before testing multi-node behavior.

- **Hypothesis:** rancher/k3s has NO systemd to query (the entrypoint runs `k3s` directly),
  so readiness is polled from inside the node via `k3s kubectl get --raw /readyz` (returns
  `ok`). The in-node `k3s kubectl` uses `/etc/rancher/k3s/k3s.yaml`, which already trusts the
  local CA, so it answers `/readyz` before the host can fetch a working kubeconfig — making
  exec the right gate, not an HTTPS GET from the host.
- **Commands:**
  ```sh
  # from launch, time how long until /readyz first returns ok
  until container exec g1 k3s kubectl get --raw /readyz 2>/dev/null | grep -qx ok; do
    sleep 2; done; echo "ready"
  container exec g1 sh -c 'command -v k3s'        # confirm k3s is on PATH this early
  # compare: an HTTPS GET from the host (expected to need -k / fail early)
  curl -ks "https://<node-ip>:6443/readyz"
  ```
- **Pass:** the in-node `k3s kubectl get --raw /readyz` returns exactly `ok`; `k3s` is on
  PATH from first boot; the probe goes green within the `readyTimeout` (120s) the code uses.
- **Fail:** `/readyz` never returns `ok`, `k3s kubectl` is not on PATH early, or readiness
  takes longer than `readyTimeout`.
- **On fail:** fix the probe (different endpoint, longer timeout) before anything else —
  Create cannot join agents without a correct readiness gate. Not a kill-switch for the
  approach, but a kill-switch for the current orchestration.

## G2 — does `--flannel-backend=host-gw` give working pod-to-pod across vmnet node-VMs? ⛔ NOT RUN

Run third: needs a server (G1) plus at least one agent, so it follows readiness.

- **Hypothesis:** vmnet places all node VMs on one L2 segment (Talos sibling saw
  node-to-node reachability with zero config), so flannel `host-gw` L2 routes should work and
  avoid the default VXLAN backend's `br_netfilter` kernel dependency. Create also execs
  `sysctl -w net.ipv4.ip_forward=1` in every node (the kiac spike proved k3s networking is
  broken without it) — confirm that write actually sticks.
- **Commands:**
  ```sh
  container exec <server> sysctl net.ipv4.ip_forward     # expect = 1
  # schedule a pod on each node, then test pod-to-pod across nodes:
  container exec <server> k3s kubectl run a --image=busybox --restart=Never -- sleep 3600
  container exec <server> k3s kubectl run b --image=busybox --restart=Never -- sleep 3600
  container exec <server> k3s kubectl get pods -o wide    # confirm a/b on different nodes
  container exec <server> k3s kubectl exec a -- ping -c3 <pod-b-ip>
  # fallback probe if host-gw fails — is br_netfilter present for VXLAN?
  container run --rm rancher/k3s:<tag> sh -c \
    'grep br_netfilter /proc/filesystems; zcat /proc/config.gz 2>/dev/null | grep -i bridge_netfilter'
  ```
- **Pass:** `ip_forward` reads `1`; a pod on the server reaches a pod on the agent across
  nodes.
- **Fail:** cross-node pod traffic drops.
- **On fail:** check the `br_netfilter` fallback probe. If `br_netfilter` is `=y`, switch the
  server flag from `--flannel-backend=host-gw` to the default VXLAN backend and re-test. If
  neither backend works, pod networking across vmnet node-VMs is unsolved for k3s.

## G3 — does the `--volume` datastore bind-mount persist `/var/lib/rancher/k3s` across stop/start AND rm? ⛔ NOT RUN

- **Hypothesis:** `container run` supports `-v/--volume`. A host bind-mount at
  `/var/lib/rancher/k3s` makes the sqlite datastore survive both `container stop/start` and
  `container rm`, where tmpfs would not. The Talos sibling's G5a/G5b will partly de-risk
  host-dir persistence (it bind-mounts `/var` + `/system/state` on virtio-fs and tests
  cold-restart survival of etcd) — but k3s uses **sqlite, not etcd**, so the failure modes
  differ (no raft, single-writer file lock, different fsync pattern). Re-confirm on k3s.
- **Commands:**
  ```sh
  container exec <server> k3s kubectl create ns probe
  container stop <server> && container start <server>
  container exec <server> k3s kubectl get ns probe          # expect: still present
  container rm -f <server>
  # relaunch with the SAME --volume host dir:
  container run --detach --name <server> --cap-add ALL --tmpfs /run --tmpfs /tmp \
    --volume "<stateDir>/<cluster>/<server>/k3s:/var/lib/rancher/k3s" \
    --env K3S_TOKEN=<token> rancher/k3s:<tag> server --flannel-backend=host-gw --tls-san aegis-k3s.local
  container exec <server> k3s kubectl get ns probe          # expect: still present
  ```
- **Pass:** namespace `probe` survives both stop/start and a full `rm` + relaunch onto the
  same host dir — proving state lives in the host dir, not the container.
- **Fail:** `probe` is gone after stop/start (datastore was not on the bind-mount) or after
  `rm` + relaunch (host dir not persisted / not re-attached).
- **On fail:** the persistence design is broken; the datastore is not actually on the host
  bind-mount. Re-check the `--volume` flag form and that `/var` is not being shadowed by a
  tmpfs. Without this, G5 cannot pass.

## G5 — IP-change recovery: does persisted sqlite + `--tls-san` survive a cold restart on a new IP? ⛔ NOT RUN

The payoff gate for the sqlite + `--tls-san` design choice. Run last; needs G3.

- **Hypothesis:** the Talos sibling found vmnet DHCP moves IPs across cold restart with no
  static lever, and a Talos node came back BLANK (tmpfs state wiped). k3s should do better:
  sqlite has no IP-bound membership (unlike embedded etcd, which encodes peer/client URLs),
  and `--tls-san aegis-k3s.local` pins a stable name into the API server cert SANs so the
  cert stays valid when the DHCP IP changes. So a single-server cluster *should* recover from
  a cold restart even on a new IP. Open question: do agents need re-pointing? `K3S_URL` is
  baked at agent launch, so a new server IP may strand them.
- **Commands:**
  ```sh
  # healthy 1-server + 1-agent cluster, then force new DHCP IPs:
  container stop <server> <agent>
  container start <server> <agent>
  container inspect <server> | grep ipv4Address           # confirm the IP actually changed
  container exec <server> k3s kubectl get --raw /readyz    # expect: ok (cert still valid via SAN)
  container exec <server> k3s kubectl get nodes            # is the agent Ready, or NotReady?
  ```
- **Pass:** the server API comes back (cert valid via the stable SAN, datastore intact via
  the G3 bind-mount); EITHER the agent rejoins on its own, OR you document the exact step to
  re-point it (`K3S_URL` updated to the new server IP).
- **Fail:** the server API does not come back (cert SAN mismatch or lost datastore), or the
  agent cannot be recovered at all.
- **On fail:** if the server fails, restart survival needs more than sqlite + `--tls-san`
  (likely an upstream static-IP / DHCP-reservation in `container`, the same gap the Talos
  sibling documents). If only the agent fails, document the re-point procedure — that is an
  acceptable, honest limitation for a single-server spike.

---

Fill each gate first-person as it runs. Surprises and dead-ends are the most valuable
entries — they are what a reviewer reads as evidence a human actually did the work.
