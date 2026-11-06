#!/bin/bash
# build.sh <kubernetes checkout> <out.wasm>
# Builds the kwasm binary from a checkout prepared by patch.go.
set -euo pipefail
K="$1"
OUT="$(cd "$(dirname "$2")" && pwd)/$(basename "$2")"
cd "$K"
V=$(git describe --tags --exact-match 2>/dev/null || echo v0.0.0)
MAJOR=$(echo "$V" | sed -E 's/^v([0-9]+)\..*/\1/')
MINOR=$(echo "$V" | sed -E 's/^v[0-9]+\.([0-9]+)\..*/\1/')
COMMIT=$(git rev-parse HEAD)
DATE=$(git show -s --format=%cI HEAD | sed -E 's/\+00:00$/Z/')
LD="-s -w"
for pkg in k8s.io/component-base/version k8s.io/client-go/pkg/version; do
	LD="$LD -X $pkg.gitVersion=$V -X $pkg.gitMajor=$MAJOR -X $pkg.gitMinor=$MINOR -X $pkg.gitCommit=$COMMIT -X $pkg.gitTreeState=clean -X $pkg.buildDate=$DATE"
done
GOOS=js GOARCH=wasm go build -trimpath -ldflags="$LD" -o "$OUT" ./cmd/kwasm
