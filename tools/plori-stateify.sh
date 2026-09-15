#!/bin/bash
# Regenerate the synthetic Go branch's FUSE state methods with upstream's tool.
set -euo pipefail
cd "$(dirname "$0")/.."
mode=${1:---write}
[[ "$mode" == --write || "$mode" == --check ]] || { echo "usage: $0 [--write|--check]" >&2; exit 2; }
package=pkg/sentry/fsimpl/fuse
output=$(mktemp "${TMPDIR:-/tmp}/plori-fuse-state.XXXXXX.go")
trap 'rm -f "$output"' EXIT
mapfile -t sources < <(find "$package" -maxdepth 1 -name '*.go' ! -name '*autogen*' ! -name '*_unsafe.go' ! -name '*_test.go' | sort)
GOWORK=off go run ./tools/go_stateify -fullpkg="$package" \
  -statepkg=gvisor.dev/gvisor/pkg/state -output="$output" -- "${sources[@]}"
GOWORK=off go run golang.org/x/tools/cmd/goimports@v0.43.0 -w "$output"
if [[ "$mode" == --check ]]; then
  diff -u "$package/fuse_state_autogen.go" "$output"
else
  cp "$output" "$package/fuse_state_autogen.go"
fi
