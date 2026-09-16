# FUSE compatibility tests

Run these tests before publishing a Plori `runsc` release. They check a FUSE
daemon inside the sandbox, using gVisor's virtual `/dev/fuse` device.

## Run the release checks

Use a Linux x86_64 host with Docker and passwordless sudo. Install the Go
version declared in `go.mod`. Install GCC,
libfuse3 development files, pkg-config, Python 3, Git, SQLite, curl, and tar.

1. Build the committed source:

   ```sh
   tools/plori-release-build.sh candidate-local
   ```

2. Run the native Linux control and sandbox tests:

   ```sh
   tests/fuse/qualify.sh out/runsc /tmp/fuse-results
   ```

3. Read `native.jsonl` and `candidate.jsonl` in the results directory.
   A failed assertion makes the command return a nonzero status.

The qualification script downloads JuiceFS `v1.5.0-plori.29` and checks its
pinned SHA-256. Both runs use that binary. The native run uses host libraries.
The sandbox run uses the Debian image built by `tests/fuse/build-image.sh`.

## What the tests check

The small libfuse server records protocol requests. It checks delayed FSYNC
replies, EIO propagation, FDATASYNC flags, FSYNCDIR, and the ENOSYS fallback.
It also checks 256 KiB writes, dynamic DirectIO reads, daemon disconnect,
and process exit after unmount.

A fresh JuiceFS volume exercises shared and private mappings, executable
pages, EOF handling, truncation, hard-link coherence, and mapping lifetime.
The workload also checks concurrent writes, allocation modes, file locks,
SQLite WAL, and Git pack files. After unmount, it removes the client cache
and checks file hashes, Git objects, and database integrity on a new mount.

These checks cover the tested filesystem operations. They do not establish
complete Linux compatibility or cover checkpoint and restore.

## Compare runtime versions

Supply the same rootfs archive to each sandbox run. Keep `gvisor-bin/` beside
the selected `runsc` binary.

```sh
FUSE_TEST_IMAGE=plori-gvisor-fuse-probe:local tests/fuse/run.sh \
  --runsc /path/to/release/runsc \
  --rootfs-tar /path/to/rootfs.tar \
  --case all \
  --output /tmp/fuse-results/release.jsonl
```

The runner uses a private OCI bundle and runtime state directory. It removes
its container and network namespace mount after the run. Set
`FUSE_TEST_PRESERVE_FAILURE=1` to retain a failed run's files for inspection.
Cleanup records whether the daemon exits after unmount or requires a signal.
The clean-unmount assertion rejects signal fallback.
