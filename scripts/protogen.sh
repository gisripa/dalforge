#!/usr/bin/env bash
# Generates Go bindings for the dalforge options protos into <out-root>,
# mirroring the proto/ tree (e.g. <out-root>/proto/dal/v1/options.pb.go).
# Usage: scripts/protogen.sh <out-root>
set -euo pipefail

out=${1:?usage: protogen.sh <out-root>}
cd "$(dirname "$0")/.."

# protoc-gen-go is a go.mod tool dependency, so its version always matches the
# google.golang.org/protobuf runtime.
plugin=$(go tool -n protoc-gen-go)

mkdir -p "$out"
protoc \
  -I proto \
  --plugin=protoc-gen-go="$plugin" \
  --go_out="$out" \
  --go_opt=module=github.com/gisripa/dalforge \
  dal/v1/options.proto \
  dal/pg/v1/options.proto
