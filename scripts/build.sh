#!/usr/bin/env bash
# Cross-compile static release archives into ./dist
#   VERSION=v1.0.0 ./scripts/build.sh
#   TARGETS="linux/amd64" ./scripts/build.sh
set -euo pipefail
cd "$(dirname "$0")/.."

VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
OUT="${OUT:-dist}"
TARGETS="${TARGETS:-linux/amd64 linux/arm64 linux/arm/7}"

rm -rf "$OUT"
mkdir -p "$OUT"
for t in $TARGETS; do
  IFS=/ read -r os arch arm <<<"$t"
  name="serverstatus_${os}_${arch}${arm:+v$arm}"
  dir="$OUT/$name"
  mkdir -p "$dir"
  for cmd in serverstatus-server serverstatus-agent; do
    echo "building $name/$cmd ($VERSION)"
    CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" GOARM="$arm" \
      go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$dir/$cmd" "./cmd/$cmd"
  done
  cp deploy/install.sh deploy/agent.example.yaml README.md "$dir/"
  "$dir/serverstatus-server" config-example > "$dir/server.example.yaml" 2>/dev/null \
    || go run ./cmd/serverstatus-server config-example > "$dir/server.example.yaml"
  chmod +x "$dir/install.sh"
  tar -C "$OUT" -czf "$OUT/$name.tar.gz" "$name"
  rm -rf "$dir"
done
(cd "$OUT" && sha256sum ./*.tar.gz > SHA256SUMS)
ls -lh "$OUT"
