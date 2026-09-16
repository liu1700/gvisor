#!/usr/bin/env python3
import argparse
import errno
import fcntl
import hashlib
import json
import mmap
import os
import pathlib
import sqlite3
import subprocess
import sys
import time


def emit(name, ok, **detail):
    print(json.dumps({"case": name, "ok": ok, **detail}, sort_keys=True), flush=True)
    if not ok:
        raise AssertionError(f"{name}: {detail}")


def pattern(size, salt=0):
    return bytes(((i * 31 + 7 + salt) & 0xFF) for i in range(size))


def mmap_matrix(root):
    root = pathlib.Path(root)
    path = root / "matrix.bin"
    original = pattern(3 * 4096 + 37)
    path.write_bytes(original)
    with path.open("r+b", buffering=0) as first, path.open("r+b", buffering=0) as sibling:
        shared = mmap.mmap(first.fileno(), len(original), access=mmap.ACCESS_WRITE)
        emit("mmap-initial", shared[:] == original)
        os.pwrite(sibling.fileno(), b"SIBLING", 5000)
        emit("mmap-sibling-pwrite", shared[5000:5007] == b"SIBLING")
        shared[4095:4097] = b"XY"
        emit("mmap-shared-dirty-pread", os.pread(sibling.fileno(), 2, 4095) == b"XY")
        shared.flush(); os.fsync(first.fileno())
        emit("mmap-shared-pread", os.pread(sibling.fileno(), 2, 4095) == b"XY")
        private = mmap.mmap(first.fileno(), 4096, access=mmap.ACCESS_COPY)
        before = os.pread(sibling.fileno(), 1, 0)
        private[0:1] = bytes([private[0] ^ 0xFF])
        emit("mmap-private-cow", os.pread(sibling.fileno(), 1, 0) == before)

        # Keep one dirty mapped byte while another process updates a disjoint
        # byte through pwrite. Dropping the mapping cache for pwrite must not
        # lose the unrelated dirty byte.
        shared[100] = 0xA1
        writer_code = "import os,sys; f=os.open(sys.argv[1],os.O_RDWR); [os.pwrite(f,bytes([i&255]),200) for i in range(128)]; os.close(f)"
        writer = subprocess.run([sys.executable, "-c", writer_code, str(path)])
        shared.flush(); os.fsync(first.fileno())
        emit("mmap-dirty-vs-concurrent-pwrite",
             writer.returncode == 0 and os.pread(sibling.fileno(), 1, 100) == b"\xa1" and os.pread(sibling.fileno(), 1, 200) == b"\x7f")

        alias = root / "matrix.alias"
        os.link(path, alias)
        with alias.open("r+b", buffering=0) as alias_file:
            alias_map = mmap.mmap(alias_file.fileno(), len(original), access=mmap.ACCESS_WRITE)
            shared[300:306] = b"LINKED"
            emit("mmap-hardlink-shared-page", alias_map[300:306] == b"LINKED")
            alias_map.close()
        private.close()
        shared.close()

    # KEEP_SIZE and range operations must update mapped pages without losing
    # dirty data outside the operated range.
    range_path = root / "ranges.bin"
    range_path.write_bytes(pattern(4 * 4096, 13))
    with range_path.open("r+b", buffering=0) as f:
        mapped = mmap.mmap(f.fileno(), 4 * 4096, access=mmap.ACCESS_WRITE)
        mapped[31:37] = b"PREFIX"
        mapped[-6:] = b"SUFFIX"
        libc = __import__("ctypes").CDLL(None, use_errno=True)
        def fallocate(mode, offset, length):
            __import__("ctypes").set_errno(0)
            rc = libc.fallocate(f.fileno(), mode, offset, length)
            return rc, __import__("ctypes").get_errno()
        before_size = os.fstat(f.fileno()).st_size
        keep_rc, keep_errno = fallocate(1, before_size, 4096)
        emit("fallocate-keep-size", keep_rc == 0 and os.fstat(f.fileno()).st_size == before_size,
             errno=keep_errno)
        punch_rc, punch_errno = fallocate(1 | 2, 4096, 4096)
        zero_rc, zero_errno = fallocate(16, 8192, 4096)
        range_ok = (punch_rc == 0 and zero_rc == 0 and mapped[4096:8192] == bytes(4096)
                    and mapped[8192:12288] == bytes(4096)
                    and mapped[31:37] == b"PREFIX" and mapped[-6:] == b"SUFFIX")
        emit("fallocate-punch-zero-mmap-coherence", range_ok,
             punch_errno=punch_errno, zero_errno=zero_errno)
        mapped.flush(); os.fsync(f.fileno()); mapped.close()

    lifetime_write = root / "dirty-after-close.bin"
    lifetime_write.write_bytes(bytes(8192))
    fd = os.open(lifetime_write, os.O_RDWR)
    dirty_map = mmap.mmap(fd, 8192, access=mmap.ACCESS_WRITE)
    os.close(fd)
    dirty_map[4090:4107] = b"DIRTY-AFTER-CLOSE"
    dirty_map.close()
    with lifetime_write.open("rb", buffering=0) as fresh:
        persisted = os.pread(fresh.fileno(), 17, 4090) == b"DIRTY-AFTER-CLOSE"
    emit("mmap-dirty-after-close-munmap", persisted)

    # Fault page 2, truncate it away, then require a later access to fail in a
    # subprocess. SIGBUS must not terminate the main JSON producer.
    truncate_code = r'''
import mmap, os, sys
p=sys.argv[1]; f=open(p,'r+b',buffering=0); m=mmap.mmap(f.fileno(),8192,access=mmap.ACCESS_READ)
_ = m[4096]; os.ftruncate(f.fileno(),4096); _ = m[4096]
'''
    path.write_bytes(pattern(8192, 3))
    result = subprocess.run([sys.executable, "-c", truncate_code, str(path)])
    emit("mmap-truncate-after-fault-sigbus", result.returncode == -7,
         returncode=result.returncode)

    # Close before first access to page 2. This proves the mapping lifetime and
    # a late page fault do not depend on the original FD.
    lifetime_code = r'''
import mmap, os, sys
p=sys.argv[1]; f=open(p,'rb',buffering=0); m=mmap.mmap(f.fileno(),8192,access=mmap.ACCESS_READ); f.close()
sys.exit(0 if m[4096:4104] == bytes(((i*31+7+9)&255) for i in range(4096,4104)) else 9)
'''
    path.write_bytes(pattern(8192, 9))
    result = subprocess.run([sys.executable, "-c", lifetime_code, str(path)])
    emit("mmap-late-fault-after-close", result.returncode == 0,
         returncode=result.returncode)

    unlinked = root / "unlinked.bin"
    unlinked.write_bytes(pattern(8192, 11))
    unlink_code = r'''
import mmap,os,sys
p=sys.argv[1]; f=open(p,'rb',buffering=0); m=mmap.mmap(f.fileno(),8192,access=mmap.ACCESS_READ)
os.unlink(p); f.close(); want=bytes(((i*31+7+11)&255) for i in range(4096,4104)); sys.exit(0 if m[4096:4104]==want else 7)
'''
    result = subprocess.run([sys.executable, "-c", unlink_code, str(unlinked)])
    emit("mmap-shared-after-close-unlink", result.returncode == 0, returncode=result.returncode)

    pressure_ok = True
    for _ in range(256):
        with path.open("rb", buffering=0) as f:
            transient = mmap.mmap(f.fileno(), 4096, access=mmap.ACCESS_READ)
            pressure_ok = pressure_ok and transient[0] == pattern(1, 9)[0]
            transient.close()
    emit("mmap-fdref-pressure", pressure_ok)

    with path.open("r+b", buffering=0) as f:
        os.ftruncate(f.fileno(), 12288)
        os.pwrite(f.fileno(), b"HOLE-END", 12280)
        emit("truncate-extend-hole-zero", os.pread(f.fileno(), 32, 8192)[:24] == bytes(24))

    with path.open("rb", buffering=0) as f:
        os.set_inheritable(f.fileno(), True)
        code = "import mmap,sys; m=mmap.mmap(int(sys.argv[1]),4096,access=mmap.ACCESS_READ); sys.exit(0 if len(m)==4096 and m[0]>=0 else 8)"
        result = subprocess.run([sys.executable, "-c", code, str(f.fileno())], pass_fds=(f.fileno(),))
        emit("mmap-inherited-fd-exec", result.returncode == 0, returncode=result.returncode)

    expected = hashlib.sha256(path.read_bytes()).hexdigest()
    lifetime_expected = hashlib.sha256(lifetime_write.read_bytes()).hexdigest()
    pathlib.Path(os.environ.get("FUSE_TEST_DATA", "/data"), "matrix.expected").write_text(
        f"{path.name} {path.stat().st_size} {expected}\n"
        f"{lifetime_write.name} {lifetime_write.stat().st_size} {lifetime_expected}\n")


def lock_matrix(root):
    path = pathlib.Path(root) / "lock"
    path.touch()
    with path.open("r+") as first, path.open("r+") as second:
        fcntl.flock(first, fcntl.LOCK_EX | fcntl.LOCK_NB)
        try:
            fcntl.flock(second, fcntl.LOCK_EX | fcntl.LOCK_NB)
            blocked = False
        except BlockingIOError as exc:
            blocked = exc.errno in (errno.EAGAIN, errno.EWOULDBLOCK)
        emit("flock-contention", blocked)
        fcntl.flock(first, fcntl.LOCK_UN)
        fcntl.flock(second, fcntl.LOCK_EX | fcntl.LOCK_NB)
        fcntl.flock(second, fcntl.LOCK_UN)

        fcntl.lockf(first, fcntl.LOCK_EX | fcntl.LOCK_NB)
        try:
            fcntl.lockf(second, fcntl.LOCK_EX | fcntl.LOCK_NB)
            # POSIX locks are process-owned, so two FDs in one process do not
            # contend. Record that Linux rule and test inter-process below.
        finally:
            fcntl.lockf(first, fcntl.LOCK_UN)
    child = subprocess.Popen([sys.executable, "-c", """
import fcntl,sys,time
f=open(sys.argv[1],'r+'); fcntl.lockf(f,fcntl.LOCK_EX); print('held',flush=True); time.sleep(1)
""", str(path)], stdout=subprocess.PIPE, text=True)
    assert child.stdout.readline().strip() == "held"
    with path.open("r+") as contender:
        try:
            fcntl.lockf(contender, fcntl.LOCK_EX | fcntl.LOCK_NB)
            blocked = False
        except BlockingIOError as exc:
            blocked = exc.errno in (errno.EAGAIN, errno.EACCES)
    emit("fcntl-lock-contention", blocked)
    child.wait(timeout=5)
    # Verify acquisition is possible after release.
    with path.open("r+") as f:
        fcntl.lockf(f, fcntl.LOCK_EX | fcntl.LOCK_NB)
        fcntl.lockf(f, fcntl.LOCK_UN)
    emit("fcntl-lock-release", child.returncode == 0)

    alias = pathlib.Path(root) / "lock.alias"
    os.link(path, alias)
    child = subprocess.Popen([sys.executable, "-c", """
import fcntl,sys,time
f=open(sys.argv[1],'r+'); fcntl.flock(f,fcntl.LOCK_EX); print('held',flush=True); time.sleep(1)
""", str(path)], stdout=subprocess.PIPE, text=True)
    assert child.stdout.readline().strip() == "held"
    with alias.open("r+") as contender:
        try:
            fcntl.flock(contender, fcntl.LOCK_EX | fcntl.LOCK_NB)
            alias_blocked = False
        except BlockingIOError as exc:
            alias_blocked = exc.errno in (errno.EAGAIN, errno.EWOULDBLOCK)
    child.wait(timeout=5)
    emit("hardlink-alias-lock-contention", alias_blocked and child.returncode == 0)


def sqlite_matrix(root):
    path = pathlib.Path(root) / "matrix.db"
    con = sqlite3.connect(path)
    con.execute("pragma journal_mode=wal")
    con.execute("create table if not exists t (id integer primary key, value text)")
    con.executemany("insert into t(value) values (?)", [(f"value-{i}",) for i in range(1000)])
    con.commit()
    result = con.execute("pragma integrity_check").fetchone()[0]
    count = con.execute("select count(*) from t").fetchone()[0]
    con.close()
    emit("sqlite-wal-integrity", result == "ok" and count == 1000,
         integrity=result, rows=count)
    writer = """import sqlite3,sys,time
c=sqlite3.connect(sys.argv[1],timeout=5); c.execute('begin immediate'); c.execute('insert into t(value) values (?)',(sys.argv[2],)); time.sleep(.3); c.commit()"""
    a = subprocess.Popen([sys.executable, "-c", writer, str(path), "writer-a"])
    time.sleep(.05)
    b = subprocess.run([sys.executable, "-c", writer, str(path), "writer-b"])
    a.wait(timeout=5)
    con = sqlite3.connect(path); writers = con.execute("select count(*) from t where value like 'writer-%'").fetchone()[0]; con.close()
    emit("sqlite-two-writer-contention", a.returncode == 0 and b.returncode == 0 and writers == 2, rows=writers)


def run_git(root):
    root = pathlib.Path(root)
    src, clone = root / "repo", root / "clone"
    src.mkdir()
    subprocess.run(["git", "init", "-q", str(src)], check=True)
    subprocess.run(["git", "-C", str(src), "config", "user.email", "fuse@example.invalid"], check=True)
    subprocess.run(["git", "-C", str(src), "config", "user.name", "FUSE Test"], check=True)
    for i in range(2000):
        (src / f"file-{i:04d}").write_bytes(pattern(257, i))
    subprocess.run(["git", "-C", str(src), "add", "."], check=True)
    subprocess.run(["git", "-C", str(src), "commit", "-qm", "packed fixture"], check=True)
    subprocess.run(["git", "-C", str(src), "gc", "--prune=now"], check=True)
    subprocess.run(["git", "-C", str(src), "fsck", "--full"], check=True)
    subprocess.run(["git", "clone", "--no-local", "-q", str(src), str(clone)], check=True)
    subprocess.run(["git", "-C", str(clone), "fsck", "--full"], check=True)
    a = subprocess.check_output(["git", "-C", str(src), "rev-parse", "HEAD"], text=True).strip()
    b = subprocess.check_output(["git", "-C", str(clone), "rev-parse", "HEAD"], text=True).strip()
    emit("git-packed-clone", a == b, head=a)
    (src / "file-0000").write_text("modified\n")
    subprocess.run(["git", "-C", str(src), "add", "."], check=True)
    subprocess.run(["git", "-C", str(src), "commit", "-qm", "modify"], check=True)
    subprocess.run(["git", "-C", str(src), "repack", "-ad"], check=True)
    subprocess.run(["git", "-C", str(src), "fsck", "--full"], check=True)
    subprocess.run(["git", "-C", str(src), "status", "--porcelain"], check=True, stdout=subprocess.PIPE)
    subprocess.run(["git", "-C", str(src), "checkout", "HEAD~1", "--", "file-0000"], check=True)
    emit("git-read-modify-repack", (src / "file-0000").read_bytes() == pattern(257, 0))


def verify(root):
    root = pathlib.Path(root)
    db = root / "matrix.db"
    checked = 0
    detail = {}
    if db.exists():
        con = sqlite3.connect(f"file:{db}?mode=ro", uri=True)
        detail["integrity"] = con.execute("pragma integrity_check").fetchone()[0]
        detail["rows"] = con.execute("select count(*) from t").fetchone()[0]
        con.close()
        checked += 1
    if (root / "repo").exists():
        subprocess.run(["git", "-C", str(root / "repo"), "fsck", "--full"], check=True)
        detail["git"] = "ok"
        checked += 1
    expected_path = pathlib.Path(os.environ.get("FUSE_TEST_DATA", "/data"), "matrix.expected")
    if expected_path.exists():
        persisted = {}
        for line in expected_path.read_text().splitlines():
            name, size, digest = line.split()
            payload = root / name
            actual = hashlib.sha256(payload.read_bytes()).hexdigest()
            persisted[name] = {"size": payload.stat().st_size, "expected_size": int(size),
                               "sha256": actual, "expected_sha256": digest}
        detail["persisted"] = persisted
        checked += len(persisted)
    ok = checked > 0
    if "integrity" in detail:
        ok = ok and detail["integrity"] == "ok" and detail["rows"] == 1002
    if "persisted" in detail:
        ok = ok and all(item["sha256"] == item["expected_sha256"] and
                        item["size"] == item["expected_size"] for item in detail["persisted"].values())
    emit("remount-persistence", ok, checked=checked, **detail)


def control(root):
    root = pathlib.Path(root)
    detail = {}
    for name in (".stats", ".accesslog"):
        code = "import os,sys; f=os.open(sys.argv[1],os.O_RDONLY); print(len(os.read(f,4096))); os.close(f)"
        try:
            if name == ".accesslog":
                reader = subprocess.Popen([sys.executable, "-c", code, str(root / name)], stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
                time.sleep(0.1)
                (root / "accesslog-trigger").write_bytes(b"trigger")
                os.stat(root / "accesslog-trigger")
                stdout, _ = reader.communicate(timeout=2)
                result_returncode, result_stdout = reader.returncode, stdout
            else:
                result = subprocess.run([sys.executable, "-c", code, str(root / name)], capture_output=True, text=True, timeout=2)
                result_returncode, result_stdout = result.returncode, result.stdout
            detail[name] = int(result_stdout.strip()) if result_returncode == 0 else {"returncode": result_returncode}
        except subprocess.TimeoutExpired:
            if name == ".accesslog":
                reader.kill(); reader.wait()
            detail[name] = {"timeout": True}
    emit("juicefs-control-files", all(isinstance(v, int) and v > 0 for v in detail.values()),
         observed=detail)


def gofer(root):
    root = pathlib.Path(root); root.mkdir(parents=True, exist_ok=True)
    src, absent, existing = root / "rename-src", root / "rename-absent", root / "rename-existing"
    src.write_bytes(b"source"); existing.write_bytes(b"existing")
    libc = __import__("ctypes").CDLL(None, use_errno=True)
    renameat2 = libc.renameat2; renameat2.argtypes = [__import__("ctypes").c_int, __import__("ctypes").c_char_p, __import__("ctypes").c_int, __import__("ctypes").c_char_p, __import__("ctypes").c_uint]
    at_fdcwd, noreplace = -100, 1
    rc1 = renameat2(at_fdcwd, os.fsencode(src), at_fdcwd, os.fsencode(absent), noreplace)
    src.write_bytes(b"second")
    __import__("ctypes").set_errno(0); rc2 = renameat2(at_fdcwd, os.fsencode(src), at_fdcwd, os.fsencode(existing), noreplace); err2 = __import__("ctypes").get_errno()
    emit("gofer-rename-noreplace", rc1 == 0 and absent.read_bytes() == b"source" and rc2 == -1 and err2 == errno.EEXIST and existing.read_bytes() == b"existing", errno=err2)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("case", choices=("matrix", "git", "sqlite", "locks", "verify", "control", "gofer"))
    parser.add_argument("root")
    args = parser.parse_args()
    started = time.monotonic()
    {"matrix": mmap_matrix, "git": run_git, "sqlite": sqlite_matrix,
     "locks": lock_matrix, "verify": verify, "control": control, "gofer": gofer}[args.case](args.root)
    emit(f"{args.case}-complete", True, elapsed_ms=int((time.monotonic() - started) * 1000))


if __name__ == "__main__":
    main()
