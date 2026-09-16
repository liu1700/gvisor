#!/bin/bash
set -euo pipefail

if (($# != 3)); then
  echo "usage: $0 JUICEFS_BINARY JUICEFS_SHA256 IMAGE_TAG" >&2
  exit 2
fi
binary=$1 expected=$2 image=$3
[[ -f "$binary" && "$expected" =~ ^[0-9a-f]{64}$ ]] || exit 2
actual=$(sha256sum "$binary" | awk '{print $1}')
[[ "$actual" == "$expected" ]] || { echo "JuiceFS digest mismatch: got $actual" >&2; exit 1; }
context=$(mktemp -d /tmp/gvisor-fuse-image.XXXXXX)
case "$context" in /tmp/gvisor-fuse-image.*) ;; *) exit 1 ;; esac
trap 'rm -rf -- "$context"' EXIT
cp tests/fuse/Dockerfile "$context/Dockerfile"
cp "$binary" "$context/juicefs"
docker build --build-arg "JUICEFS_SHA256=$expected" -t "$image" "$context"
docker image inspect "$image" --format '{{.Id}}'
