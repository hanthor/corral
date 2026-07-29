# Publishing Corral plugins

Corral plugins are standalone `corral-<name>` executables. The marketplace is
discovery and verified delivery; it does not load third-party code into the
main Corral process.

## Authoring contract

- Name the executable `corral-<name>`, where the name matches
  `^[a-z][a-z0-9-]{0,62}$`.
- Accept normal CLI arguments directly. Corral connects stdin/stdout/stderr and
  returns the plugin process's exit status. Human output belongs on stdout;
  diagnostics belong on stderr.
- Read `CORRAL_PLUGIN` when behavior depends on the dispatched name. Honor the
  same `CORRAL_KUBE_CONTEXT`, `CORRAL_INCUS_REMOTE`, and
  `CORRAL_LIBVIRT_URI` one-shot context variables as core commands. Never
  switch kubectl, Incus, or libvirt global state.
- Implement `--corral-plugin-metadata`. It must print one JSON object using
  API `corral.plugin/v1` and exit successfully without doing other work. Go
  plugins can call `sdk.HandleMetadata` before constructing their command.
- Treat capabilities and permissions as declarations, not authorization.
  Corral asks for install consent; the plugin must still use least-privilege
  credentials and rely on Kubernetes, filesystem, and network enforcement.
- Declare `supportedBackends` using `qemu`, `kubevirt`, `incus`, `libvirt`, or
  `all`. Reject a selected backend before mutation when it is not supported;
  do not silently fall back to another context.

Minimal Go entry point:

```go
package main

import (
    "fmt"
    "os"

    "github.com/tuna-os/corral/pkg/plugin/sdk"
)

func main() {
    if sdk.HandleMetadata(sdk.Metadata{
        Name: "hello", Version: "1.0.0",
        Capabilities: []string{"cli-command"},
        Permissions: []string{"read Corral configuration"},
        SupportedBackends: []string{"all"},
    }) { return }
    fmt.Fprintln(os.Stdout, "hello from Corral")
}
```

Test the binary directly, test the metadata handshake, then test dispatch with
`CORRAL_PLUGIN_DIR=/path/to/bin corral hello`. Plugins should not import
`cmd`; reusable backend behavior belongs in a `pkg/` package.

## External marketplace

1. Build immutable binaries for each advertised platform.
2. Publish them at HTTPS URLs that will never be replaced.
3. Create an index conforming to `marketplace/schema-v2.json`. Every artifact
   needs its SHA-256 digest. Publishers may additionally sign the lowercase
   digest with Ed25519 and publish the base64 public key.
4. Validate it with `corral marketplace validate index.json`.
5. Host the index over HTTPS and test it with
   `corral marketplace add <source> <url>`.

Duplicate plugin names must be selected as `source/name`. Installation shows
the publisher, requested permissions, and requires explicit permission
consent. Corral verifies compatibility, digest, and any supplied signature
before atomically replacing an installed plugin; failed state writes roll the
binary back.

## Recommended CI

Build from a version tag, run the plugin's tests, generate checksums, validate
the index, and publish both artifacts and index in one immutable release. The
repository's `scripts/generate-marketplace.sh` and `publish-plugins` workflow
are a working reference.

## First-party plugins

The maintained catalog and its maturity are documented in
[first-party-plugins.md](first-party-plugins.md). First-party means published
by tuna-os through the curated marketplace; it does not grant the plugin more
runtime privilege than an external plugin.
