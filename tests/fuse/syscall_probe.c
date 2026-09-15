#define _GNU_SOURCE

#include <errno.h>
#include <fcntl.h>
#include <setjmp.h>
#include <signal.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/file.h>
#include <sys/mman.h>
#include <sys/stat.h>
#include <sys/types.h>
#include <sys/wait.h>
#include <time.h>
#include <unistd.h>

static sigjmp_buf sigbus_env;
static volatile sig_atomic_t saw_sigbus;

static int64_t monotonic_ms(void) {
  struct timespec ts;
  if (clock_gettime(CLOCK_MONOTONIC, &ts) != 0) return -1;
  return (int64_t)ts.tv_sec * 1000 + ts.tv_nsec / 1000000;
}

static void sigbus_handler(int sig) {
  (void)sig;
  saw_sigbus = 1;
  siglongjmp(sigbus_env, 1);
}

static int fsync_case(const char *path, int want_errno, int min_delay_ms) {
  int fd = open(path, O_RDWR);
  if (fd < 0) return 10;
  int64_t start = monotonic_ms();
  errno = 0;
  int rc = fsync(fd);
  int got_errno = errno;
  int64_t elapsed = monotonic_ms() - start;
  close(fd);
  int ok = elapsed >= min_delay_ms && ((want_errno == 0 && rc == 0) ||
      (want_errno != 0 && rc == -1 && got_errno == want_errno));
  printf("{\"case\":\"fsync-%s\",\"ok\":%s,\"elapsed_ms\":%lld,\"rc\":%d,\"errno\":%d}\n",
         want_errno ? "eio" : "delayed", ok ? "true" : "false",
         (long long)elapsed, rc, got_errno);
  return ok ? 0 : 11;
}

static int sync_call_case(const char *name, const char *path, int directory,
                          int data_only, int want_errno, int min_delay_ms) {
  int fd = open(path, directory ? (O_RDONLY | O_DIRECTORY) : O_RDWR);
  if (fd < 0) return 12;
  int64_t start = monotonic_ms();
  errno = 0;
  int rc = data_only ? fdatasync(fd) : fsync(fd);
  int got_errno = errno;
  int64_t elapsed = monotonic_ms() - start;
  close(fd);
  int ok = elapsed >= min_delay_ms && ((want_errno == 0 && rc == 0) ||
      (want_errno != 0 && rc == -1 && got_errno == want_errno));
  printf("{\"case\":\"%s\",\"ok\":%s,\"elapsed_ms\":%lld,\"rc\":%d,\"errno\":%d}\n",
         name, ok ? "true" : "false", (long long)elapsed, rc, got_errno);
  return ok ? 0 : 13;
}

static int mmap_case(const char *root) {
  char path[4096];
  snprintf(path, sizeof(path), "%s/mmap.bin", root);
  int fd = open(path, O_CREAT | O_TRUNC | O_RDWR, 0644);
  if (fd < 0 || ftruncate(fd, 8192) != 0) return 20;
  unsigned char pattern[8192];
  for (size_t i = 0; i < sizeof(pattern); ++i) pattern[i] = (unsigned char)(i * 31u + 7u);
  if (pwrite(fd, pattern, sizeof(pattern), 0) != (ssize_t)sizeof(pattern)) return 21;

  unsigned char *shared = mmap(NULL, sizeof(pattern), PROT_READ | PROT_WRITE, MAP_SHARED, fd, 0);
  if (shared == MAP_FAILED || memcmp(shared, pattern, sizeof(pattern)) != 0) return 22;
  shared[4095] ^= 0x5a;
  shared[4096] ^= 0xa5;
  if (msync(shared, sizeof(pattern), MS_SYNC) != 0 || fsync(fd) != 0) return 23;
  unsigned char pair[2];
  if (pread(fd, pair, 2, 4095) != 2 || pair[0] != shared[4095] || pair[1] != shared[4096]) return 24;
  if (shared[4094] != pattern[4094] || shared[4097] != pattern[4097]) return 31;

  unsigned char *priv = mmap(NULL, 4096, PROT_READ | PROT_WRITE, MAP_PRIVATE, fd, 0);
  if (priv == MAP_FAILED) return 25;
  unsigned char disk0 = shared[0];
  priv[0] ^= 0xff;
  unsigned char reread = 0;
  if (pread(fd, &reread, 1, 0) != 1 || reread != disk0) return 26;

  if (close(fd) != 0 || shared[1] != pattern[1]) return 27;
  munmap(priv, 4096);
  munmap(shared, sizeof(pattern));

  fd = open(path, O_RDWR);
  if (fd < 0 || ftruncate(fd, 4096) != 0) return 28;
  unsigned char *shrunk = mmap(NULL, 8192, PROT_READ, MAP_SHARED, fd, 0);
  if (shrunk == MAP_FAILED) return 29;
  struct sigaction sa = {.sa_handler = sigbus_handler};
  sigemptyset(&sa.sa_mask);
  sigaction(SIGBUS, &sa, NULL);
  saw_sigbus = 0;
  if (sigsetjmp(sigbus_env, 1) == 0) {
    volatile unsigned char beyond = shrunk[4096];
    (void)beyond;
  }
  munmap(shrunk, 8192);
  close(fd);
  int ok = saw_sigbus != 0;
  printf("{\"case\":\"mmap-core\",\"ok\":%s}\n", ok ? "true" : "false");
  if (!ok) return 30;

  char eof_path[4096];
  snprintf(eof_path, sizeof(eof_path), "%s/eof.bin", root);
  fd = open(eof_path, O_CREAT | O_TRUNC | O_RDWR, 0644);
  if (fd < 0 || ftruncate(fd, 4096 + 17) != 0) return 35;
  unsigned char *tail = mmap(NULL, 8192, PROT_READ, MAP_SHARED, fd, 0);
  if (tail == MAP_FAILED) return 36;
  int tail_zero = 1;
  for (size_t i = 4096 + 17; i < 8192; ++i) {
    if (tail[i] != 0) { tail_zero = 0; break; }
  }
  munmap(tail, 8192); close(fd);
  printf("{\"case\":\"mmap-eof-tail-zero\",\"ok\":%s}\n",
         tail_zero ? "true" : "false");
  if (!tail_zero) return 37;

#if defined(__x86_64__)
  char exec_path[4096];
  snprintf(exec_path, sizeof(exec_path), "%s/exec.bin", root);
  fd = open(exec_path, O_CREAT | O_TRUNC | O_RDWR, 0755);
  const unsigned char code[] = {0xb8, 0x2a, 0x00, 0x00, 0x00, 0xc3};
  if (fd < 0 || write(fd, code, sizeof(code)) != (ssize_t)sizeof(code)) return 32;
  void *executable = mmap(NULL, 4096, PROT_READ | PROT_EXEC, MAP_PRIVATE, fd, 0);
  if (executable == MAP_FAILED) return 33;
  int value = ((int (*)(void))executable)();
  munmap(executable, 4096);
  close(fd);
  printf("{\"case\":\"mmap-prot-exec\",\"ok\":%s,\"value\":%d}\n",
         value == 42 ? "true" : "false", value);
  if (value != 42) return 34;
#endif
  return 0;
}

static int fake_protocol_case(const char *root) {
  char path[4096];
  snprintf(path, sizeof(path), "%s/static", root);
  int fd = open(path, O_RDWR);
  if (fd < 0) return 40;
  struct stat st;
  if (fstat(fd, &st) != 0 || st.st_size != 8192) return 41;
  char buf[64] = {};
  ssize_t n = pread(fd, buf, sizeof(buf), 0);
  int static_ok = n == 17 && memcmp(buf, "static-fuse-data\n", 17) == 0;
  printf("{\"case\":\"fake-static-read\",\"ok\":%s,\"bytes\":%zd}\n",
         static_ok ? "true" : "false", n);
  if (!static_ok) return 42;
  unsigned char *mapped = mmap(NULL, 4096, PROT_READ, MAP_PRIVATE, fd, 0);
  int mmap_ok = mapped != MAP_FAILED && memcmp(mapped, "static-fuse-data\n", 17) == 0;
  printf("{\"case\":\"fake-static-mmap\",\"ok\":%s,\"errno\":%d}\n",
         mmap_ok ? "true" : "false", mapped == MAP_FAILED ? errno : 0);
  if (mapped != MAP_FAILED) munmap(mapped, 4096);
  if (posix_fallocate(fd, 5, 1) != 0) return 43;
  printf("{\"case\":\"fake-fallocate-offset\",\"ok\":true}\n");
  close(fd);

  snprintf(path, sizeof(path), "%s/direct", root);
  fd = open(path, O_WRONLY);
  char *payload = malloc(262144);
  if (fd < 0 || payload == NULL) return 46;
  memset(payload, 0x5a, 262144);
  ssize_t wrote = write(fd, payload, 262144);
  free(payload); close(fd);
  snprintf(path, sizeof(path), "%s/write-stats", root);
  fd = open(path, O_RDONLY); memset(buf, 0, sizeof(buf)); n = read(fd, buf, sizeof(buf)-1); close(fd);
  size_t total = 0, max_write = 0, zero_nonzero = 0;
  int write_ok = wrote == 262144 && n > 0 && sscanf(buf, "total=%zu max=%zu zero_nonzero=%zu", &total, &max_write, &zero_nonzero) == 3 && total == 262144 && max_write > 4096 && zero_nonzero == 0;
  printf("{\"case\":\"fuse-big-writes\",\"ok\":%s,\"total\":%zu,\"max\":%zu,\"zero_nonzero\":%zu}\n",
         write_ok ? "true" : "false", total, max_write, zero_nonzero);

  snprintf(path, sizeof(path), "%s/dynamic", root);
  fd = open(path, O_RDONLY);
  if (fd < 0 || fstat(fd, &st) != 0) return 44;
  memset(buf, 0, sizeof(buf));
  n = read(fd, buf, sizeof(buf));
  close(fd);
  int dynamic_ok = st.st_size == 0 && n == 23 && memcmp(buf, "dynamic-zero-size-data\n", 23) == 0;
  printf("{\"case\":\"fake-directio-zero-size-read\",\"ok\":%s,\"size\":%lld,\"bytes\":%zd}\n",
         dynamic_ok ? "true" : "false", (long long)st.st_size, n);

  int direct_mmap_ok = 1;
  const char *direct_names[] = {"direct", "dynamic"};
  for (size_t i = 0; i < 2; ++i) {
    snprintf(path, sizeof(path), "%s/%s", root, direct_names[i]);
    fd = open(path, O_RDWR);
    errno = 0;
    void *direct_private = mmap(NULL, 4096, PROT_READ, MAP_PRIVATE, fd, 0);
    int private_errno = direct_private == MAP_FAILED ? errno : 0;
    if (direct_private != MAP_FAILED) munmap(direct_private, 4096);
    errno = 0;
    void *direct_shared = mmap(NULL, 4096, PROT_READ | PROT_WRITE, MAP_SHARED, fd, 0);
    int shared_errno = direct_shared == MAP_FAILED ? errno : 0;
    if (direct_shared != MAP_FAILED) munmap(direct_shared, 4096);
    close(fd);
    int one_ok = private_errno == 0 && shared_errno == ENODEV;
    direct_mmap_ok = direct_mmap_ok && one_ok;
    printf("{\"case\":\"fake-directio-mmap-%s\",\"ok\":%s,\"private_errno\":%d,\"shared_errno\":%d}\n",
           direct_names[i], one_ok ? "true" : "false", private_errno, shared_errno);
  }
  return mmap_ok && dynamic_ok && direct_mmap_ok && write_ok ? 0 : 45;
}

int main(int argc, char **argv) {
  if (argc < 3) {
    fprintf(stderr, "usage: %s fsync MOUNTPOINT [DELAY_MS] | fake MOUNTPOINT | mmap ROOT\n", argv[0]);
    return 2;
  }
  if (strcmp(argv[1], "fsync") == 0) {
    int delay = argc > 3 ? atoi(argv[3]) : 300;
    char ok[4096], eio[4096], nosys[4096];
    snprintf(ok, sizeof(ok), "%s/fsync-ok", argv[2]);
    snprintf(eio, sizeof(eio), "%s/fsync-eio", argv[2]);
    snprintf(nosys, sizeof(nosys), "%s/fsync-nosys", argv[2]);
    int a = fsync_case(ok, 0, delay * 3 / 4);
    int b = fsync_case(eio, EIO, delay * 3 / 4);
    int c = sync_call_case("fdatasync-delayed", ok, 0, 1, 0, delay * 3 / 4);
    int d = sync_call_case("fsyncdir-delayed", argv[2], 1, 0, 0, delay * 3 / 4);
    // ENOSYS is last: Linux caches it as connection-wide no_fsync support.
    int e = sync_call_case("fsync-enosys", nosys, 0, 0, 0, delay * 3 / 4);
    if (a != 0) return a;
    if (b != 0) return b;
    if (c != 0) return c;
    if (d != 0) return d;
    return e;
  }
  if (strcmp(argv[1], "fake") == 0) return fake_protocol_case(argv[2]);
  if (strcmp(argv[1], "mmap") == 0) return mmap_case(argv[2]);
  return 2;
}
