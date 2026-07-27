# Corral task runner. `just` (https://github.com/casey/just) — run `just` to list.
# Build tags: most code is plain; the bootc plugin needs `-tags bootc`.

# Version stamp for `corral version`. Defaults to the current git describe;
# a plain `go build` still self-reports via the toolchain's VCS stamps.
export CORRAL_VERSION := `git describe --tags --always --dirty 2>/dev/null || echo dev`
_ldflags := "-X github.com/tuna-os/corral/cmd.version=" + CORRAL_VERSION

_default:
    @just --list

# Build everything (both tag sets).
build:
    go build -ldflags "{{_ldflags}}" ./...
    go build -tags bootc -ldflags "{{_ldflags}}" ./...

# Install corral + the bootc plugin into ~/.local/bin and the plugin dir.
# This is the supported "get current" path — run it after `git pull`.
install:
    go build -ldflags "{{_ldflags}}" -o ~/.local/bin/corral .
    go build -tags bootc -ldflags "{{_ldflags}}" -o "${XDG_DATA_HOME:-$HOME/.local/share}/corral/plugins/corral-bootc" ./cmd/corral-bootc
    go build -ldflags "{{_ldflags}}" -o "${XDG_DATA_HOME:-$HOME/.local/share}/corral/plugins/corral-incus" ./cmd/corral-incus
    @echo "✓ installed: $(~/.local/bin/corral version)"

# Run the full test suite (both tag sets), race detector on.
test:
    go test -race -count=1 ./...
    go test -race -count=1 -tags bootc ./...

# Format Go sources in place.
fmt:
    gofmt -w pkg cmd .

# Static analysis (both tag sets).
vet:
    go vet ./...
    go vet -tags bootc ./...

# The local pre-push gate — mirrors CI's `test` job.
ci: fmt-check vet build test

# Fail if anything isn't gofmt-clean (what CI checks).
fmt-check:
    @unformatted="$(gofmt -l pkg cmd .)"; \
      if [ -n "$unformatted" ]; then echo "gofmt needed on:"; echo "$unformatted"; exit 1; fi

# Run the web UI locally against the current kube context.
web addr="127.0.0.1:8006":
    go run -tags bootc . web --addr {{addr}}

# Refresh the ublue/bluefin/tuna bootc catalog from ghcr (drops >60d-stale). Needs gh + curl.
regen-catalog:
    python3 scripts/regen-catalog.py
    gofmt -w pkg/catalog/catalog_generated.go
    go build ./... && go test ./pkg/catalog/
    @echo "✓ catalog regenerated — review 'git diff pkg/catalog/catalog_generated.go'"
