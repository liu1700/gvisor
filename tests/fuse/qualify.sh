#!/usr/bin/env bash
# Run the native control and the selected release binary against disposable data.
set -euo pipefail
runsc=$(realpath "${1:?usage: tests/fuse/qualify.sh RUNSC OUTPUT_DIR}")
output=$(realpath -m "${2:?output directory required}")
mkdir -p "$output"
work=$(mktemp -d /tmp/gvisor-fuse-qualification.XXXXXX)
cid=
image=
cleanup() {
  if [[ -n "$cid" ]]; then docker rm -f "$cid" >/dev/null; fi
  if [[ -n "$image" ]]; then docker image rm "$image" >/dev/null || true; fi
  rm -rf -- "$work"
}
trap cleanup EXIT
archive=juicefs-1.5.0-plori.29-linux-amd64.tar.gz
curl --fail --location --retry 3 --max-time 120 \
  "https://github.com/liu1700/juicefs/releases/download/v1.5.0-plori.29/$archive" -o "$work/$archive"
printf '%s  %s\n' 683bc014345a6ab541c8dc71011d704e83e2b0dc62752e464e1d51f0b15dadc2 "$work/$archive" | sha256sum -c -
tar -xzf "$work/$archive" -C "$work"
juicefs="$work/juicefs-1.5.0-plori.29-linux-amd64"
image="plori-gvisor-fuse-qualification:$(basename "$work" | tr '[:upper:]' '[:lower:]')"
tests/fuse/build-image.sh "$juicefs" 887bc8b817db6fc587278f6b5e443b95ca08aa3855bdd0ce8c02dc06f540b8ef "$image"
cid=$(docker create "$image")
docker export "$cid" > "$work/rootfs.tar"
docker rm "$cid" >/dev/null
cid=
tests/fuse/native.sh "$juicefs" all "$output/native.jsonl"
FUSE_TEST_IMAGE="$image" tests/fuse/run.sh --runsc "$runsc" \
  --rootfs-tar "$work/rootfs.tar" --case all --output "$output/candidate.jsonl"
