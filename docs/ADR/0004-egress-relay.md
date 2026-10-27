# ADR-0004: Guest egress through a host-side userspace relay (`ac-egress`)

**Status:** Accepted, 2026-10-27 (hardware-verified with Cloudflare WARP connected)

---

## Context

Where Cloudflare WARP has to stay connected on the Mac (for example by organisation policy), Apple
`container` VMs on a vmnet network have no egress (measured 2026-10-20 and 2026-10-27 on
`tw-poc-net`, 192.168.200.0/24):

| From a node | Result |
|---|---|
| DNS to 1.1.1.1 / 8.8.8.8, and to the gateway 192.168.200.1 | timeout |
| TCP to a literal public IP (`wget http://1.1.1.1/`) | timeout |
| ICMP to the gateway 192.168.200.1 | 100% loss |
| ICMP to the other node | ok |
| **TCP to a listener on the host bound to 192.168.200.1** (bridge100) | **ok** (accepted from the node IP) |

The host itself reaches the internet (through WARP, with TLS inspection by the Gateway CA).
colima keeps egress under WARP because its user-mode networking turns guest traffic into
ordinary host-process sockets. vmnet NAT traffic evidently is not handled the same way.

## Decision

Add a userspace relay on the host, bound only to the bridge (gateway) address, and steer
guest traffic to it from inside the guest:

- **`ac-egress`** (package `egress`, stdlib only, no k3s imports — it is meant to move to its
  own repository) serves transparent HTTP by `Host`, transparent TLS by SNI (passthrough, no
  decryption), an explicit HTTP/CONNECT proxy on the HTTP port, and DNS forwarding to the
  host's resolvers.
- **Guest steering** is an iptables `nat` chain (`AC-EGRESS`) in `OUTPUT` and `PREROUTING`
  that DNATs outbound tcp/80, tcp/443 and udp+tcp/53 to non-private destinations. Pod traffic
  is caught in the node's `PREROUTING`, so pods need no configuration.
- **k3ac** only consumes it: `-egress` finds the network gateway, runs the relay detached with
  a pid file, installs the chain on every k3s node, and records the choice in state; `-stop`
  and `-destroy` stop it by verified pid.

## Alternatives considered

- **`HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY` on nodes and pods.** Node env is fixed at
  `container run`, so an existing cluster would need re-creation; every pod would need the env
  (and must honour it); `NO_PROXY` has to enumerate pod/service CIDRs, node IPs and suffixes;
  DNS is still unsolved. The relay keeps this mode available (CONNECT on 15080) for clients
  that prefer it, but k3ac does not depend on it.
- **Host packet forwarding / pf rules / a route change.** Needs root and changes global network
  configuration; out of bounds.
- **Route guest traffic through colima.** Works, but adds a second VM runtime as a hard
  dependency and an extra hop; kept as a fallback only.
- **DNS on port 53 of the bridge address.** Unnecessary: the guest chain DNATs to an
  unprivileged port.

## Consequences

- Covered: HTTP, HTTPS/TLS with SNI, DNS. Not covered: HTTPS to literal IPs (no SNI — the DNAT
  erased the original destination, so the relay cannot route it), other TCP ports, UDP other
  than DNS, IPv6.
- TLS inspection still applies (the relay's upstream sockets are host sockets), so guests and
  pods that verify TLS still need the inspecting CA (`-ca-dir`).
- Inbound exposure: listeners bind only to an address on a `bridge*` interface and accept only
  sources in that bridge's subnet; upstream dials are restricted to public unicast addresses,
  checked after resolution, so the relay cannot be used to reach host-loopback or LAN services.
- The bridge address exists only while a VM on that network runs, so the relay is started
  after the nodes and stopped with them.
- `-start` also rewrites the `# k3ac` lines of each node's `/etc/hosts` with current IPs: with
  DNS going to the host's resolvers, cluster FQDNs exist only in `/etc/hosts`, which a cold
  restart resets.
