# SPDX-License-Identifier: MIT
#
# Local checks. These mirror what CI would run, so a problem fails on the machine before it
# ever leaves it. CI / branch protection for this repo is deliberately deferred until G1
# passes (see docs/VERIFICATION.md) and is handled separately.

.PHONY: build vet test fmt secrets check

# build compiles every package.
build:
	go build ./...

# vet runs the Go static checks.
vet:
	go vet ./...

# test runs the unit tests (BVA recipe-lock + stale-state guard).
test:
	go test ./...

# fmt fails if any file is not gofmt-clean (does not rewrite; run `gofmt -w .` to fix).
fmt:
	@out="$$(gofmt -l .)"; \
	if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; echo "run: gofmt -w ."; exit 1; fi

# secrets scans the working tree for committed secrets BEFORE they reach CI — the local
# half of the secret-scan defense (the Talos sibling hit a gitleaks/CI surprise; catching
# it locally pre-empts that). Requires gitleaks on PATH (`brew install gitleaks`).
secrets:
	@command -v gitleaks >/dev/null 2>&1 || { \
		echo "gitleaks not found — install it: brew install gitleaks"; exit 1; }
	gitleaks detect --source . --redact --no-banner

# check runs the full local gate: formatting, build, vet, tests, and secret scan.
check: fmt build vet test secrets
