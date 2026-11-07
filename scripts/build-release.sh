#!/usr/bin/env bash
# Builds the Go agent release assets (docs/go-rewrite/update.md):
#   <out>/route-agent-linux-amd64, <out>/route-agent-linux-arm64, <out>/checksums.txt
#
# Usage: bash scripts/build-release.sh <version> [out-dir]
#   version  vX.Y.Z or vX.Y.Z-N-gHASH (the shape the orchestrator parses, spec A §7.3); anything
#            else is refused, because a node reporting it would be "behind" forever and get
#            self-updated every 6 hours.
#   out-dir  default dist-go (relative to the current directory)
#
# One script for every producer — go.yml (CI of the branch and the go-preview prerelease),
# release.yml (assets of a semantic-release release) and scripts/test-delivery.sh — so the asset
# names, the ldflags and the checksums format cannot drift between them.
set -euo pipefail

version="${1:-}"
out="${2:-dist-go}"
VERSION_RE='^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9]+-g[0-9a-f]+)?$'

if [[ ! "$version" =~ $VERSION_RE ]]; then
  echo "build-release: version '${version}' does not have the shape vX.Y.Z[-N-gHASH]" >&2
  exit 1
fi

mkdir -p "$out"
out="$(cd "$out" && pwd)"
cd "$(dirname "${BASH_SOURCE[0]}")/.."
rm -f "$out"/route-agent-linux-* "$out/checksums.txt"

for arch in amd64 arm64; do
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath \
    -ldflags "-s -w -X github.com/Ametas/route-agent/internal/version.Version=${version}" \
    -o "$out/route-agent-linux-${arch}" ./cmd/route-agent
done

# The binary must print exactly the version it was built with: nodes check `--version` before
# installing it (SelfUpdate, install-go.sh, bridge-to-go.sh). Only runnable on an amd64 host.
if [[ "$(uname -m)" == "x86_64" ]]; then
  got="$(cd / && "$out/route-agent-linux-amd64" --version)"
  if [[ "$got" != "$version" ]]; then
    echo "build-release: built binary reports '${got}', expected '${version}'" >&2
    exit 1
  fi
fi

(cd "$out" && sha256sum route-agent-linux-amd64 route-agent-linux-arm64 > checksums.txt && cat checksums.txt)
