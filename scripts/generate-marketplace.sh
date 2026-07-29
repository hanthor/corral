#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 3 ]]; then
  echo "usage: $0 <plugin-dist-dir> <release-tag> <output>" >&2
  exit 2
fi

dist=$1
tag=$2
output=$3
repo=${GITHUB_REPOSITORY:-tuna-os/corral}
tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT

jq --arg tag "$tag" --arg repo "$repo" '
  {
    schemaVersion: "corral.marketplace/v2",
    name: "Corral curated marketplace",
    publisher: {name: "tuna-os", url: "https://github.com/tuna-os"},
    plugins: [.plugins[]
      | select(.name != "incus")
      | .license = (.license // "Apache-2.0")
      | .pluginApi = (.pluginApi // "corral.plugin/v1")
      | .corral = (.corral // ">=0.1.0")
      | .publisher = (.publisher // {name: "tuna-os", url: "https://github.com/tuna-os/corral"})
      | .capabilities = (.capabilities // ["cli-command"])
      | .permissions = (.permissions // ["execute subprocesses", "read Corral configuration"])
      | .supportedBackends = (.supportedBackends // (if .name == "auth" then ["all"] else ["kubevirt"] end))
      | .platforms |= with_entries(
          .value.url = ("https://github.com/" + $repo + "/releases/download/" + $tag + "/corral-" + input_filename)
        )
    ]
  }
' marketplace/index.json > "$tmp"

# jq cannot infer the plugin name/platform from with_entries alone while also
# keeping this script readable, so populate immutable URLs and digests in one
# deterministic pass.
cp "$tmp" "$output"
for artifact in "$dist"/corral-*-linux-*; do
  [[ -f "$artifact" ]] || continue
  filename=$(basename "$artifact")
  plugin=${filename#corral-}; plugin=${plugin%-linux-*}
  arch=${filename##*-linux-}
  sha=$(sha256sum "$artifact" | awk '{print $1}')
  next=$(mktemp)
  jq --arg plugin "$plugin" --arg platform "linux/$arch" \
     --arg url "https://github.com/$repo/releases/download/$tag/$filename" \
     --arg sha "$sha" \
     '(.plugins[] | select(.name == $plugin) | .platforms[$platform]) = {url: $url, sha256: $sha}' \
     "$output" > "$next"
  mv "$next" "$output"
done

jq -e '
  .schemaVersion == "corral.marketplace/v2" and
  ([.plugins[] | (.supportedBackends | length > 0)] | all) and
  ([.plugins[].platforms[] | (.sha256 | length == 64)] | all)
' "$output" >/dev/null
