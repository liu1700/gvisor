#!/usr/bin/env bash
# Build the plain-Go release layout used by Plori's gVisor fork.
set -euo pipefail

tag=${1:?usage: tools/plori-release-build.sh RELEASE_TAG}
out=${2:-out}
src="$out/.plori-release-src"
trap 'rm -rf "$src"' EXIT

mkdir -p "$out/gvisor-bin" "$src/sentry"
ldflags="-s -w -X gvisor.dev/gvisor/runsc/version.version=${tag}"
upstream_rev=a955aa09e3a8c67843a89b9e4ff14c0f1d12549b

if [ "${GOOS:-$(go env GOOS)}" != linux ] || [ "${GOARCH:-$(go env GOARCH)}" != amd64 ]; then
  echo "this release workflow supports linux/amd64 only" >&2
  exit 1
fi

go build -trimpath -ldflags "$ldflags" -o "$out/runsc" ./runsc
go build -trimpath -ldflags '-s -w' -o "$out/containerd-shim-runsc-v1" ./shim

# The synthetic Go export intentionally excludes Bazel-only entrypoints, C
# sources, and the packages used only by optional sidecars. Build a temporary
# source tree from this exact fork revision, then fill those omitted paths from
# the upstream master revision contained by this Go export.
mkdir -p "$src/buildroot"
git archive --format=tar HEAD | tar -x -C "$src/buildroot"
git init --quiet "$src/upstream"
git -C "$src/upstream" remote add origin https://github.com/google/gvisor.git
git -C "$src/upstream" fetch --depth=1 --quiet origin "$upstream_rev"
git -C "$src/upstream" checkout --detach --quiet FETCH_HEAD
[ "$(git -C "$src/upstream" rev-parse HEAD)" = "$upstream_rev" ] || {
  echo "unexpected upstream source revision" >&2
  exit 1
}
git -C "$src/upstream" archive --format=tar "$upstream_rev" \
  runsc/checkpointgofer/gcs runsc/metricserver | tar -x -C "$src/buildroot"
cp "$src/upstream/runsc/cmd/sentry/sentry_main.go" "$src/buildroot/release-sentry-main.go"
cp "$src/upstream/runsc/checkpointgofer/main.go" "$src/buildroot/release-checkpointgofer-main.go"
cp "$src/upstream/runsc/cmd/metricserver/metricserver_main.go" "$src/buildroot/release-metricserver-main.go"

# Each Go wrapper must live in a distinct main package. The source files are
# copied from upstream unchanged; the generated package state comes from the
# current synthetic export.
mkdir -p "$src/buildroot/release-sentry" "$src/buildroot/release-checkpointgofer" "$src/buildroot/release-metricserver"
mv "$src/buildroot/release-sentry-main.go" "$src/buildroot/release-sentry/main.go"
mv "$src/buildroot/release-checkpointgofer-main.go" "$src/buildroot/release-checkpointgofer/main.go"
mv "$src/buildroot/release-metricserver-main.go" "$src/buildroot/release-metricserver/main.go"
go -C "$src/buildroot" build -trimpath -ldflags "$ldflags" \
  -o "$out/gvisor-bin/gvisor_sentry" ./release-sentry

# Build each standard upstream release sidecar. checkpointgofer and metric
# server need packages excluded from the synthetic Go export, so they build
# from the composite source tree. The two C programs use the flags from their
# upstream Bazel targets.
go -C "$src/buildroot" build -trimpath -ldflags "$ldflags" \
  -o "$out/gvisor-bin/checkpointgofer" ./release-checkpointgofer
go -C "$src/buildroot" build -trimpath -ldflags "$ldflags" \
  -o "$out/gvisor-bin/runsc-metric-server" ./release-metricserver
cc -Os -fno-pie -ffreestanding -fno-sanitize=all -fno-stack-protector \
  -fno-asynchronous-unwind-tables -fno-unwind-tables -static -nostdlib \
  -nostartfiles -Wl,--build-id=none -Wl,--gc-sections -Wl,-s -Wl,-n -no-pie \
  -o "$out/gvisor-bin/gvisor-sentry-prewarmer" "$src/upstream/runsc/prewarmer/prewarmer.c"
cc -Os -fno-pie -ffreestanding -fno-sanitize=all -fno-stack-protector \
  -fno-asynchronous-unwind-tables -fno-unwind-tables -static -nostdlib \
  -nostartfiles -Wl,--build-id=none -Wl,--gc-sections -Wl,-n -Wl,-s -no-pie \
  -o "$out/gvisor-bin/runsc-fd-parking" "$src/upstream/runsc/fdparking/fdparking.c"

got=$("$out/runsc" --version | head -n1)
[ "$got" = "runsc version ${tag}" ] || {
  echo "version stamp mismatch: $got" >&2
  exit 1
}
test -x "$out/gvisor-bin/gvisor_sentry"
test -x "$out/gvisor-bin/gvisor-sentry-prewarmer"
test -x "$out/gvisor-bin/checkpointgofer"
test -x "$out/gvisor-bin/runsc-metric-server"
test -x "$out/gvisor-bin/runsc-fd-parking"
test "$(stat --format=%s "$out/gvisor-bin/gvisor-sentry-prewarmer")" -le 4096

tar -C "$out" -cjf gvisor.tar.bz2 runsc containerd-shim-runsc-v1 gvisor-bin
sha512sum gvisor.tar.bz2 >gvisor.tar.bz2.sha512
