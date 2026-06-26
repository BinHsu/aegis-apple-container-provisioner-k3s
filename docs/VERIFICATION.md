# Verification log — G-gate skeleton (SPIKE, NOTHING VERIFIED YET)

The proof that the work was **actually run and a human accepted it**, not just that
artifacts exist. This mirrors the Talos sibling's `docs/VERIFICATION.md` format: one
entry per gate — *what I ran · what I expected · what I saw · what surprised me · verdict.*

> **STATUS: every gate below is UNVERIFIED.** This is a spike draft written from the
> Talos sibling's verified recipe by analogy; not one k3s gate has been executed. The
> entries below are **hypotheses + test commands**, not observations. Per the sibling's
> rule: *don't pre-fill gates not yet run* — so each entry is explicitly marked
> `⛔ NOT RUN`, with only a hypothesis and the command that would test it. Replace each
> with a real first-person observation when the gate is actually executed.

**Highest-risk gate is G1.** A note that should temper expectations: the **kiac** spike
chose `kindest/node` + `kubeadm` over k3s for Apple-container Kubernetes — possibly for
exactly the G1 reason (k3s's embedded containerd under Apple's `vminitd`). Treat G1 as
the make-or-break gate; if it fails, the rest of this document is moot and the kiac
choice is vindicated.

---

## G1 — does k3s's embedded containerd start under Apple `vminitd` with `--cap-add ALL`? ⛔ NOT RUN
- **Hypothesis:** the Talos sibling proved `--cap-add ALL` is the `Privileged: true`
  equivalent and lets containerd/machined run under `vminitd`. k3s bundles its OWN
  containerd (not the host's), which needs CAP_SYS_ADMIN for mount/pivot_root/cgroup
  setup. Hypothesis: ALL caps are likewise sufficient for k3s's containerd. **This is
  the central unknown** — and the one kiac may have hit when it picked kubeadm over k3s.
- **Test:** `container run --detach --name g1 --cap-add ALL --tmpfs /run --tmpfs /tmp \
  --volume "$PWD/g1:/var/lib/rancher/k3s" rancher/k3s:<tag> server --flannel-backend=host-gw`
  then `container logs g1` — look for `containerd` coming up and `k3s` reaching
  "Running kube-apiserver", NOT a `failed to mount` / `operation not permitted` loop.
- **Verdict:** _unknown — highest risk. If this fails, stop; k3s is the wrong base._

## G2 — does `--flannel-backend=host-gw` give working pod-to-pod across vmnet node-VMs? ⛔ NOT RUN
- **Hypothesis:** vmnet places all node VMs on one L2 segment (Talos sibling G3 saw
  node-to-node reachability with zero config), so flannel `host-gw` L2 routes should work
  and avoid the default VXLAN backend's `br_netfilter` dependency.
- **Test:** bring up 1 server + 1 agent with `host-gw`; schedule a pod on each; exec one
  pod and `ping`/`curl` the other pod IP. **Fallback if host-gw fails:** check whether
  `br_netfilter` is present for VXLAN — `container run --rm rancher/k3s:<tag> sh -c \
  'grep br_netfilter /proc/filesystems; zcat /proc/config.gz 2>/dev/null | grep -i bridge_netfilter'`.
  (Talos sibling G1 found the Kata kernel ships features `=y`; UNVERIFIED for this image.)
- **Verdict:** _unknown._

## G3 — does the `--volume` datastore bind-mount persist /var/lib/rancher/k3s across stop/start AND rm? ⛔ NOT RUN
- **Hypothesis:** `container run` supports `-v/--volume` (the design assumes this). A host
  bind-mount at `/var/lib/rancher/k3s` should make the sqlite datastore survive both
  `container stop/start` and `container rm`, where tmpfs would not.
- **Test:** create cluster; `container exec server k3s kubectl create ns probe`;
  `container stop server && container start server` → assert ns `probe` still present;
  then `container rm -f server`, relaunch with the SAME `--volume`, assert ns still
  present. Confirms persistence is in the host dir, not the container.
- **Verdict:** _unknown._

## G4 — readiness probe: does /readyz answer before the kubeconfig is host-fetchable? ⛔ NOT RUN
- **Hypothesis:** rancher/k3s has NO systemd to query, so readiness is polled via
  `container exec server k3s kubectl get --raw /readyz` (returns `ok`). Hypothesis: the
  in-node `k3s kubectl` (using `/etc/rancher/k3s/k3s.yaml`) answers /readyz before the
  host can fetch a working kubeconfig — so exec is the right gate, not an HTTPS GET.
- **Test:** loop `container exec server k3s kubectl get --raw /readyz` from launch; record
  the time it first returns `ok`; separately confirm `k3s kubectl` is on PATH that early.
  Compare against an HTTPS `GET https://<ip>:6443/readyz -k` from the host.
- **Verdict:** _unknown._

## G5 — IP-change recovery: does persisted sqlite + `--tls-san` survive a cold restart on a new IP? ⛔ NOT RUN
- **Hypothesis:** the Talos sibling G3/G5 found vmnet DHCP moves IPs across cold restart
  with no static lever, and a Talos node came back BLANK (tmpfs state wiped). k3s should
  do better here: sqlite has no IP-bound membership (unlike embedded etcd) and `--tls-san`
  keeps the API cert valid across IP changes — so a single-server cluster *should* recover
  from a host/daemon restart even on a new IP. Open question: do agents need re-pointing at
  the new server IP (K3S_URL is baked at launch)?
- **Test:** healthy 1-server + 1-agent cluster → `container stop`/`start` all nodes (forces
  new DHCP IPs) → assert the server's API comes back (cert still valid via the stable SAN,
  datastore intact via the G3 bind-mount) → assert the agent rejoins, or document that it
  needs K3S_URL updated to the new server IP.
- **Verdict:** _unknown — this is the payoff gate for the sqlite+tls-san design choice._

---

Fill each first-person as the gate runs. Surprises and dead-ends are the most valuable
entries — they are what a reviewer reads as a human having actually done the work.
