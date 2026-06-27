# ADR-0001: Kubeconfig delivery via host bind-mount

**Status:** Accepted, 2026-06-27

---

## Context

### The old mechanism

The provisioner used `container cp <server>:/etc/rancher/k3s/k3s.yaml <host-path>` for
two jobs at once: **readiness signal** (polled every 5 s, each attempt SIGKILLed at a
timeout) and **kubeconfig delivery**.

### Root cause of the failure

`container cp` and `container exec` both ride the guest agent (`vminitd`) over a vsock
channel. During a k3s cold boot the guest is saturated extracting bundled images
(coredns, traefik, metrics-server, local-path) — sustained disk and vsock I/O. A
`container cp` issued in that window **faults the vsock channel**, and the cp process is
killed externally by the system.

This was verified on hardware: with our own per-attempt timeout raised to 180 s and
`main.go` holding `context.Background()` (no parent deadline), the cp still died well
before 180 s. Container uptime at failure was 182 s, meaning cp ran fewer than 180 s and
was killed by the platform, not by us.

Once the vsock faults, the cascade is immediate: `container stop`, `container rm`, and
`container system stop` all hang. Observed: a destroy hung over two minutes. Recovery
without a reboot requires force-killing the per-container helper:

```sh
pkill -9 -f "container-runtime-linux.<container-id>"
```

After that, `container ls` and `container rm` work again.

Our old polling loop (SIGKILL every 5 s) amplified the fault pattern but did not cause
it. The fundamental issue is the platform: the vsock channel has no graceful degradation
under sustained I/O backpressure.

**Memory pressure is an amplifier, not the root cause.** The failure reproduced on a host
with 80 % free RAM and a 1536 MB server VM — the I/O window during bundle extraction is
intrinsic to the k3s boot sequence regardless of host memory headroom.

### The separate relative-path bug

Independently, `container cp` fails with `NSCocoaErrorDomain Code=642 "Read-only file
system"` when the host destination is a relative path. The runtime resolves the path
against the container root (e.g., `/_out/...`), which is read-only. This is
[apple/container #1738](https://github.com/apple/container/issues/1738), fixed by
[PR #1741](https://github.com/apple/container/pull/1741) (merged 2026-06-22) but **not
included in the 1.0.0 release** (released 2026-06-09). An absolute host path is the
interim workaround; the bind-mount design sidesteps the bug entirely.

### Why the Talos sibling never hit this

The Talos sibling uses zero `container cp` or `container exec` in its critical path.
Talos is network-API-native: `talosctl` (gRPC over the VM IP) handles apply-config,
bootstrap, health checks, and kubeconfig retrieval — it never touches the guest agent.
k3s's admin kubeconfig is a client-cert file inside the VM with no host-reachable API to
fetch it, which forced the k3s provisioner onto `container cp`. The bind-mount redesign
brings k3s to Talos parity: avoid the guest-agent channel entirely for the critical path.

---

## Decision

Replace `container cp` for both readiness and kubeconfig delivery:

1. **Readiness — host-side TLS dial.** The provisioner dials
   `https://<server-IP>:6443` directly from the host. The kube-apiserver answers TLS on
   that port. No guest agent, no `container exec`.

2. **Kubeconfig delivery — host bind-mount.** The server container bind-mounts the host
   cluster state directory (`<stateDir>/<cluster>`) at `/mnt/k3s-out` inside the VM.
   k3s is launched with:
   ```
   --write-kubeconfig /mnt/k3s-out/k3s.yaml --write-kubeconfig-mode 0644
   ```
   k3s writes the kubeconfig straight to the host filesystem. The provisioner polls
   `os.Stat` for the file, rewrites the server URL from `https://127.0.0.1:6443` to the
   FQDN endpoint (`https://<server-fqdn>:6443`), and writes the result to
   `<stateDir>/<cluster>/kubeconfig`.

3. **Zero `container cp` or `container exec` for kubeconfig or readiness.** One
   `container exec` call remains: `sysctl net.ipv4.ip_forward=1`, which runs early
   (before the boot I/O window), targets a standalone system binary (not a k3s
   multi-call symlink), and is a single non-repeating call.

### Verification (hardware, 2026-06-27)

Host at 80 % free RAM, server VM at 1536 MB. Provision exited 0. Kubeconfig written to
`_out/clusters/k3x/kubeconfig` with endpoint `https://k3x-server-1.aegis:6443`.
`kubectl get nodes` returned `k3x-server-1 Ready` (control-plane,master, v1.32.5+k3s1).
Destroy exited 0 in 1 second — no hang. Zero `container cp` used.

A separate spike (2026-06-27) confirmed virtio-fs write semantics: a guest root process
wrote a file to a virtio-fs bind-mount and `chmod 600` / `chmod 0644` both succeeded; the
host read the file back correctly.

---

## Consequences

### Positive

- **Robust.** Avoids the vsock channel entirely for the critical path; the vsock fault
  window (k3s image extraction) cannot affect readiness or kubeconfig delivery.
- **Fast destroy.** Removing the wedge risk means `container stop` and `container rm`
  return in under a second rather than hanging two or more minutes.
- **No recovery procedure.** The force-kill runbook (`pkill -9 container-runtime-linux`)
  is no longer needed during normal Create/Destroy cycles.
- **Talos parity.** Both provisioners now avoid the guest agent for their critical paths.

### Limits and watch items

- **Virtio-fs for plain file writes.** The bind-mount relies on virtio-fs to carry a
  plain sequential write (`k3s.yaml`). The spike confirmed this works. The named-volume
  datastore stays on a block-backed named volume because sqlite's WAL and advisory
  file-locking are the concern there, not the bind-mount itself.
- **One remaining `container exec`.** The `sysctl net.ipv4.ip_forward=1` call still uses
  `container exec`. It runs once, early, against a standalone binary — not a k3s
  multi-call symlink — so it is not in the vsock-fault window and has not caused issues.
- **Memory is an amplifier.** Tight host memory lengthens the k3s boot I/O window and
  makes the vsock fault more likely under the old design. The new design removes the
  dependency, but the amplification is worth knowing if the `sysctl exec` call ever
  becomes a concern.

---

## Alternatives considered

### Keep `container cp`, but wait for the vsock to settle

Wait for k3s boot I/O to complete before issuing `container cp` — either by a fixed
sleep or by detecting quiescence. **Rejected:** there is no reliable signal that the vsock
channel is safe. Even at 180 s with no imposed deadline, cp was killed externally. The
platform gives no per-channel health signal; a sleep-based guard is a heuristic that fails
on slower hardware.

### Reduce k3s boot I/O via `--disable`

Disable optional components (`traefik`, `metrics-server`) to shrink the bundle extraction
window. **Rejected:** this changes cluster capabilities and is not appropriate for a
general-purpose provisioner. The fault also reproduced on a host with 80 % free RAM,
suggesting the timing is inherent to k3s boot, not solely to host resource pressure.

---

## References

- [apple/container #1738](https://github.com/apple/container/issues/1738) — "Container
  copy doesn't expand host path correctly" (closed; fix in PR #1741, not in 1.0.0)
- [apple/container #861](https://github.com/apple/container/issues/861) — "Cannot Stop
  or Uninstall Container" (open; `container system stop` hangs; same daemon-hang class)
- [apple/container #576](https://github.com/apple/container/issues/576) — "Unable to
  stop a container when it's frozen" (open; stop/exec hang)
- [apple/containerization #712](https://github.com/apple/containerization/issues/712) —
  "Head-of-line blocking in BidirectionalRelay causes permanent hang under concurrent
  vsock proxy load" (closed; fix in PR #713, 0.35.0; a single long cp under sustained
  backpressure is a distinct unaddressed edge)
- [apple/containerization #678](https://github.com/apple/containerization/issues/678),
  [#572](https://github.com/apple/containerization/issues/572),
  [#503](https://github.com/apple/containerization/issues/503) — vsock reliability class
  (fd EBADF crashes; the project's longest-standing pain point)
