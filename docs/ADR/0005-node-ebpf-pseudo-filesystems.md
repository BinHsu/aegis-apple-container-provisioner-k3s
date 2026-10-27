# ADR-0005: Mount debugfs, tracefs and bpffs on every k3s node, by default

**Status:** Accepted, 2026-10-27 (unit-tested; hardware verification pending)

---

## Context

A systemd-booted Linux host mounts `debugfs` on `/sys/kernel/debug`, `tracefs` on `/sys/kernel/tracing`
and `bpffs` on `/sys/fs/bpf`. eBPF agents assume them: kprobe/tracepoint attach and kernel offset
discovery read tracefs (directly or through debugfs), and pinned maps live on bpffs.

An apple/container 1.5.0 node (guest kernel 6.18 arm64) boots without systemd. The three directories
exist — the kernel offers the filesystems — but nothing is mounted on them. Observed 2026-10-27:

| Agent in a pod on the node | Result |
|---|---|
| DataKit eBPF 2.14.3 | fails: "neither debugfs nor tracefs are mounted" |
| OpenTelemetry eBPF Instrumentation (OBI), network-level context propagation | needs tracefs |

The v0.7.0 node fixes (`/proc/sys` remount, `/` rshared, TSO/GSO off) already run in one idempotent
preparation step on create, start, add, upgrade/rollback, rotate and restore.

## Decision

Add the three mounts to that step (`mountEBPFKernelFS`, after `/` is made rshared so the new mounts
propagate into pods that bind-mount the node's `/sys`). The step reads `/proc/mounts` and
`/proc/filesystems` in the guest and runs `mount -t <fs> <fs> <target>` only for filesystems the kernel
lists and that are not already mounted at that target. The decision is a pure function
(`missingKernelFS`) with unit tests for the fresh, already-prepared, partial and limited-kernel cases.

**Legacy tracefs path.** After debugfs is mounted, tracefs is also mounted at `/sys/kernel/debug/tracing`
(`legacyTracefsNeeded`: kernel offers tracefs, debugfs is on `/sys/kernel/debug`, nothing is mounted at the
legacy path yet). Observed on a real node with only the three mounts above: the directory does not exist
(this kernel does not automount tracefs inside debugfs), and DataKit 2.14.3 netflow still failed with
"neither debugfs nor tracefs are mounted", because the official DataKit DaemonSet hostPath-mounts only
`/sys/kernel/debug` and never sees `/sys/kernel/tracing`. k3ac runs `mkdir -p` first; if debugfs refuses
it, k3ac prints a warning and continues (the node is no worse than before), while a failed mount after a
successful mkdir is an error like the others.

**Default-on, no flag.** These mounts are what a stock Linux host already has, so they change nothing
for workloads that do not use them; a node that already has them is left untouched, so re-runs are
no-ops; a kernel that lacks one of them is skipped rather than failing node preparation. A flag would add
a choice with no reason to pick "off", and an existing cluster would need to be re-created to flip it.
A mount failure on a filesystem the kernel does offer is reported as an error, like the other node
fixes, because it means the node is not in the state k3ac promises.

## Consequences

- An existing cluster gets the mounts on its next `k3ac -start <name>`; no re-create.
- debugfs exposes kernel internals to anything that can already reach the node's `/sys` — i.e. privileged
  pods with a hostPath, which on these single-user lab nodes (`--cap-add ALL`) could mount it themselves.
  No new capability is granted.
- The mounts are not persisted across a node reboot by themselves; the preparation step re-creates them,
  which is why every path that boots a node runs it.
