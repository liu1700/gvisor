#define _POSIX_C_SOURCE 200809L
#define FUSE_USE_VERSION 35

#include <errno.h>
#include <fcntl.h>
#include <fuse3/fuse_lowlevel.h>
#include <signal.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <time.h>
#include <unistd.h>

enum {
  ROOT_INO = 1,
  STATIC_INO,
  DIRECT_INO,
  DYNAMIC_INO,
  FSYNC_OK_INO,
  FSYNC_EIO_INO,
  FSYNC_NOSYS_INO,
  WRITE_STATS_INO,
};

static const char static_data[] = "static-fuse-data\n";
static const char dynamic_data[] = "dynamic-zero-size-data\n";
static int delay_ms = 300;
static volatile sig_atomic_t stop;
static size_t written_total;
static size_t written_max;
static size_t zero_write_nonzero_offset;

static void sleep_ms(int ms) {
  struct timespec ts = {.tv_sec = ms / 1000, .tv_nsec = (ms % 1000) * 1000000L};
  while (nanosleep(&ts, &ts) != 0 && errno == EINTR) {}
}

static void fill_stat(fuse_ino_t ino, struct stat *st) {
  memset(st, 0, sizeof(*st));
  st->st_ino = ino;
  st->st_uid = getuid();
  st->st_gid = getgid();
  st->st_blksize = 4096;
  if (ino == ROOT_INO) {
    st->st_mode = S_IFDIR | 0755;
    st->st_nlink = 2;
    return;
  }
  st->st_mode = S_IFREG | 0644;
  st->st_nlink = 1;
  st->st_size = ino == DYNAMIC_INO ? 0 : 8192;
  st->st_blocks = (st->st_size + 511) / 512;
}

static fuse_ino_t name_ino(const char *name) {
  if (strcmp(name, "static") == 0) return STATIC_INO;
  if (strcmp(name, "direct") == 0) return DIRECT_INO;
  if (strcmp(name, "dynamic") == 0) return DYNAMIC_INO;
  if (strcmp(name, "fsync-ok") == 0) return FSYNC_OK_INO;
  if (strcmp(name, "fsync-eio") == 0) return FSYNC_EIO_INO;
  if (strcmp(name, "fsync-nosys") == 0) return FSYNC_NOSYS_INO;
  if (strcmp(name, "write-stats") == 0) return WRITE_STATS_INO;
  return 0;
}

static void op_lookup(fuse_req_t req, fuse_ino_t parent, const char *name) {
  fuse_ino_t ino = parent == ROOT_INO ? name_ino(name) : 0;
  if (ino == 0) {
    fuse_reply_err(req, ENOENT);
    return;
  }
  struct fuse_entry_param entry = {.ino = ino, .generation = 1, .attr_timeout = 0, .entry_timeout = 0};
  fill_stat(ino, &entry.attr);
  fuse_reply_entry(req, &entry);
}

static void op_getattr(fuse_req_t req, fuse_ino_t ino, struct fuse_file_info *fi) {
  (void)fi;
  if (ino < ROOT_INO || ino > WRITE_STATS_INO) {
    fuse_reply_err(req, ENOENT);
    return;
  }
  struct stat st;
  fill_stat(ino, &st);
  fuse_reply_attr(req, &st, 0);
}

static void op_open(fuse_req_t req, fuse_ino_t ino, struct fuse_file_info *fi) {
  if (ino < STATIC_INO || ino > WRITE_STATS_INO) {
    fuse_reply_err(req, EISDIR);
    return;
  }
  fi->fh = ino;
  if (ino == DIRECT_INO || ino == DYNAMIC_INO) fi->direct_io = 1;
  fuse_reply_open(req, fi);
}

static void op_opendir(fuse_req_t req, fuse_ino_t ino, struct fuse_file_info *fi) {
  if (ino != ROOT_INO) {
    fuse_reply_err(req, ENOTDIR);
    return;
  }
  fi->fh = ino;
  fuse_reply_open(req, fi);
}

static void reply_buf(fuse_req_t req, const char *data, size_t data_len, size_t size, off_t off) {
  if (off < 0 || (size_t)off >= data_len) {
    fuse_reply_buf(req, NULL, 0);
    return;
  }
  size_t remain = data_len - (size_t)off;
  if (size > remain) size = remain;
  fuse_reply_buf(req, data + off, size);
}

static void op_read(fuse_req_t req, fuse_ino_t ino, size_t size, off_t off,
                    struct fuse_file_info *fi) {
  (void)fi;
  if (ino == DYNAMIC_INO) {
    reply_buf(req, dynamic_data, sizeof(dynamic_data) - 1, size, off);
  } else if (ino == WRITE_STATS_INO) {
    char stats[128];
    int n = snprintf(stats, sizeof(stats), "total=%zu max=%zu zero_nonzero=%zu\n",
                     written_total, written_max, zero_write_nonzero_offset);
    reply_buf(req, stats, (size_t)n, size, off);
  } else {
    reply_buf(req, static_data, sizeof(static_data) - 1, size, off);
  }
}

static void op_write(fuse_req_t req, fuse_ino_t ino, const char *buf, size_t size,
                     off_t off, struct fuse_file_info *fi) {
  (void)ino; (void)buf; (void)off; (void)fi;
  written_total += size;
  if (size > written_max) written_max = size;
  if (size == 0 && off != 0) zero_write_nonzero_offset++;
  printf("{\"server_opcode\":\"FUSE_WRITE\",\"size\":%zu}\n", size);
  fflush(stdout);
  fuse_reply_write(req, size);
}

static void op_fsync(fuse_req_t req, fuse_ino_t ino, int datasync,
                     struct fuse_file_info *fi) {
  (void)fi;
  sleep_ms(delay_ms);
  printf("{\"server_opcode\":\"FUSE_FSYNC\",\"ino\":%llu,\"datasync\":%d}\n",
         (unsigned long long)ino, datasync);
  fflush(stdout);
  int err = ino == FSYNC_EIO_INO ? EIO : (ino == FSYNC_NOSYS_INO ? ENOSYS : 0);
  fuse_reply_err(req, err);
}

static void op_fsyncdir(fuse_req_t req, fuse_ino_t ino, int datasync,
                        struct fuse_file_info *fi) {
  (void)fi;
  sleep_ms(delay_ms);
  printf("{\"server_opcode\":\"FUSE_FSYNCDIR\",\"ino\":%llu,\"datasync\":%d}\n",
         (unsigned long long)ino, datasync);
  fflush(stdout);
  fuse_reply_err(req, 0);
}

static void op_fallocate(fuse_req_t req, fuse_ino_t ino, int mode, off_t off,
                         off_t length, struct fuse_file_info *fi) {
  (void)ino; (void)mode; (void)off; (void)length; (void)fi;
  fuse_reply_err(req, 0);
}

static void on_signal(int sig) { (void)sig; stop = 1; }

int main(int argc, char **argv) {
  if (argc < 2 || argc > 3) {
    fprintf(stderr, "usage: %s MOUNTPOINT [DELAY_MS]\n", argv[0]);
    return 2;
  }
  if (argc == 3) delay_ms = atoi(argv[2]);
  if (delay_ms < 0) return 2;

  const struct fuse_lowlevel_ops ops = {
    .lookup = op_lookup, .getattr = op_getattr, .open = op_open, .opendir = op_opendir,
    .read = op_read, .write = op_write, .fsync = op_fsync, .fsyncdir = op_fsyncdir,
    .fallocate = op_fallocate,
  };
  char *fuse_argv[] = {argv[0], "-o", "default_permissions"};
  struct fuse_args args = FUSE_ARGS_INIT(3, fuse_argv);
  struct fuse_session *se = fuse_session_new(&args, &ops, sizeof(ops), NULL);
  if (se == NULL) return 1;
  if (fuse_session_mount(se, argv[1]) != 0) {
    fuse_session_destroy(se);
    return 1;
  }
  signal(SIGTERM, on_signal);
  signal(SIGINT, on_signal);
  int rc = 0;
  while (!stop) {
    rc = fuse_session_loop(se);
    break;
  }
  fuse_session_unmount(se);
  fuse_session_destroy(se);
  fuse_opt_free_args(&args);
  return rc == 0 ? 0 : 1;
}
