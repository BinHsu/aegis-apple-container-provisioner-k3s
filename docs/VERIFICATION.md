# Verification runbook — G-gates

An ordered, on-hardware runbook. Run the gates **top to bottom**: each is the precondition
for the next. The order is execution order, not gate-number order — the canonical numbers
(G1, G2, G3, G4, G5) are kept stable so they cross-reference the README and the Talos
sibling, but you run them as: **G1 → G4 → G2 → G3 → G5**.

Each gate states: *hypothesis · exact commands · pass/fail criteria · what a fail means.*
Fill in a first-person observation (what you ran · what you saw · what surprised you ·
verdict) when you actually execute it.

---

## G1 — does k3s's embedded containerd start under Apple `vminitd` with `--cap-add ALL`? ✅ VERIFIED 2026-06-26

**PASSED.** `rancher/k3s:v1.32.5-k3s1` booted under Apple `container` 1.0.0. Embedded
containerd ran, full control plane came up, coredns pod running. Clean Kubernetes node name
`k3sg1` (container DNS domain suffix dropped). Cluster accessible from the host via
kubeconfig (server URL rewritten to the node IP). **`container exec` mangles entrypoint args**
for the rancher/k3s image (the entrypoint runs `k3s` directly and exec prepends it again) —
do not rely on `container exec` for arbitrary shell commands; use only for k3s subcommands
that pass cleanly (sysctl, `k3s kubectl`). Use host-side kubeconfig access for everything
else.

**Recipe used (now the baseline for G2/G3/G5):**

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

**Image tag confirmed:** `rancher/k3s:v1.32.5-k3s1`. Use this exact tag; remove the
`UNVERIFIED ASSUMPTION` comment from any docs that still carry it.

---

## G4 — readiness probe: exec-based `/readyz` approach INVALIDATED; replaced by `container cp` ⛔ INVALIDATED 2026-06-26

**FINDING (2026-06-26, G1 hardware run):** `container exec <id> k3s kubectl get --raw
/readyz` fails with `unknown command "kubectl" for "kubectl"`.

**Root cause:** k3s is a multi-call binary — `kubectl` and `crictl` are symlinks to the
same `k3s` binary. The container entrypoint runs `k3s` directly; `container exec` prepends
the entrypoint again, so the effective invocation becomes `k3s k3s kubectl ...`. The outer
`k3s` does not recognize `kubectl` as a subcommand of itself when invoked that way. This
means the exec-based readiness probe ALWAYS errors — the cluster comes up healthy but
Create exits 1, every time.

**Note:** the G1 observation that "`container exec` works for k3s subcommands" was incorrect.
The earlier sysctl exec (`container exec <id> sysctl -w net.ipv4.ip_forward=1`) passes
because `sysctl` is a separate system binary — not a k3s multi-call symlink.

**Replacement (implemented 2026-06-26; see `create.go` `waitForReady`):**

Poll `container cp <server-fqdn>:/etc/rancher/k3s/k3s.yaml <kubeconfigPath>` with 5-second
backoff until it succeeds (overall timeout: 120s). k3s writes `k3s.yaml` only once the API
server is fully initialized (CA issued, control-plane healthy), so a successful `cp` is a
reliable "server is up" signal — equivalent to `/readyz` returning `ok`. As a bonus it
simultaneously delivers the operator's kubeconfig; the provisioner rewrites the server URL
from `https://127.0.0.1:6443` to the FQDN endpoint (or current IP in IP-only mode) and
writes the result to `<stateDir>/<cluster>/kubeconfig`. No manual steps needed.

- **Commands (verification with replacement approach):**
  ```sh
  # confirm the provisioner wrote a working kubeconfig:
  export KUBECONFIG=_out/clusters/aegis/kubeconfig
  kubectl get nodes
  # optional secondary check via curl:
  curl -ks https://aegis-server-1.aegis:6443/readyz
  ```
- **Pass:** `kubectl get nodes` returns the server node as `Ready`; no manual kubeconfig
  fetch or server-URL rewrite was needed.
- **Fail:** kubeconfig is absent or still contains `127.0.0.1` — the rewrite step failed.

---

## G2 — does `--flannel-backend=host-gw` give working pod-to-pod across vmnet node-VMs? ⛔ NOT RUN

Run third: needs a server (G1) plus at least one agent, so it follows readiness.

- **Hypothesis:** vmnet places all node VMs on one L2 segment (Talos sibling saw
  node-to-node reachability with zero config), so flannel `host-gw` L2 routes should work and
  avoid the default VXLAN backend's `br_netfilter` kernel dependency. Create also execs
  `sysctl -w net.ipv4.ip_forward=1` in every node (the kiac spike proved k3s networking is
  broken without it) — confirm that write actually sticks.
- **Commands:**
  ```sh
  container exec <server-fqdn> sysctl net.ipv4.ip_forward     # expect = 1
  # schedule a pod on each node, then test pod-to-pod across nodes:
  container exec <server-fqdn> k3s kubectl run a --image=busybox --restart=Never -- sleep 3600
  container exec <server-fqdn> k3s kubectl run b --image=busybox --restart=Never -- sleep 3600
  container exec <server-fqdn> k3s kubectl get pods -o wide    # confirm a/b on different nodes
  container exec <server-fqdn> k3s kubectl exec a -- ping -c3 <pod-b-ip>
  ```
- **Pass:** `ip_forward` reads `1`; a pod on the server reaches a pod on the agent.
- **Fail:** cross-node pod traffic drops.
- **On fail:** check the `br_netfilter` fallback; switch to default VXLAN backend and re-test.

---

## G3 — does the named volume datastore persist `/var/lib/rancher/k3s` across stop/start AND rm? ⛔ NOT RUN

**Updated:** the datastore is now an Apple `container` NAMED VOLUME (not a host-path
bind-mount). Named volumes are block-backed ext4 owned by the guest root, so guest chmod
works. Host-path bind-mounts (virtio-fs) rejected guest chmod — the same EPERM issue the
Talos sibling hit on `/system/state`.

- **Hypothesis:** `container volume create` creates a named volume that persists across
  `container stop/start` and `container rm`. The sqlite datastore and all server state survive,
  so a cold restart brings the cluster back intact.
- **Commands:**
  ```sh
  # with a running server (from G2):
  container exec <server-fqdn> k3s kubectl create ns probe
  container stop <server-fqdn> && container start <server-fqdn>
  container exec <server-fqdn> k3s kubectl get ns probe          # expect: still present
  container rm -f <server-fqdn>
  # relaunch with the SAME named volume (provisioner does this automatically on restart):
  container run --detach --name <server-fqdn> --cap-add ALL \
    --tmpfs /run --tmpfs /tmp \
    --volume <cluster>-<server>-k3s:/var/lib/rancher/k3s \
    --env K3S_TOKEN=<token> rancher/k3s:v1.32.5-k3s1 server \
    --flannel-backend=host-gw --tls-san <server-fqdn>
  container exec <server-fqdn> k3s kubectl get ns probe          # expect: still present
  ```
- **Pass:** namespace `probe` survives both stop/start and a full `rm` + relaunch onto the
  same named volume — proving state lives in the named volume, not the container layer.
- **Fail:** `probe` is gone, meaning the datastore is not persisted in the named volume.
- **On fail:** check whether `container volume create` + `--volume <name>:` persists data
  across rm (named-volume semantics should guarantee this). If named volumes do NOT persist,
  the whole persistence design needs rethinking.

---

## G5 — FQDN endpoint + named volume: does the single-server cluster survive a cold restart on a new IP? ⛔ NOT RUN

The combined payoff gate for the sqlite + FQDN design. Run last; needs G3.

- **Hypothesis:** with `--tls-san <server-fqdn>` the API server cert covers the FQDN. After
  a cold restart the vmnet DHCP IP changes, but Apple's container DNS (`container system dns
  create aegis`) re-registers the FQDN to the new IP — so host-side FQDN access stays valid.
  sqlite has no IP-bound membership (unlike embedded etcd), so the datastore is intact.
  Agent nodes join via `K3S_URL=https://<server-fqdn>:6443` — this URL stays stable too.
- **Commands:**
  ```sh
  # healthy 1-server + 1-agent cluster, then cold restart:
  container stop <server-fqdn> <agent-fqdn>
  container start <server-fqdn> <agent-fqdn>
  container inspect <server-fqdn> | grep ipv4Address           # confirm the IP changed
  # host-side FQDN access (stable because DNS re-registers):
  curl -ks https://<server-fqdn>:6443/readyz                   # expect: ok
  container exec <server-fqdn> k3s kubectl get nodes            # expect: both Ready
  KUBECONFIG=./kubeconfig kubectl get nodes                     # expect: works via FQDN URL
  ```
- **Pass:** API comes back via FQDN (cert valid via SAN, IP changed); agent rejoins; host
  kubeconfig (pointing at the FQDN) continues working.
- **Fail scenarios:**
  - API cert SAN mismatch — the FQDN was not included in `--tls-san`. Fix: confirm the
    FQDN matches exactly what was baked into `--tls-san` at create time.
  - DNS not updated — Apple container DNS did not re-register after restart. Check: was
    `sudo container system dns create aegis` run after the last macOS reboot?
  - Agent stuck NotReady — re-pointing `K3S_URL` to the FQDN is a no-op since the FQDN
    is already the agent's join endpoint; if it fails, check DHCP reconvergence timing.

---

Fill each gate first-person as it runs. Surprises and dead-ends are the most valuable
entries — they are what a reviewer reads as evidence a human actually did the work.
