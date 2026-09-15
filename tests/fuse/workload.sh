#!/bin/bash
set -euo pipefail

case_name=${FUSE_TEST_CASE:-all}
data=${FUSE_TEST_DATA:-/data}
mountpoint=${FUSE_TEST_MOUNTPOINT:-/mnt/fuse-test}
bin_dir=${FUSE_TEST_BIN_DIR:-/opt/fuse-tests}
mkdir -p "$data" "$mountpoint" "$data/cache" "$data/bucket"

json() {
  python3 -c 'import json,sys; print(json.dumps(dict(zip(sys.argv[1::2],sys.argv[2::2]))))' "$@"
}

mounted() {
  awk -v mp="$mountpoint" '$5 == mp { for (i=6; i<=NF; i++) if ($i == "-" && $(i+1) ~ /^fuse/) found=1 } END { exit !found }' /proc/self/mountinfo
}

wait_mounted() {
  local pid=$1 i
  for ((i=0; i<300; i++)); do
    mounted && return 0
    kill -0 "$pid" 2>/dev/null || return 1
    sleep 0.1
  done
  return 1
}

stop_owned_mount() {
  local pid=$1 label=$2 i signal=TERM
  fusermount3 -u "$mountpoint" >"$data/${label}-unmount.log" 2>&1 || true
  kill -TERM "$pid" 2>/dev/null || true
  for ((i=0; i<50; i++)); do
    ! kill -0 "$pid" 2>/dev/null && { wait "$pid" 2>/dev/null || true; mounted && { printf '{"case":"cleanup","ok":false,"label":"%s","error":"mounted-after-exit"}\n' "$label"; return 1; }; printf '{"case":"cleanup","ok":true,"label":"%s","signal":"%s"}\n' "$label" "$signal"; return; }
    sleep 0.1
  done
  signal=KILL
  kill -KILL "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
  if kill -0 "$pid" 2>/dev/null || mounted; then
    printf '{"case":"cleanup","ok":false,"label":"%s","signal":"%s"}\n' "$label" "$signal"
    return 1
  fi
  printf '{"case":"cleanup","ok":true,"label":"%s","signal":"%s"}\n' "$label" "$signal"
}

run_fake() {
  rm -rf "$mountpoint"/*
  "$bin_dir/fake-fuse" "$mountpoint" 300 >"$data/fake-server.jsonl" 2>"$data/fake-server.err" &
  mount_pid=$!
  mount_label=fake
  trap 'stop_owned_mount "$mount_pid" "$mount_label"' EXIT
  if ! wait_mounted "$mount_pid"; then
    cat "$data/fake-server.err" >&2
    return 1
  fi
  local failures=0
  "$bin_dir/syscall-probe" fake "$mountpoint" || failures=1
  "$bin_dir/syscall-probe" fsync "$mountpoint" 300 || failures=1
  stop_owned_mount "$mount_pid" fake
  trap - EXIT
  cat "$data/fake-server.jsonl"
  return "$failures"
}

start_juicefs() {
  local label=$1
  juicefs mount sqlite3:///$data/meta.db "$mountpoint" \
    --cache-dir "$data/cache" --no-usage-report --no-syslog --no-bgjob \
    >"$data/${label}-mount.log" 2>&1 &
  mount_pid=$!
  mount_label=$label
  trap 'stop_owned_mount "$mount_pid" "$mount_label"' EXIT
  if ! wait_mounted "$mount_pid"; then
    cat "$data/${label}-mount.log" >&2
    return 1
  fi
}

run_juicefs() {
  rm -f "$data/meta.db" "$data/meta.db-shm" "$data/meta.db-wal"
  rm -rf "$data/bucket" "$data/cache" "$data/persist"
  mkdir -p "$data/bucket" "$data/cache" "$data/persist"
  juicefs format --storage file --bucket "$data/bucket/" --trash-days 0 \
    sqlite3:///$data/meta.db compat >"$data/format.log" 2>&1

  local failures=0
  start_juicefs first
  case "$case_name" in
    mmap) "$bin_dir/syscall-probe" mmap "$mountpoint" || failures=1; python3 "$bin_dir/compat.py" matrix "$mountpoint" || failures=1 ;;
    git) python3 "$bin_dir/compat.py" git "$mountpoint" || failures=1 ;;
    control) python3 "$bin_dir/compat.py" control "$mountpoint" || failures=1 ;;
    all)
      python3 "$bin_dir/compat.py" gofer "$data/gofer-control" || failures=1
      "$bin_dir/syscall-probe" mmap "$mountpoint" || failures=1
      python3 "$bin_dir/compat.py" matrix "$mountpoint" || failures=1
      python3 "$bin_dir/compat.py" locks "$mountpoint" || failures=1
      python3 "$bin_dir/compat.py" sqlite "$mountpoint" || failures=1
      python3 "$bin_dir/compat.py" git "$mountpoint" || failures=1
      python3 "$bin_dir/compat.py" control "$mountpoint" || failures=1
      ;;
    *) json case workload ok false error "unknown case: $case_name"; return 2 ;;
  esac
  stop_owned_mount "$mount_pid" first
  trap - EXIT

  if [[ "$case_name" == all || "$case_name" == git ]]; then
    rm -rf "$data/cache"/*
    start_juicefs second
    python3 "$bin_dir/compat.py" verify "$mountpoint" || failures=1
    stop_owned_mount "$mount_pid" second
    trap - EXIT
  fi
  return "$failures"
}

run_all() {
  local failures=0
  run_fake || failures=1
  case_name=all
  run_juicefs || failures=1
  return "$failures"
}

mount_pid=
mount_label=

case "$case_name" in
  diagnostic) run_fake ;;
  all) run_all ;;
  mmap|git|control) run_juicefs ;;
  *) json case workload ok false error "unsupported case: $case_name"; exit 2 ;;
esac
printf '{"case":"workload-complete","ok":true,"selected":"%s"}\n' "$case_name"
