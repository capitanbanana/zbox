#!/bin/bash
# Build portable release archives with Go's built-in ad-hoc signing on arm64.
set -euo pipefail
cd "$(dirname "$0")/.."
version=${1:?Usage: scripts/release.sh vX.Y.Z}
[[ $version =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo 'Expected vX.Y.Z' >&2; exit 1; }
mkdir -p dist
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT
for arch in arm64 amd64; do
  mkdir "$stage/$arch"
  CGO_ENABLED=0 GOOS=darwin GOARCH=$arch go build -trimpath -buildvcs=false -ldflags="-s -w -X main.version=$version" -o "$stage/$arch/agents" ./cmd/agents
  CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -buildvcs=false -ldflags="-s -w -X main.version=$version" -o "$stage/$arch/agents-linux-$arch" ./cmd/agents
  # Normalize archive metadata: never publish a builder's username, UID,
  # filesystem paths, extended attributes, or local timestamps.
  python3 - "$stage/$arch" "dist/agents-$version-darwin-$arch.tar.gz" "$arch" <<'PY'
import gzip
from pathlib import Path
import sys
import tarfile

source, target, arch = sys.argv[1:]
with open(target, "wb") as output:
    with gzip.GzipFile(filename="", mode="wb", fileobj=output, mtime=0) as compressed:
        with tarfile.open(fileobj=compressed, mode="w", format=tarfile.USTAR_FORMAT) as archive:
            for name in ("agents", f"agents-linux-{arch}"):
                path = Path(source) / name
                entry = tarfile.TarInfo(name)
                entry.mode = 0o755
                entry.size = path.stat().st_size
                with path.open("rb") as content:
                    archive.addfile(entry, content)
PY
done
cd dist
shasum -a 256 "agents-$version-darwin-arm64.tar.gz" "agents-$version-darwin-amd64.tar.gz" > SHA256SUMS
printf 'Release assets ready in dist/\n'
