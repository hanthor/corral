# Contributing

Thanks for your interest in contributing! This project is part of the [TunaOS](https://tunaos.org) ecosystem.

## Getting Started

1. Fork the repo and clone it locally.
2. Install the Go version declared in [`go.mod`](go.mod) and the
   [`just`](https://just.systems) command runner.
3. Read the project [README](README.md), [architecture overview](docs/architecture.md),
   and [testing strategy](docs/testing.md).
4. Open an issue to discuss your change before submitting a PR.

## Validate Your Changes

Corral builds two tag sets — plain and `-tags bootc` (the bootc plugin) —
and CI checks both. Run the local pre-push gate before opening a PR:

```bash
just ci
```

This runs, in order: `gofmt` (fmt-check), `go vet` (both tag sets),
`go build` (both tag sets), and `go test -race` (both tag sets). You can
also run the steps individually:

```bash
just fmt     # gofmt -w
just vet     # go vet, both tag sets
just build   # go build, both tag sets
just test    # go test -race, both tag sets
```

Some tests exercise real tools (qemu-img, rclone conversions); install them
locally if you touch that code path, or rely on CI to run them.

## Pull Requests

- Keep PRs focused — one change per PR.
- Follow the existing code style and conventions.
- Update `docs/` if your change affects usage, backend parity, or architecture.
- Include the validation commands you ran in the PR description.

## Questions?

- [TunaOS Documentation](https://tunaos.org)
- [GitHub Issues](https://github.com/tuna-os/tunaOS/issues)
