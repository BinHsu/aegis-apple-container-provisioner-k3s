module github.com/BinHsu/aegis-apple-container-provisioner-k3s

// Stdlib-only on purpose: unlike the Talos sibling there is NO k3s
// "provisioner interface" to satisfy, so this module imports no upstream framework.
// It is a standalone launcher that shells out to Apple's `container` CLI.
//
// CI pins Go 1.26.4 (fixes GO-2026-5037/GO-2026-5039); local toolchain must be >= 1.26.4.
go 1.26
