#!/bin/bash
set -euo pipefail

usage() {
  echo "usage: $0 --runsc PATH --rootfs-tar PATH --case diagnostic|mmap|git|control|all --output PATH" >&2
  exit 2
}

runsc= rootfs_tar= image=${FUSE_TEST_IMAGE:-plori-gvisor-fuse-probe:local} selected=all output=
while (($#)); do
  case "$1" in
    --runsc) runsc=$2; shift 2 ;;
    --rootfs-tar) rootfs_tar=$2; shift 2 ;;
    --case) selected=$2; shift 2 ;;
    --output) output=$2; shift 2 ;;
    *) usage ;;
  esac
done
[[ -x "$runsc" && -f "$rootfs_tar" && -n "$output" ]] || usage
case "$selected" in diagnostic|mmap|git|control|all) ;; *) usage ;; esac

base=${FUSE_TEST_TMPDIR:-/tmp/gvisor-fuse-runs}
mkdir -p "$base" "$(dirname "$output")"
run_dir=$(mktemp -d "$base/run.XXXXXX")
bundle=$run_dir/bundle
rootfs=$bundle/rootfs
fixture=$run_dir/fixture
state=$run_dir/runsc-root
container_id=fuse-compat-$(basename "$run_dir")
mkdir -p "$rootfs" "$fixture" "$state"
case "$run_dir" in "$base"/run.*) ;; *) echo "unsafe run directory: $run_dir" >&2; exit 1 ;; esac
elevate=()
if ((EUID != 0)); then
  sudo -n true >/dev/null 2>&1 || { echo "root or passwordless sudo is required for runsc" >&2; exit 1; }
  elevate=(sudo -n)
fi

cleanup() {
  local rc=$?
  local safe_to_remove=1 null_netns="$state/null-netns" owned_mounts
  timeout 15s "${elevate[@]}" "$runsc" --root="$state" delete --force "$container_id" >/dev/null 2>&1 || true
  local i residue=0
  for ((i=0; i<50; i++)); do
    if ! ps -eo comm=,args= | awk -v root="$state" '$1 ~ /runsc/ && index($0, root) { found=1 } END { exit found ? 0 : 1 }'; then
      residue=0
      break
    fi
    residue=1
    sleep 0.1
  done
  if ((residue)); then
    echo "runsc process remains for private root: $state" >&2
    rc=1
  fi
  if findmnt -rn --mountpoint "$null_netns" >/dev/null 2>&1; then
    if ! "${elevate[@]}" umount -- "$null_netns"; then
      echo "failed to unmount private runsc network namespace: $null_netns" >&2
      rc=1
    fi
  fi
  owned_mounts=$(findmnt -rn -o TARGET | awk -v root="$state" '$0 == root || index($0, root "/") == 1')
  if [[ -n "$owned_mounts" ]]; then
    echo "mount remains under private runsc root: $owned_mounts" >&2
    rc=1
    safe_to_remove=0
  fi
  if ((safe_to_remove)) && { ((rc == 0)) || [[ ${FUSE_TEST_PRESERVE_FAILURE:-0} != 1 ]]; }; then
    "${elevate[@]}" rm -rf -- "$run_dir"
  else
    echo "preserved_failed_run=$run_dir" >&2
  fi
  exit "$rc"
}
trap cleanup EXIT INT TERM

"${elevate[@]}" tar -xf "$rootfs_tar" -C "$rootfs"
"${elevate[@]}" mkdir -p "$rootfs/opt/fuse-tests" "$rootfs/mnt/fuse-test" "$rootfs/data"
"${elevate[@]}" cp tests/fuse/fake_fuse.c tests/fuse/syscall_probe.c tests/fuse/compat.py tests/fuse/workload.sh "$rootfs/opt/fuse-tests/"

# Compile in the exact rootfs used for native and runsc execution. This avoids
# carrying a host libfuse SONAME into a different distribution.
docker run --rm --network=none -v "$rootfs:/rootfs" -w /rootfs/opt/fuse-tests \
  "$image" /bin/bash -ceu '
    gcc -O2 -Wall -Wextra -Werror $(pkg-config --cflags fuse3) fake_fuse.c -o fake-fuse $(pkg-config --libs fuse3)
    gcc -O2 -Wall -Wextra -Werror syscall_probe.c -o syscall-probe
    chmod 755 fake-fuse syscall-probe compat.py workload.sh
  '

python3 - "$bundle/config.json" "$fixture" "$selected" <<'PY'
import json, os, sys
out, fixture, selected = sys.argv[1:]
caps = ["CAP_CHOWN", "CAP_DAC_OVERRIDE", "CAP_FOWNER", "CAP_KILL", "CAP_MKNOD", "CAP_SETGID", "CAP_SETUID", "CAP_SYS_ADMIN"]
config = {
  "ociVersion": "1.0.0",
  "process": {"terminal": False, "user": {"uid": 0, "gid": 0},
    "args": ["/bin/bash", "/opt/fuse-tests/workload.sh"],
    "env": ["PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", f"FUSE_TEST_CASE={selected}"],
    "cwd": "/", "capabilities": {k: caps for k in ("bounding", "effective", "inheritable", "permitted")},
    "rlimits": [{"type": "RLIMIT_NOFILE", "hard": 8192, "soft": 8192}]},
  "root": {"path": "rootfs", "readonly": False}, "hostname": "fuse-compat",
  "mounts": [
    {"destination": "/proc", "type": "proc", "source": "proc"},
    {"destination": "/dev", "type": "tmpfs", "source": "tmpfs", "options": ["nosuid", "strictatime", "mode=755", "size=65536k"]},
    {"destination": "/data", "type": "bind", "source": os.path.realpath(fixture), "options": ["rbind", "rw"]}],
  "linux": {"namespaces": [{"type": x} for x in ("pid", "ipc", "uts", "mount", "network")],
    "devices": [{"path": "/dev/fuse", "type": "c", "major": 10, "minor": 229, "fileMode": 0o666, "uid": 0, "gid": 0}]}}
with open(out, "w") as f: json.dump(config, f, indent=2)
PY

{
  printf '{"runner":"metadata","case":"%s","runsc_sha256":"%s","rootfs_sha256":"%s"}\n' \
    "$selected" "$(sha256sum "$runsc" | awk '{print $1}')" "$(sha256sum "$rootfs_tar" | awk '{print $1}')"
  timeout --signal=TERM --kill-after=15s 10m "${elevate[@]}" "$runsc" --root="$state" \
    --network=none --platform=systrap --overlay2=none --ignore-cgroups=true \
    run --bundle="$bundle" "$container_id"
} >"$output" 2>&1

printf 'output=%s\n' "$output"
