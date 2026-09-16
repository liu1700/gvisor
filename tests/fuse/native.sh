#!/bin/bash
set -euo pipefail

if (($# != 3)); then
  echo "usage: $0 JUICEFS_BINARY diagnostic|mmap|correctness|control|all OUTPUT" >&2
  exit 2
fi
juicefs=$1 selected=$2 output=$3
[[ -x "$juicefs" && "$selected" =~ ^(diagnostic|mmap|correctness|control|all)$ ]] || exit 2
base=${FUSE_TEST_TMPDIR:-/tmp/gvisor-fuse-native}
mkdir -p "$base" "$(dirname "$output")"
run_dir=$(mktemp -d "$base/run.XXXXXX")
case "$run_dir" in "$base"/run.*) ;; *) exit 1 ;; esac
elevate=()
if ((EUID != 0)); then sudo -n true >/dev/null 2>&1 || exit 1; elevate=(sudo -n); fi
cleanup() {
  local rc=$?
  if ((rc == 0)) || [[ ${FUSE_TEST_PRESERVE_FAILURE:-1} != 1 ]]; then
    "${elevate[@]}" rm -rf -- "$run_dir"
  else
    echo "preserved_failed_run=$run_dir" >&2
  fi
  exit "$rc"
}
trap cleanup EXIT INT TERM
mkdir -p "$run_dir/bin" "$run_dir/data" "$run_dir/mnt"
gcc -O2 -Wall -Wextra -Werror $(pkg-config --cflags fuse3) tests/fuse/fake_fuse.c -o "$run_dir/bin/fake-fuse" $(pkg-config --libs fuse3)
gcc -O2 -Wall -Wextra -Werror tests/fuse/syscall_probe.c -o "$run_dir/bin/syscall-probe"
cp tests/fuse/compat.py tests/fuse/workload.sh "$run_dir/bin/"
ln -s "$juicefs" "$run_dir/bin/juicefs"
timeout --signal=TERM --kill-after=15s 10m "${elevate[@]}" env \
  PATH="$run_dir/bin:$PATH" FUSE_TEST_CASE="$selected" FUSE_TEST_DATA="$run_dir/data" \
  FUSE_TEST_MOUNTPOINT="$run_dir/mnt" FUSE_TEST_BIN_DIR="$run_dir/bin" \
  bash "$run_dir/bin/workload.sh" >"$output" 2>&1
