# Vendored console libraries

Pinned copies so the consoles work offline / air-gapped (same rationale as
the vendored `../alpine.min.js` — see docs/adr/0004-web-ui-alpinejs-no-build.md).

`MANIFEST.json` is the machine-readable record of the same set: package,
version, upstream URL, SHA-256 and byte count for every third-party browser
asset corral embeds, including `../alpine.min.js`. The table below is the
human-readable view of it; the two must not disagree.

| File | Source |
|---|---|
| `xterm.min.js`, `xterm.min.css` | `@xterm/xterm@5.5.0` |
| `addon-fit.min.js` | `@xterm/addon-fit@0.10.0` |
| `novnc-rfb.esm.js` | `@novnc/novnc@1.4.0` `core/rfb.js`, bundled to a single ES module via jsDelivr `/+esm` |
| `iron-remote-desktop.js` | **unknown** — provenance was not recorded when this file was vendored, and the build carries no version string. See [#215](https://github.com/tuna-os/corral/issues/215) |
| `../alpine.min.js` | `alpinejs@3.15.12` (per ADR-0004) |

To update, re-download from jsdelivr with the new version pinned:

```sh
curl -fsSLo xterm.min.js      https://cdn.jsdelivr.net/npm/@xterm/xterm@<v>/lib/xterm.min.js
curl -fsSLo xterm.min.css     https://cdn.jsdelivr.net/npm/@xterm/xterm@<v>/css/xterm.min.css
curl -fsSLo addon-fit.min.js  https://cdn.jsdelivr.net/npm/@xterm/addon-fit@<v>/lib/addon-fit.min.js
curl -fsSLo novnc-rfb.esm.js "https://cdn.jsdelivr.net/npm/@novnc/novnc@<v>/core/rfb.js/+esm"
```

Then update `MANIFEST.json` in the same commit — new `version`, `url`, `sha256`
(`sha256sum <file>`) and `bytes` (`wc -c <file>`).

`iron-remote-desktop.js` has no update recipe yet because its upstream is not
known. `MANIFEST.json` pins the digest of the copy in tree so the file cannot
be changed unnoticed, but that digest cannot be checked against an upstream
release until the source package and version are identified.
