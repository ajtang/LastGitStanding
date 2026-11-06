#!/usr/bin/env bash
# Generated code is committed so that `go build` works on a laptop with no
# protoc, which is every laptop the first time. CI re-runs this and fails on drift.
set -euo pipefail
cd "$(dirname "$0")/.."
export PATH="$(go env GOPATH)/bin:$PATH"
rm -rf gen/lgs
protoc -I proto \
  --go_out=gen --go_opt=module=github.com/ajtang/LastGitStanding/gen \
  --go-grpc_out=gen --go-grpc_opt=module=github.com/ajtang/LastGitStanding/gen \
  proto/lgs/v1/*.proto
