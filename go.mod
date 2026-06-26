module github.com/BinHsu/aegis-apple-container-provisioner-k3s

// SPIKE DRAFT. Stdlib-only on purpose: unlike the Talos sibling there is NO k3s
// "provisioner interface" to satisfy, so this module imports no upstream framework.
// It is a standalone launcher that shells out to Apple's `container` CLI.
//
// UNVERIFIED ASSUMPTION: pinned to the same Go toolchain line as the Talos sibling
// (go1.26). Verify this matches the local toolchain before building.
go 1.26
