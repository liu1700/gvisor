# In-sandbox FUSE compatibility tests

These tests exercise an in-sandbox FUSE daemon through an explicitly selected
`runsc` binary. They do not install a runtime or use Kubernetes.

The `diagnostic` case mounts a small libfuse low-level server and verifies exact
FSYNC behavior: delayed replies block the syscall, EIO propagates, fdatasync
sets `FUSE_FSYNC_FDATASYNC`, directory fsync emits `FUSE_FSYNCDIR`, and ENOSYS
uses Linux's successful no-fsync fallback. The server emits the observed
opcodes as JSON.

The other cases mount a fresh file-backed JuiceFS volume and exercise mmap,
coherence, EOF/truncate, mapping lifetime, exec inheritance, private/shared
maps, msync/fsync, locks, packed Git objects, SQLite WAL, control files, and a
cold-cache remount. Correctness failures produce a nonzero exit.

```sh
tests/fuse/run.sh \
  --runsc /tmp/gvisor-fuse-work/baseline-release/runsc \
  --rootfs-tar /tmp/gvisor-fuse-work/rootfs.tar \
  --case diagnostic \
  --output /tmp/gvisor-fuse-work/baseline-fsync.jsonl
```

Build the image and rootfs from an explicitly supplied JuiceFS binary. The
build refuses a mismatched SHA-256:

```sh
tests/fuse/build-image.sh /path/to/juicefs SHA256 plori-gvisor-fuse-probe:local
cid=$(docker create plori-gvisor-fuse-probe:local)
docker export "$cid" > /tmp/gvisor-fuse-rootfs.tar
docker rm "$cid"
```

Run native Linux first with the same JuiceFS binary:

```sh
tests/fuse/native.sh /path/to/juicefs all /tmp/fuse-native.jsonl
```

Use the same rootfs tar for native, baseline, and candidate comparisons. The
runner compiles the C fixtures inside `plori-gvisor-fuse-probe:local`, gives
each run a private OCI bundle and `runsc --root`, uses gVisor's virtual
character device at major 10 minor 229, and persists `/data` in the run's owned
fixture directory. It does not pass the host `/dev/fuse` into the sandbox.

The workload records its JuiceFS PID. Cleanup sends TERM, waits at most five
seconds, then sends KILL. This handles the known baseline behavior where an
in-sandbox JuiceFS process can survive unmount.
