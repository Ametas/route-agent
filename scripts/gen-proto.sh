#!/usr/bin/env bash
# Regenerates internal/agentpb from proto/agent.proto.
#
# proto/agent.proto is shared byte-for-byte with the orchestrator and has no go_package option;
# the Go import path is supplied here with M-flags instead of editing the file.
#
# Pinned toolchain (the generated headers record these versions):
#   protoc             v29.3   (https://github.com/protocolbuffers/protobuf/releases/tag/v29.3,
#                               protoc-29.3-linux-x86_64.zip)
#   protoc-gen-go      v1.36.6 (go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.6)
#   protoc-gen-go-grpc v1.5.1  (go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1)
#
# Usage: PROTOC=/path/to/protoc bash scripts/gen-proto.sh   (from the repo root)
set -euo pipefail

PROTOC="${PROTOC:-protoc}"
GOBIN_DIR="$(go env GOPATH)/bin"
MOD=github.com/Ametas/route-agent
PKG="$MOD/internal/agentpb"

cd "$(dirname "$0")/.."

PATH="$GOBIN_DIR:$PATH" "$PROTOC" \
  --proto_path=proto \
  --go_out=. --go_opt=module="$MOD" --go_opt=Magent.proto="$PKG" \
  --go-grpc_out=. --go-grpc_opt=module="$MOD" --go-grpc_opt=Magent.proto="$PKG" \
  agent.proto

gofmt -l internal/agentpb
