#!/usr/bin/env bash
# Build the plain-Go release layout used by Plori's gVisor fork.
set -euo pipefail

tag=${1:?usage: tools/plori-release-build.sh RELEASE_TAG}
out=${2:-out}
src="$out/.plori-release-src"
trap 'rm -rf "$src"' EXIT

mkdir -p "$out/gvisor-bin" "$src/sentry"
ldflags="-s -w -X gvisor.dev/gvisor/runsc/version.version=${tag}"

go build -trimpath -ldflags "$ldflags" -o "$out/runsc" ./runsc
go build -trimpath -ldflags '-s -w' -o "$out/containerd-shim-runsc-v1" ./shim

# The synthetic Go export does not contain Bazel's runsc/cmd/sentry/sentry_main.go.
# Keep this entrypoint byte-for-byte equivalent in behavior to the upstream target
# at a955aa09e3a8c67843a89b9e4ff14c0f1d12549b, while linking the release tag here.
cat >"$src/sentry/main.go" <<'EOF'
package main

import (
	"gvisor.dev/gvisor/runsc/cli"
	"gvisor.dev/gvisor/runsc/cmd/sentry/sentrycmd"
	"gvisor.dev/gvisor/runsc/cmd/util"
	"gvisor.dev/gvisor/runsc/gvisorbinaries"
)

func main() {
	cli.Run(&gvisorbinaries.GvisorSentry, map[util.SubCommand]string{
		new(sentrycmd.Boot):   "internal use only",
		new(sentrycmd.Umount): "internal use only",
	}, nil)
}
EOF
go build -trimpath -ldflags "$ldflags" -o "$out/gvisor-bin/gvisor_sentry" "$src/sentry"

# The strict boot path invokes this freestanding C program before gvisor_sentry.
# Fetch it by immutable upstream revision and verify its content because the plain
# Go export deliberately omits C sources and Bazel metadata.
prewarmer_url=https://raw.githubusercontent.com/google/gvisor/a955aa09e3a8c67843a89b9e4ff14c0f1d12549b/runsc/prewarmer/prewarmer.c
prewarmer_src="$src/prewarmer.c"
curl --fail --location --silent --show-error "$prewarmer_url" -o "$prewarmer_src"
printf '%s  %s\n' \
  dc7285c3d089e7df59222d58bf250e4f02062c89b1064e8fe37f67b1d3105880 \
  "$prewarmer_src" | sha256sum --check --status
cc -Os -fno-pie -ffreestanding -fno-sanitize=all -fno-stack-protector \
  -fno-asynchronous-unwind-tables -fno-unwind-tables -static -nostdlib \
  -nostartfiles -Wl,--build-id=none -Wl,--gc-sections -Wl,-s -Wl,-n -no-pie \
  -o "$out/gvisor-bin/gvisor-sentry-prewarmer" "$prewarmer_src"

got=$("$out/runsc" --version | head -n1)
[ "$got" = "runsc version ${tag}" ] || {
  echo "version stamp mismatch: $got" >&2
  exit 1
}
test -x "$out/gvisor-bin/gvisor_sentry"
test -x "$out/gvisor-bin/gvisor-sentry-prewarmer"
test "$(stat --format=%s "$out/gvisor-bin/gvisor-sentry-prewarmer")" -le 4096

tar -C "$out" -cjf gvisor.tar.bz2 runsc containerd-shim-runsc-v1 gvisor-bin
sha512sum gvisor.tar.bz2 >gvisor.tar.bz2.sha512
