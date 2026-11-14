// init is PID 1 of a pod that runs in v86 on a tab node
// (k8s/tabernetes/v86.go). It is plain C with one poll loop and no threads:
// the Go version was 40 times bigger, used 3.7 MiB of the RAM of the pod and
// made every saved VM state 6 MB larger.
//
// The kernel starts it as /init of an initramfs, where it waits for the pod
// (stage0): the page may save the VM at that point and restore it for later
// pods (web/src/vmworker.js). The page then sends "go <unix time in ms>
// <random bytes in hex>" on ttyS1. A restored VM has the clock and the random
// numbers of the saved one, so init sets both first. Then it mounts the
// layers of the image over 9p, with the container as
// /.tabernetes/config.json on top, sets up eth0 and makes the 9p file system
// the root.
//
// It does what containerd and runc do for a container: mount /proc, /sys and
// /dev, set the hostname, then start the entrypoint with the environment,
// working directory and user of the container. The page puts /etc/hosts and
// /etc/resolv.conf into the file system, as the kubelet does. The console
// (ttyS0) is stdout and stderr of the container; the second serial port
// (ttyS1) tells the page "started", "exit <code>" and, if the entrypoint did
// not start, "error <message>" before that.
//
// kubectl exec and attach come in over the network: the kubelet connects to
// port 60250 and sends one line of JSON (k8s/tabernetes/v86exec.go). After
// that, both sides send frames: one byte type, four bytes big-endian length,
// the payload.
#define _GNU_SOURCE
#include <arpa/inet.h>
#include <ctype.h>
#include <errno.h>
#include <fcntl.h>
#include <net/if.h>
#include <net/route.h>
#include <netinet/in.h>
#include <poll.h>
#include <signal.h>
#include <stdarg.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/ioctl.h>
#include <sys/mman.h>
#include <sys/mount.h>
#include <sys/prctl.h>
#include <sys/signalfd.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/time.h>
#include <sys/wait.h>
#include <termios.h>
#include <time.h>
#include <unistd.h>

#define EXEC_PORT 60250

// Set by TABERNETES_INIT_TEST: run as a normal process on the host, for the
// tests in vm/test. Then init does not mount anything, reads the config from
// the path in that variable and reports on stdout.
static const char *test_config;
static int exec_port = EXEC_PORT;
#define NEW_ROOT "/newroot"
// The tag of the 9p device with the root file system. The page names it
// (MOUNT_TAG in web/src/kernelboot.js), so that / shows up as "overlay" in
// df and /proc/mounts, like in a container of containerd or Docker.
#define ROOT_TAG "overlay"
#define DEFAULT_PATH "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// frames to the VM
enum { IN_DATA = 0, IN_CLOSE = 1, IN_RESIZE = 2 };
// frames from the VM
enum { OUT_STDOUT = 1, OUT_STDERR = 2, OUT_EXIT = 3, OUT_ERROR = 4, OUT_STDIN_ACK = 5 };

#define MAX_FRAME (1 << 20)
// An attach that does not read its output is dropped rather than hold up the
// container. An exec stops reading the output of its command instead.
#define ATTACH_MAX_BUFFER (8 << 20)
#define EXEC_HIGH_WATER (256 << 10)

static int control = -1; // ttyS1

static void report(const char *fmt, ...) {
  if (control < 0) return;
  char buf[4096];
  va_list ap;
  va_start(ap, fmt);
  int n = vsnprintf(buf, sizeof(buf) - 1, fmt, ap);
  va_end(ap);
  if (n < 0) return;
  if (n > (int)sizeof(buf) - 2) n = sizeof(buf) - 2;
  for (int i = 0; i < n; i++)
    if (buf[i] == '\n') buf[i] = ' ';
  buf[n++] = '\n';
  for (int off = 0; off < n;) {
    ssize_t w = write(control, buf + off, n - off);
    if (w < 0 && errno == EINTR) continue;
    if (w <= 0) return;
    off += w;
  }
}

static void *xmalloc(size_t n) {
  void *p = malloc(n ? n : 1);
  if (!p) {
    report("error out of memory");
    for (;;) pause();
  }
  return p;
}

static void *xrealloc(void *p, size_t n) {
  p = realloc(p, n ? n : 1);
  if (!p) {
    report("error out of memory");
    for (;;) pause();
  }
  return p;
}

static char *xstrdup(const char *s) {
  size_t n = strlen(s) + 1;
  return memcpy(xmalloc(n), s, n);
}

static int64_t now_ms(void) {
  struct timespec ts;
  clock_gettime(CLOCK_MONOTONIC, &ts);
  return (int64_t)ts.tv_sec * 1000 + ts.tv_nsec / 1000000;
}

// errtext is the text of an errno as Go prints it: in lower case.
static const char *errtext(int e) {
  static char buf[128];
  snprintf(buf, sizeof(buf), "%s", strerror(e));
  if (buf[0]) buf[0] = tolower((unsigned char)buf[0]);
  return buf;
}

// ---- growing buffers and string lists ----

typedef struct {
  char *b;
  size_t len, cap;
} buf_t;

static void buf_add(buf_t *b, const void *p, size_t n) {
  if (b->len + n > b->cap) {
    size_t c = b->cap ? b->cap : 4096;
    while (c < b->len + n) c *= 2;
    b->b = xrealloc(b->b, c);
    b->cap = c;
  }
  memcpy(b->b + b->len, p, n);
  b->len += n;
}

static void buf_drop(buf_t *b, size_t n) {
  memmove(b->b, b->b + n, b->len - n);
  b->len -= n;
}

static void buf_free(buf_t *b) {
  free(b->b);
  b->b = NULL;
  b->len = b->cap = 0;
}

typedef struct {
  char **v; // NULL terminated
  int n, cap;
} strv_t;

static void strv_add(strv_t *s, char *str) {
  if (s->n + 2 > s->cap) {
    s->cap = s->cap ? s->cap * 2 : 8;
    s->v = xrealloc(s->v, s->cap * sizeof(char *));
  }
  s->v[s->n++] = str;
  s->v[s->n] = NULL;
}

// ---- JSON: just what config.json and the exec request need ----

typedef struct {
  const char *p, *end;
  int err;
} json_t;

static void js_ws(json_t *j) {
  while (j->p < j->end && (*j->p == ' ' || *j->p == '\t' || *j->p == '\n' || *j->p == '\r')) j->p++;
}

static int js_eat(json_t *j, char c) {
  js_ws(j);
  if (j->p < j->end && *j->p == c) {
    j->p++;
    return 1;
  }
  return 0;
}

static void put_utf8(buf_t *b, uint32_t c) {
  char u[4];
  int n;
  if (c < 0x80) u[0] = c, n = 1;
  else if (c < 0x800) u[0] = 0xc0 | c >> 6, u[1] = 0x80 | (c & 0x3f), n = 2;
  else if (c < 0x10000) u[0] = 0xe0 | c >> 12, u[1] = 0x80 | ((c >> 6) & 0x3f), u[2] = 0x80 | (c & 0x3f), n = 3;
  else u[0] = 0xf0 | c >> 18, u[1] = 0x80 | ((c >> 12) & 0x3f), u[2] = 0x80 | ((c >> 6) & 0x3f), u[3] = 0x80 | (c & 0x3f), n = 4;
  buf_add(b, u, n);
}

static int hex4(const char *p, uint32_t *v) {
  *v = 0;
  for (int i = 0; i < 4; i++) {
    int c = p[i], d;
    if (c >= '0' && c <= '9') d = c - '0';
    else if (c >= 'a' && c <= 'f') d = c - 'a' + 10;
    else if (c >= 'A' && c <= 'F') d = c - 'A' + 10;
    else return 0;
    *v = *v << 4 | d;
  }
  return 1;
}

// js_string returns a new string, or NULL on an error.
static char *js_string(json_t *j) {
  if (!js_eat(j, '"')) {
    j->err = 1;
    return NULL;
  }
  buf_t b = {0};
  while (j->p < j->end && *j->p != '"') {
    char c = *j->p++;
    if (c != '\\') {
      buf_add(&b, &c, 1);
      continue;
    }
    if (j->p >= j->end) break;
    c = *j->p++;
    switch (c) {
    case 'n': buf_add(&b, "\n", 1); break;
    case 't': buf_add(&b, "\t", 1); break;
    case 'r': buf_add(&b, "\r", 1); break;
    case 'b': buf_add(&b, "\b", 1); break;
    case 'f': buf_add(&b, "\f", 1); break;
    case 'u': {
      uint32_t u;
      if (j->end - j->p < 4 || !hex4(j->p, &u)) goto bad;
      j->p += 4;
      if (u >= 0xd800 && u < 0xdc00 && j->end - j->p >= 6 && j->p[0] == '\\' && j->p[1] == 'u') {
        uint32_t lo;
        if (hex4(j->p + 2, &lo) && lo >= 0xdc00 && lo < 0xe000) {
          u = 0x10000 + ((u - 0xd800) << 10) + (lo - 0xdc00);
          j->p += 6;
        }
      }
      put_utf8(&b, u);
      break;
    }
    default: buf_add(&b, &c, 1); // \" \\ \/
    }
  }
  if (j->p >= j->end) goto bad;
  j->p++;
  buf_add(&b, "", 1);
  return b.b;
bad:
  free(b.b);
  j->err = 1;
  return NULL;
}

static void js_skip(json_t *j);

static int js_bool(json_t *j) {
  js_ws(j);
  if (j->end - j->p >= 4 && !memcmp(j->p, "true", 4)) {
    j->p += 4;
    return 1;
  }
  if (j->end - j->p >= 5 && !memcmp(j->p, "false", 5)) {
    j->p += 5;
    return 0;
  }
  js_skip(j);
  return 0;
}

static long js_number(json_t *j) {
  js_ws(j);
  char *e;
  long v = strtol(j->p, &e, 10);
  if (e == j->p) j->err = 1;
  j->p = e;
  while (j->p < j->end && strchr("0123456789.eE+-", *j->p)) j->p++;
  return v;
}

// js_strings reads an array of strings; null is an empty one.
static void js_strings(json_t *j, strv_t *out) {
  js_ws(j);
  if (j->end - j->p >= 4 && !memcmp(j->p, "null", 4)) {
    j->p += 4;
    return;
  }
  if (!js_eat(j, '[')) {
    j->err = 1;
    return;
  }
  if (js_eat(j, ']')) return;
  do {
    char *s = js_string(j);
    if (!s) return;
    strv_add(out, s);
  } while (js_eat(j, ','));
  if (!js_eat(j, ']')) j->err = 1;
}

static void js_skip(json_t *j) {
  js_ws(j);
  if (j->p >= j->end) {
    j->err = 1;
    return;
  }
  char c = *j->p;
  if (c == '"') {
    free(js_string(j));
  } else if (c == '[' || c == '{') {
    char close = c == '[' ? ']' : '}';
    j->p++;
    if (js_eat(j, close)) return;
    do {
      if (c == '{') {
        free(js_string(j));
        if (!js_eat(j, ':')) j->err = 1;
      }
      js_skip(j);
    } while (!j->err && js_eat(j, ','));
    if (!js_eat(j, close)) j->err = 1;
  } else {
    while (j->p < j->end && !strchr(",]} \t\r\n", *j->p)) j->p++;
  }
}

// js_object calls field for every key of an object.
static int js_object(json_t *j, void (*field)(json_t *, const char *, void *), void *arg) {
  if (!js_eat(j, '{')) return -1;
  if (js_eat(j, '}')) return 0;
  do {
    char *k = js_string(j);
    if (!k || !js_eat(j, ':')) {
      free(k);
      return -1;
    }
    field(j, k, arg);
    free(k);
  } while (!j->err && js_eat(j, ','));
  if (j->err || !js_eat(j, '}')) return -1;
  return 0;
}

// ---- the container ----

// config is the container, written by the page (web/src/vmworker.js).
typedef struct {
  strv_t args, env;
  char *cwd, *user, *hostname, *ip, *gateway;
  int stdin_, stdin_once, tty;
} config_t;

static void config_field(json_t *j, const char *k, void *arg) {
  config_t *c = arg;
  if (!strcmp(k, "args")) js_strings(j, &c->args);
  else if (!strcmp(k, "env")) js_strings(j, &c->env);
  else if (!strcmp(k, "cwd")) c->cwd = js_string(j);
  else if (!strcmp(k, "user")) c->user = js_string(j);
  else if (!strcmp(k, "hostname")) c->hostname = js_string(j);
  else if (!strcmp(k, "ip")) c->ip = js_string(j);
  else if (!strcmp(k, "gateway")) c->gateway = js_string(j);
  else if (!strcmp(k, "stdin")) c->stdin_ = js_bool(j);
  else if (!strcmp(k, "stdinOnce")) c->stdin_once = js_bool(j);
  else if (!strcmp(k, "tty")) c->tty = js_bool(j);
  else js_skip(j);
}

static char *read_file(const char *path, size_t *len) {
  int fd = open(path, O_RDONLY | O_CLOEXEC);
  if (fd < 0) return NULL;
  buf_t b = {0};
  char tmp[8192];
  for (;;) {
    ssize_t n = read(fd, tmp, sizeof(tmp));
    if (n < 0 && errno == EINTR) continue;
    if (n <= 0) break;
    buf_add(&b, tmp, n);
  }
  close(fd);
  buf_add(&b, "", 1);
  if (len) *len = b.len - 1;
  return b.b;
}

static char err_msg[1024]; // the error of the last failed step

static int fail(const char *fmt, ...) {
  va_list ap;
  va_start(ap, fmt);
  vsnprintf(err_msg, sizeof(err_msg), fmt, ap);
  va_end(ap);
  return -1;
}

static int load_config(const char *path, config_t *c) {
  size_t n;
  char *s = read_file(path, &n);
  if (!s) return fail("open %s: %s", path, errtext(errno));
  json_t j = {s, s + n, 0};
  int rc = js_object(&j, config_field, c);
  free(s);
  if (rc < 0) return fail("config.json: invalid JSON");
  return 0;
}

static const char *env_get(strv_t *env, const char *key) {
  size_t k = strlen(key);
  for (int i = env->n - 1; i >= 0; i--)
    if (!strncmp(env->v[i], key, k) && env->v[i][k] == '=') return env->v[i] + k + 1;
  return NULL;
}

static void env_default(strv_t *env, const char *key, const char *value) {
  if (env_get(env, key)) return;
  char *s = xmalloc(strlen(key) + strlen(value) + 2);
  sprintf(s, "%s=%s", key, value);
  strv_add(env, s);
}

// look_path finds name in the PATH of the container, not of init. It
// returns a new string, or NULL with err_msg set.
static char *look_path(const char *name, const char *path) {
  if (strchr(name, '/')) return xstrdup(name);
  const char *p = path ? path : "";
  for (;;) {
    const char *e = strchr(p, ':');
    size_t n = e ? (size_t)(e - p) : strlen(p);
    char *full = xmalloc(n + strlen(name) + 3);
    if (n == 0) sprintf(full, "%s", name);
    else sprintf(full, "%.*s/%s", (int)n, p, name);
    struct stat st;
    if (stat(full, &st) == 0 && !S_ISDIR(st.st_mode) && (st.st_mode & 0111)) return full;
    free(full);
    if (!e) break;
    p = e + 1;
  }
  fail("exec: \"%s\": executable file not found in $PATH", name);
  return NULL;
}

// The user of the container: uid, uid:gid, name or name:group, as runc
// resolves them with /etc/passwd and /etc/group of the image.
typedef struct {
  int set;
  uid_t uid;
  gid_t gid;
  char *home;
} cred_t;

// db_find returns the fields of the first line of a passwd or group file
// whose first or third field is key, in a static buffer.
static int db_find(const char *path, const char *key, int by_name_only, char *fields[7]) {
  static char *data;
  free(data);
  data = read_file(path, NULL);
  if (!data) return 0;
  for (char *line = data; line && *line;) {
    char *next = strchr(line, '\n');
    if (next) *next++ = 0;
    if (*line && *line != '#') {
      int n = 0;
      for (char *f = line; n < 7;) {
        fields[n++] = f;
        f = strchr(f, ':');
        if (!f) break;
        *f++ = 0;
      }
      for (int i = n; i < 7; i++) fields[i] = NULL;
      if (n >= 3 && (!strcmp(fields[0], key) || (!by_name_only && !strcmp(fields[2], key)))) return n;
    }
    line = next;
  }
  return 0;
}

static int is_number(const char *s) {
  if (!*s) return 0;
  for (; *s; s++)
    if (!isdigit((unsigned char)*s)) return 0;
  return 1;
}

static int lookup_user(const char *spec, cred_t *cred) {
  memset(cred, 0, sizeof(*cred));
  if (!spec || !*spec) {
    cred->home = "/root";
    return 0;
  }
  char *u = xstrdup(spec), *g = strchr(u, ':');
  if (g) *g++ = 0;
  long uid = -1, gid = -1;
  char *home = "/";
  if (is_number(u)) uid = atol(u);
  char *f[7];
  if (db_find("/etc/passwd", u, 0, f) >= 6) {
    uid = atol(f[2]);
    gid = atol(f[3]);
    home = xstrdup(f[5]);
  }
  if (uid < 0) return fail("unable to find user %s: no matching entries in passwd file", u);
  if (g) {
    gid = is_number(g) ? atol(g) : -1;
    if (db_find("/etc/group", g, 1, f) >= 3) gid = atol(f[2]);
    if (gid < 0) return fail("unable to find group %s: no matching entries in group file", g);
  }
  if (gid < 0) gid = 0;
  if (uid == 0 && !strcmp(home, "/")) home = "/root";
  cred->set = 1;
  cred->uid = uid;
  cred->gid = gid;
  cred->home = home;
  return 0;
}

static strv_t ctr_env;
static char *ctr_dir = "/";
static cred_t ctr_cred;

// ---- processes ----

typedef struct proc {
  pid_t pid;
  int done, code; // code: as a shell reports it
  int stdin_fd;   // -1 without stdin; the pty master with a tty
  int pty;        // the master, -1 without a tty
  int out[2];     // the read ends of stdout and stderr, or the pty; -1 closed
  int out_type[2];
  int nout;
  int64_t drain_until; // after the exit: how long to wait for the output
} proc_t;

static int exit_code(int status) {
  if (WIFSIGNALED(status)) return 128 + WTERMSIG(status);
  return WEXITSTATUS(status);
}

static void set_size(int fd, int rows, int cols) {
  if (fd < 0 || rows <= 0 || cols <= 0) return;
  struct winsize ws = {.ws_row = rows, .ws_col = cols};
  ioctl(fd, TIOCSWINSZ, &ws);
}

// open_pty opens a pair of the devpts instance of the container.
static int open_pty(int rows, int cols, int *master, int *slave) {
  int m = open("/dev/ptmx", O_RDWR | O_NOCTTY | O_CLOEXEC);
  if (m < 0) return fail("open pty: %s", errtext(errno));
  int unlock = 0;
  unsigned n;
  char name[32];
  if (ioctl(m, TIOCSPTLCK, &unlock) < 0 || ioctl(m, TIOCGPTN, &n) < 0) {
    int e = errno;
    close(m);
    return fail("open pty: %s", errtext(e));
  }
  snprintf(name, sizeof(name), "/dev/pts/%u", n);
  int s = open(name, O_RDWR | O_NOCTTY | O_CLOEXEC);
  if (s < 0) {
    int e = errno;
    close(m);
    return fail("open pty: %s", errtext(e));
  }
  set_size(m, rows, cols);
  *master = m;
  *slave = s;
  return 0;
}

static void nonblock(int fd) {
  fcntl(fd, F_SETFL, fcntl(fd, F_GETFL) | O_NONBLOCK);
}

// proc_start starts argv in the container: as its user, in its working
// directory, in a session of its own, with a tty or with pipes.
static int proc_start(proc_t *p, const char *path, char **argv, int tty, int with_stdin, int rows, int cols) {
  memset(p, 0, sizeof(*p));
  p->stdin_fd = p->pty = p->out[0] = p->out[1] = -1;
  int child[3] = {-1, -1, -1};
  int extra[3] = {-1, -1, -1}; // the ends of the child, closed after the start
  strv_t env = {0};
  for (int i = 0; i < ctr_env.n; i++) strv_add(&env, ctr_env.v[i]);
  if (tty && !env_get(&env, "TERM")) strv_add(&env, "TERM=xterm"); // as containerd does
  if (tty) {
    int m = -1, s = -1;
    if (open_pty(rows, cols, &m, &s) < 0) goto fail;
    child[0] = child[1] = child[2] = s;
    extra[0] = s;
    p->pty = m;
    if (with_stdin) p->stdin_fd = m;
    p->out[0] = m;
    p->out_type[0] = OUT_STDOUT;
    p->nout = 1;
  } else {
    int fds[2];
    if (with_stdin) {
      if (pipe2(fds, O_CLOEXEC) < 0) goto fail_errno;
      child[0] = extra[0] = fds[0];
      p->stdin_fd = fds[1];
    } else {
      child[0] = extra[0] = open("/dev/null", O_RDONLY | O_CLOEXEC);
    }
    for (int i = 0; i < 2; i++) {
      if (pipe2(fds, O_CLOEXEC) < 0) goto fail_errno;
      child[i + 1] = extra[i + 1] = fds[1];
      p->out[i] = fds[0];
      p->out_type[i] = i ? OUT_STDERR : OUT_STDOUT;
    }
    p->nout = 2;
  }
  int errp[2];
  if (pipe2(errp, O_CLOEXEC) < 0) goto fail_errno;
  pid_t pid = fork();
  if (pid < 0) {
    close(errp[0]);
    close(errp[1]);
    goto fail_errno;
  }
  if (pid == 0) {
    sigset_t all;
    sigemptyset(&all);
    sigprocmask(SIG_SETMASK, &all, NULL);
    setsid();
    // in the tests on the host, init is not PID 1: its children must not
    // outlive it
    if (test_config) prctl(PR_SET_PDEATHSIG, SIGKILL);
    int e = 0;
    // move the ends of the child out of the way of 0-2 first
    int fd[3];
    for (int i = 0; i < 3; i++) fd[i] = fcntl(child[i], F_DUPFD_CLOEXEC, 10);
    for (int i = 0; i < 3; i++)
      if (dup2(fd[i], i) < 0) e = errno;
    if (!e && tty && ioctl(0, TIOCSCTTY, 1) < 0) e = errno;
    if (!e && ctr_cred.set && (setgid(ctr_cred.gid) < 0 || setuid(ctr_cred.uid) < 0)) e = errno;
    if (!e && chdir(ctr_dir) < 0) e = errno;
    if (!e) {
      execve(path, argv, env.v);
      e = errno;
    }
    (void)!write(errp[1], &e, sizeof(e));
    _exit(127);
  }
  close(errp[1]);
  int e = 0;
  ssize_t n;
  while ((n = read(errp[0], &e, sizeof(e))) < 0 && errno == EINTR) {}
  close(errp[0]);
  for (int i = 0; i < 3; i++)
    if (extra[i] >= 0) close(extra[i]);
  free(env.v);
  if (n == sizeof(e)) {
    // the child is gone: the reaper ignores the pid
    if (p->pty >= 0) close(p->pty);
    else {
      if (p->stdin_fd >= 0) close(p->stdin_fd);
      for (int i = 0; i < p->nout; i++) close(p->out[i]);
    }
    return fail("exec: \"%s\": %s", argv[0], errtext(e));
  }
  p->pid = pid;
  if (p->stdin_fd >= 0) nonblock(p->stdin_fd);
  for (int i = 0; i < p->nout; i++) nonblock(p->out[i]);
  return 0;
fail_errno:
  fail("%s", errtext(errno));
fail:
  for (int i = 0; i < 3; i++)
    if (extra[i] >= 0) close(extra[i]);
  if (p->pty >= 0) close(p->pty);
  else {
    if (p->stdin_fd >= 0) close(p->stdin_fd);
    for (int i = 0; i < 2; i++)
      if (p->out[i] >= 0) close(p->out[i]);
  }
  free(env.v);
  return -1;
}

// proc_end_input tells the process that stdin ended. A tty has no end of its
// own, so it gets ^D, as a terminal does.
static void proc_end_input(proc_t *p) {
  if (p->stdin_fd < 0) return;
  if (p->pty >= 0) {
    char eot = 4;
    (void)!write(p->pty, &eot, 1);
    return;
  }
  close(p->stdin_fd);
  p->stdin_fd = -1;
}

static void proc_close(proc_t *p) {
  if (p->pty >= 0) {
    close(p->pty);
  } else {
    if (p->stdin_fd >= 0) close(p->stdin_fd);
    for (int i = 0; i < p->nout; i++)
      if (p->out[i] >= 0) close(p->out[i]);
  }
  p->pty = p->stdin_fd = p->out[0] = p->out[1] = -1;
}

static int proc_outputs_open(proc_t *p) {
  for (int i = 0; i < p->nout; i++)
    if (p->out[i] >= 0) return 1;
  return 0;
}

// ---- kubectl exec and attach ----

enum { S_REQUEST, S_EXEC, S_ATTACH };

typedef struct session {
  struct session *next;
  int fd, kind;
  buf_t in, out;
  proc_t proc; // of an exec
  int has_proc;
  int stdin_on;   // stdin frames go to the process
  buf_t pending;  // a stdin frame that is not written yet
  int close_next; // stdin ended after the pending frame
  int closing;    // close once out is sent
  int gone;       // the peer went away
  int dropped;    // an attach that was too slow
  int ended;      // endStdin ran
} session_t;

static session_t *sessions;
static proc_t main_proc;
static int main_started, main_exited, main_code;
static int main_stdin_once, main_stdin_closed;
static int64_t main_exit_at;
static int console = -1;

static void frame(session_t *s, int type, const void *b, uint32_t n) {
  if (s->closing && type != OUT_EXIT && type != OUT_ERROR) return;
  unsigned char h[5] = {type, n >> 24, n >> 16, n >> 8, n};
  buf_add(&s->out, h, 5);
  if (n) buf_add(&s->out, b, n);
}

static void frame_code(session_t *s, int code) {
  unsigned char b[4] = {(uint32_t)code >> 24, (uint32_t)code >> 16, (uint32_t)code >> 8, code};
  frame(s, OUT_EXIT, b, 4);
}

static void frame_error(session_t *s, const char *msg) {
  frame(s, OUT_ERROR, msg, strlen(msg));
  s->closing = 1;
}

static int stdin_target(session_t *s) {
  if (!s->stdin_on) return -1;
  if (s->kind == S_EXEC) return s->proc.stdin_fd;
  return main_proc.stdin_fd;
}

// end_stdin: an exec closes the stdin of its command; an attach with
// stdinOnce closes the stdin of the container, once.
static void end_stdin(session_t *s) {
  if (s->kind == S_EXEC) {
    if (s->stdin_on) proc_end_input(&s->proc);
    return;
  }
  if (s->ended || !s->stdin_on) return;
  s->ended = 1;
  if (main_stdin_once && !main_stdin_closed) {
    main_stdin_closed = 1;
    proc_end_input(&main_proc);
  }
}

// write_pending writes the stdin frame that waits, and acknowledges it once
// it is written: the kubelet sends the next one only then.
static void write_pending(session_t *s) {
  if (!s->pending.len && !s->close_next) return;
  int fd = stdin_target(s);
  while (s->pending.len && fd >= 0) {
    ssize_t n = write(fd, s->pending.b, s->pending.len);
    if (n < 0 && errno == EINTR) continue;
    if (n < 0 && errno == EAGAIN) return;
    if (n <= 0) break; // the reader is gone: drop the rest
    buf_drop(&s->pending, n);
  }
  int had = s->pending.len > 0 || !s->close_next;
  s->pending.len = 0;
  if (had) frame(s, OUT_STDIN_ACK, NULL, 0);
  if (s->close_next) {
    s->close_next = 0;
    end_stdin(s);
  }
}

static void session_frame(session_t *s, int type, const char *b, uint32_t n) {
  switch (type) {
  case IN_DATA:
    if (s->pending.len) {
      s->gone = 1; // the peer did not wait for the acknowledgement
      return;
    }
    if (stdin_target(s) < 0) {
      frame(s, OUT_STDIN_ACK, NULL, 0);
      return;
    }
    buf_add(&s->pending, b, n);
    if (!n) frame(s, OUT_STDIN_ACK, NULL, 0);
    else write_pending(s);
    break;
  case IN_CLOSE:
    if (s->pending.len) s->close_next = 1;
    else end_stdin(s);
    break;
  case IN_RESIZE:
    if (n == 4) {
      int pty = s->kind == S_EXEC ? s->proc.pty : main_proc.pty;
      set_size(pty, (unsigned char)b[0] << 8 | (unsigned char)b[1], (unsigned char)b[2] << 8 | (unsigned char)b[3]);
    }
    break;
  }
}

typedef struct {
  strv_t cmd;
  int tty, stdin_, attach, rows, cols;
} request_t;

static void request_field(json_t *j, const char *k, void *arg) {
  request_t *r = arg;
  if (!strcmp(k, "cmd")) js_strings(j, &r->cmd);
  else if (!strcmp(k, "tty")) r->tty = js_bool(j);
  else if (!strcmp(k, "stdin")) r->stdin_ = js_bool(j);
  else if (!strcmp(k, "attach")) r->attach = js_bool(j);
  else if (!strcmp(k, "rows")) r->rows = js_number(j);
  else if (!strcmp(k, "cols")) r->cols = js_number(j);
  else js_skip(j);
}

static void session_request(session_t *s, const char *line, size_t n) {
  request_t r = {0};
  json_t j = {line, line + n, 0};
  if (js_object(&j, request_field, &r) < 0) {
    frame_error(s, "invalid request");
    goto out;
  }
  if (r.attach) {
    s->kind = S_ATTACH;
    if (!main_started || main_exited) {
      frame_error(s, "container not running");
      goto out;
    }
    s->stdin_on = r.stdin_ && main_proc.stdin_fd >= 0;
    if (main_proc.pty >= 0 && r.rows > 0) set_size(main_proc.pty, r.rows, r.cols);
    goto out;
  }
  s->kind = S_EXEC;
  if (!r.cmd.n) {
    frame_error(s, "exec: no command specified");
    goto out;
  }
  char *path = look_path(r.cmd.v[0], env_get(&ctr_env, "PATH"));
  if (!path || proc_start(&s->proc, path, r.cmd.v, r.tty, r.stdin_, r.rows, r.cols) < 0) {
    frame_error(s, err_msg);
    free(path);
    goto out;
  }
  free(path);
  s->has_proc = 1;
  s->stdin_on = r.stdin_;
out:
  for (int i = 0; i < r.cmd.n; i++) free(r.cmd.v[i]);
  free(r.cmd.v);
}

// session_input handles what came from the kubelet.
static void session_input(session_t *s) {
  for (;;) {
    if (s->closing || s->gone) return;
    if (s->kind == S_REQUEST) {
      char *nl = memchr(s->in.b, '\n', s->in.len);
      if (!nl) {
        if (s->in.len > MAX_FRAME) s->gone = 1;
        return;
      }
      size_t n = nl - s->in.b;
      session_request(s, s->in.b, n);
      buf_drop(&s->in, n + 1);
      continue;
    }
    if (s->in.len < 5) return;
    unsigned char *h = (unsigned char *)s->in.b;
    uint32_t n = (uint32_t)h[1] << 24 | h[2] << 16 | h[3] << 8 | h[4];
    if (n > MAX_FRAME) {
      s->gone = 1;
      return;
    }
    if (s->in.len < 5 + n) return;
    session_frame(s, h[0], s->in.b + 5, n);
    buf_drop(&s->in, 5 + n);
  }
}

static void session_free(session_t *s) {
  close(s->fd);
  if (s->has_proc) proc_close(&s->proc);
  buf_free(&s->in);
  buf_free(&s->out);
  buf_free(&s->pending);
  free(s);
}

// ---- the poll loop ----

static int listener = -1, sigfd = -1;

static void write_all(int fd, const char *b, size_t n) {
  while (n > 0) {
    ssize_t w = write(fd, b, n);
    if (w < 0 && errno == EINTR) continue;
    if (w <= 0) return;
    b += w;
    n -= w;
  }
}

// read_output reads one chunk of an output of p; it returns the bytes, 0 at
// the end, -1 if there is nothing now.
static ssize_t read_output(proc_t *p, int i, char *b, size_t n) {
  ssize_t r = read(p->out[i], b, n);
  if (r < 0 && (errno == EAGAIN || errno == EINTR)) return -1;
  if (r <= 0) {
    // the end, or EIO from a pty whose last slave closed
    if (p->pty < 0) close(p->out[i]);
    p->out[i] = -1;
    return 0;
  }
  return r;
}

static void main_output(int i) {
  static char b[32 << 10];
  ssize_t n = read_output(&main_proc, i, b, sizeof(b));
  if (n <= 0) return;
  if (console >= 0) write_all(console, b, n);
  for (session_t *s = sessions; s; s = s->next) {
    if (s->kind != S_ATTACH || s->closing || s->gone) continue;
    frame(s, main_proc.out_type[i], b, n);
    if (s->out.len > ATTACH_MAX_BUFFER) {
      // too slow: drop it rather than hold up the container
      s->dropped = 1;
      s->gone = 1;
    }
  }
}

static void exec_output(session_t *s, int i) {
  static char b[32 << 10];
  ssize_t n = read_output(&s->proc, i, b, sizeof(b));
  if (n > 0) frame(s, s->proc.out_type[i], b, n);
}

static void reap(void) {
  int status;
  pid_t pid;
  while ((pid = waitpid(-1, &status, WNOHANG)) > 0) {
    proc_t *p = NULL;
    if (main_started && pid == main_proc.pid) p = &main_proc;
    for (session_t *s = sessions; s && !p; s = s->next)
      if (s->has_proc && s->proc.pid == pid) p = &s->proc;
    if (!p || p->done) continue;
    p->done = 1;
    p->code = exit_code(status);
    // what is still in the pipes, unless a process in the background holds
    // them open
    p->drain_until = now_ms() + 1000;
  }
}

static void accept_sessions(void) {
  for (;;) {
    int fd = accept4(listener, NULL, NULL, SOCK_NONBLOCK | SOCK_CLOEXEC);
    if (fd < 0) return;
    session_t *s = xmalloc(sizeof(*s));
    memset(s, 0, sizeof(*s));
    s->fd = fd;
    s->proc.stdin_fd = s->proc.pty = s->proc.out[0] = s->proc.out[1] = -1;
    s->next = sessions;
    sessions = s;
  }
}

static void session_read(session_t *s) {
  char b[32 << 10];
  for (;;) {
    ssize_t n = read(s->fd, b, sizeof(b));
    if (n < 0 && errno == EINTR) continue;
    if (n < 0 && errno == EAGAIN) break;
    if (n <= 0) {
      s->gone = 1;
      return;
    }
    buf_add(&s->in, b, n);
    if (s->in.len > 2 * MAX_FRAME) break;
  }
  session_input(s);
}

static void session_write(session_t *s) {
  while (s->out.len) {
    ssize_t n = write(s->fd, s->out.b, s->out.len);
    if (n < 0 && errno == EINTR) continue;
    if (n < 0 && errno == EAGAIN) return;
    if (n <= 0) {
      s->gone = 1;
      s->out.len = 0;
      return;
    }
    buf_drop(&s->out, n);
  }
}

// session_step moves a session on; it returns 0 when the session is done.
static int session_step(session_t *s, int64_t now) {
  if (s->kind == S_EXEC && s->has_proc) {
    proc_t *p = &s->proc;
    if (s->gone && !p->done) {
      // kubectl went away: so does the command, with its children
      kill(-p->pid, SIGKILL);
      return 1;
    }
    if (p->done && (!proc_outputs_open(p) || now >= p->drain_until)) {
      proc_close(p);
      s->has_proc = 0;
      frame_code(s, p->code);
      s->closing = 1;
    }
  }
  if (s->gone) {
    if (s->kind == S_ATTACH) end_stdin(s);
    return 0;
  }
  if (s->closing && !s->out.len) return 0;
  return 1;
}

static void main_step(int64_t now) {
  if (!main_started || main_exited || !main_proc.done) return;
  if (proc_outputs_open(&main_proc) && now < main_proc.drain_until) return;
  proc_close(&main_proc);
  main_exited = 1;
  main_code = main_proc.code;
  main_exit_at = now;
  for (session_t *s = sessions; s; s = s->next) {
    if (s->kind != S_ATTACH || s->gone) continue;
    if (!s->dropped) frame_code(s, main_code);
    s->closing = 1;
  }
}

static int attaches_left(void) {
  for (session_t *s = sessions; s; s = s->next)
    if (s->kind == S_ATTACH) return 1;
  return 0;
}

// loop runs until the main process exited and the attaches saw it, or
// forever with exit_done set (exec still works, as with runc).
static void loop(int *reported) {
  struct pollfd *fds = NULL;
  void **who = NULL; // what each pollfd is for
  int *what = NULL;
  int cap = 0;
  for (;;) {
    int64_t now = now_ms();
    main_step(now);
    for (session_t **pp = &sessions; *pp;) {
      session_t *s = *pp;
      if (!session_step(s, now)) {
        *pp = s->next;
        session_free(s);
      } else {
        pp = &s->next;
      }
    }
    if (main_exited && !*reported && (!attaches_left() || now - main_exit_at >= 500)) {
      // let kubectl attach see the end before the page stops the VM
      report("exit %d", main_code);
      *reported = 1;
    }
    int n = 0;
    int64_t deadline = -1;
#define ADD(fd_, ev_, who_, what_)                                     \
  do {                                                                 \
    if (n == cap) {                                                    \
      cap = cap ? cap * 2 : 32;                                        \
      fds = xrealloc(fds, cap * sizeof(*fds));                         \
      who = xrealloc(who, cap * sizeof(*who));                         \
      what = xrealloc(what, cap * sizeof(*what));                      \
    }                                                                  \
    fds[n] = (struct pollfd){.fd = (fd_), .events = (ev_)};           \
    who[n] = (who_);                                                   \
    what[n] = (what_);                                                 \
    n++;                                                               \
  } while (0)
    ADD(sigfd, POLLIN, NULL, 0);
    if (listener >= 0) ADD(listener, POLLIN, NULL, 1);
    if (main_started && !main_exited) {
      for (int i = 0; i < main_proc.nout; i++)
        if (main_proc.out[i] >= 0) ADD(main_proc.out[i], POLLIN, NULL, 2 + i);
      if (main_proc.done) deadline = main_proc.drain_until;
    }
    if (main_exited && !*reported) {
      int64_t d = main_exit_at + 500;
      if (deadline < 0 || d < deadline) deadline = d;
    }
    for (session_t *s = sessions; s; s = s->next) {
      ADD(s->fd, (s->closing ? 0 : POLLIN) | (s->out.len ? POLLOUT : 0), s, 10);
      if (s->has_proc) {
        if (s->out.len < EXEC_HIGH_WATER)
          for (int i = 0; i < s->proc.nout; i++)
            if (s->proc.out[i] >= 0) ADD(s->proc.out[i], POLLIN, s, 20 + i);
        if (s->proc.done && (deadline < 0 || s->proc.drain_until < deadline)) deadline = s->proc.drain_until;
      }
      if (s->pending.len && stdin_target(s) >= 0) ADD(stdin_target(s), POLLOUT, s, 30);
    }
#undef ADD
    int timeout = -1;
    if (deadline >= 0) timeout = deadline > now ? (int)(deadline - now) : 0;
    if (poll(fds, n, timeout) < 0) {
      if (errno == EINTR) continue;
      continue;
    }
    for (int i = 0; i < n; i++) {
      if (!fds[i].revents) continue;
      session_t *s = who[i];
      switch (what[i]) {
      case 0: {
        struct signalfd_siginfo si;
        while (read(sigfd, &si, sizeof(si)) > 0) {}
        reap();
        break;
      }
      case 1: accept_sessions(); break;
      case 2:
      case 3: main_output(what[i] - 2); break;
      case 10:
        if (fds[i].revents & POLLOUT) session_write(s);
        if (fds[i].revents & (POLLIN | POLLHUP | POLLERR)) session_read(s);
        break;
      case 20:
      case 21:
        if (s->has_proc && s->proc.out[what[i] - 20] == fds[i].fd) exec_output(s, what[i] - 20);
        break;
      case 30: write_pending(s); break;
      }
    }
    // the output that came in; a session freed above is not in fds
    for (session_t *s = sessions; s; s = s->next) session_write(s);
  }
}

// ---- warm ----

// The page sends "warm" instead of "go" to a VM that it throws away
// afterwards (web/src/vmworker.js). A restored VM has run nothing when its
// pod starts, and the browser optimises the code of the emulator only after
// it ran for a while. That code is shared by all pods of a tab: the first
// pod of a tab ran on slow code. The work here is what the start of a
// program does: fork, exec, page faults, files, pipes, TCP over loopback
// and a loop that the JIT of the emulator compiles.

#ifndef WARM_CHILDREN
#define WARM_CHILDREN 2
#endif

static void warm_io(int in, int out, char *buf, size_t n, int rounds) {
  for (int i = 0; i < rounds; i++) {
    size_t done = 0;
    while (done < n) {
      ssize_t w = write(out, buf + done, n - done < 4096 ? n - done : 4096);
      if (w <= 0) return;
      ssize_t r = read(in, buf, w);
      if (r <= 0) return;
      done += w;
    }
  }
}

static int cmp_u32(const void *a, const void *b) {
  uint32_t x = *(const uint32_t *)a, y = *(const uint32_t *)b;
  return x < y ? -1 : x > y;
}

// warm_child is the program that warm starts, as /init with the name "warm".
static int warm_child(void) {
  // memory: page faults, then loops over it
  size_t n = 256 << 10;
  uint32_t *m = mmap(NULL, n, PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
  if (m == MAP_FAILED) return 1;
  uint32_t x = 2463534242u, sum = 0;
  for (size_t i = 0; i < n / 4; i++) {
    x ^= x << 13, x ^= x >> 17, x ^= x << 5;
    m[i] = x;
  }
  for (int k = 0; k < 4; k++)
    for (size_t i = 0; i < n / 4; i++) sum = sum * 31 + (m[i] >> (k & 7));
  qsort(m, 2048, sizeof(*m), cmp_u32);
  char *buf = (char *)m;
  // text: what the C library does with strings
  char line[256];
  for (int i = 0; i < 300; i++) {
    int l = snprintf(line, sizeof(line), "%d %s %08x %lld\n", i, "warm", sum + i, (long long)i * 1000003);
    sum += strlen(line) + (memchr(line, '\n', l) != NULL) + strtol(line, NULL, 10);
  }
  // files of the kernel and of the initramfs
  static const char *const files[] = {"/proc/self/status", "/proc/self/maps", "/proc/meminfo", "/proc/stat", "/proc/mounts", "/init"};
  for (size_t i = 0; i < sizeof(files) / sizeof(files[0]); i++) {
    size_t len;
    char *b = read_file(files[i], &len);
    struct stat st;
    stat(files[i], &st);
    free(b);
  }
  int fd = open("/warm.tmp", O_RDWR | O_CREAT | O_TRUNC | O_CLOEXEC, 0600);
  if (fd >= 0) {
    for (int i = 0; i < 16; i++) (void)!write(fd, buf + i * 4096, 4096);
    lseek(fd, 0, SEEK_SET);
    while (read(fd, buf, 16384) > 0) {}
    close(fd);
    unlink("/warm.tmp");
  }
  // a pipe and a TCP connection over loopback
  int p[2];
  if (pipe(p) == 0) {
    warm_io(p[0], p[1], buf, 32768, 1);
    close(p[0]);
    close(p[1]);
  }
  int l = socket(AF_INET, SOCK_STREAM | SOCK_CLOEXEC, 0);
  struct sockaddr_in a = {.sin_family = AF_INET, .sin_addr.s_addr = htonl(INADDR_LOOPBACK)};
  socklen_t alen = sizeof(a);
  if (l >= 0 && bind(l, (struct sockaddr *)&a, sizeof(a)) == 0 && listen(l, 1) == 0 && getsockname(l, (struct sockaddr *)&a, &alen) == 0) {
    int c = socket(AF_INET, SOCK_STREAM | SOCK_CLOEXEC, 0);
    if (c >= 0 && connect(c, (struct sockaddr *)&a, sizeof(a)) == 0) {
      int s = accept(l, NULL, NULL);
      if (s >= 0) {
        warm_io(s, c, buf, 32768, 1);
        close(s);
      }
    }
    if (c >= 0) close(c);
  }
  if (l >= 0) close(l);
  munmap(m, n);
  return sum == 0;
}

static void warm(void) {
  for (int i = 0; i < WARM_CHILDREN; i++) {
    pid_t pid = fork();
    if (pid == 0) {
      char *argv[] = {"warm", NULL}, *envp[] = {NULL};
      execve("/init", argv, envp);
      _exit(127);
    }
    if (pid > 0) waitpid(pid, NULL, 0);
  }
  report("warmed");
}

// ---- stage 0 ----

// What a container gets in /dev, /proc and /sys. Stage 0 mounts them before
// the page saves the VM and moves them into the root of the pod after "go":
// a restored VM does not mount them again.
static const struct {
  const char *fs, *dir, *data;
} mounts[] = {
    {"proc", "/proc", NULL},
    {"sysfs", "/sys", NULL},
    {"devtmpfs", "/dev", NULL},
    {"devpts", "/dev/pts", "newinstance,ptmxmode=0666"},
    {"tmpfs", "/dev/shm", "mode=1777"},
};

static int mount_dev_proc_sys(void) {
  for (size_t i = 0; i < sizeof(mounts) / sizeof(mounts[0]); i++) {
    mkdir(mounts[i].dir, 0755);
    if (mount(mounts[i].fs, mounts[i].dir, mounts[i].fs, 0, mounts[i].data) < 0 && errno != EBUSY)
      return fail("mount %s: %s", mounts[i].dir, errtext(errno));
  }
  // programs open /dev/ptmx, the pairs are in the instance of /dev/pts
  unlink("/dev/ptmx");
  (void)!symlink("pts/ptmx", "/dev/ptmx");
  // What a container runtime puts into /dev: the log files of nginx are
  // links to /dev/stdout and /dev/stderr. A container that does not run as
  // root opens the console through them, too.
  (void)!symlink("/proc/self/fd", "/dev/fd");
  (void)!symlink("/proc/self/fd/0", "/dev/stdin");
  (void)!symlink("/proc/self/fd/1", "/dev/stdout");
  (void)!symlink("/proc/self/fd/2", "/dev/stderr");
  chmod("/dev/console", 0666);
  return 0;
}

// The config of the pod, read by stage 0 from the root of the pod.
static config_t staged_config;
static int staged; // stage 0 set up /dev, /proc, /sys and the config

// from <linux/random.h>, which a musl tool chain may not have
#define RNDADDENTROPY 0x40085203
#define RNDRESEEDCRNG 0x5207

static int add_entropy(const char *hex) {
  size_t n = strlen(hex) / 2;
  struct {
    int entropy_count, buf_size;
    unsigned char buf[256];
  } info;
  if (n > sizeof(info.buf)) n = sizeof(info.buf);
  for (size_t i = 0; i < n; i++) {
    unsigned v;
    if (sscanf(hex + 2 * i, "%2x", &v) != 1) return fail("ttyS1: seed: invalid hex");
    info.buf[i] = v;
  }
  info.entropy_count = n * 8;
  info.buf_size = n;
  int fd = open("/dev/urandom", O_WRONLY | O_CLOEXEC);
  if (fd < 0) return fail("open /dev/urandom: %s", errtext(errno));
  // mix the seed of the page in and reseed, so that two pods from the same
  // state differ
  int rc = ioctl(fd, RNDADDENTROPY, &info);
  if (rc == 0) rc = ioctl(fd, RNDRESEEDCRNG, 0);
  int e = errno;
  close(fd);
  if (rc < 0) return fail("RNDADDENTROPY: %s", errtext(e));
  return 0;
}

static int if_up(int s, const char *name) {
  struct ifreq r = {0};
  snprintf(r.ifr_name, IFNAMSIZ, "%s", name);
  if (ioctl(s, SIOCGIFFLAGS, &r) < 0) return fail("%s: SIOCGIFFLAGS: %s", name, errtext(errno));
  r.ifr_flags |= IFF_UP;
  if (ioctl(s, SIOCSIFFLAGS, &r) < 0) return fail("%s: SIOCSIFFLAGS: %s", name, errtext(errno));
  return 0;
}

static void sockaddr_in_set(struct sockaddr *sa, in_addr_t a) {
  struct sockaddr_in *in = (struct sockaddr_in *)sa;
  memset(in, 0, sizeof(*in));
  in->sin_family = AF_INET;
  in->sin_addr.s_addr = a;
}

static int if_addr(int s, const char *name, unsigned long req, in_addr_t a) {
  struct ifreq r = {0};
  snprintf(r.ifr_name, IFNAMSIZ, "%s", name);
  sockaddr_in_set(&r.ifr_addr, a);
  if (ioctl(s, req, &r) < 0) return fail("%s: ioctl %#lx: %s", name, req, errtext(errno));
  return 0;
}

// setup_network does what ip= on the kernel command line did: the address
// of the pod on eth0 (/24) and the default route to the gateway. lo and eth0
// are up since stage 0.
static int setup_network(const char *ip, const char *gateway) {
  int s = socket(AF_INET, SOCK_DGRAM | SOCK_CLOEXEC, 0);
  if (s < 0) return fail("socket: %s", errtext(errno));
  int rc = -1;
  struct in_addr a, gw;
  if (!ip || !*ip) {
    rc = 0;
    goto out;
  }
  if (!inet_aton(ip, &a)) {
    fail("invalid IP %s", ip);
    goto out;
  }
  if (if_addr(s, "eth0", SIOCSIFADDR, a.s_addr) < 0 || if_addr(s, "eth0", SIOCSIFNETMASK, htonl(0xffffff00)) < 0) goto out;
  if (!gateway || !*gateway) {
    rc = 0;
    goto out;
  }
  if (!inet_aton(gateway, &gw)) {
    fail("invalid gateway %s", gateway);
    goto out;
  }
  struct rtentry rt = {0};
  sockaddr_in_set(&rt.rt_dst, INADDR_ANY);
  sockaddr_in_set(&rt.rt_gateway, gw.s_addr);
  sockaddr_in_set(&rt.rt_genmask, INADDR_ANY);
  rt.rt_flags = RTF_UP | RTF_GATEWAY;
  if (ioctl(s, SIOCADDRT, &rt) < 0) {
    fail("add default route: %s", errtext(errno));
    goto out;
  }
  rc = 0;
out:
  close(s);
  return rc;
}

static int stage0(void) {
  if (mount_dev_proc_sys() < 0) return -1;
  int tty = open("/dev/ttyS1", O_RDWR | O_CLOEXEC);
  if (tty < 0) return fail("open /dev/ttyS1: %s", errtext(errno));
  struct termios t;
  if (tcgetattr(tty, &t) == 0) {
    t.c_lflag &= ~(ECHO | ECHONL);
    tcsetattr(tty, TCSANOW, &t);
  }
  control = tty;
  // eth0 fills its receive queue (1024 buffers) when it comes up: 0.8
  // million instructions, a third of the start of a pod after "go". Done
  // here, it is in the saved VM. The address comes with the pod.
  int s = socket(AF_INET, SOCK_DGRAM | SOCK_CLOEXEC, 0);
  if (s < 0) return fail("socket: %s", errtext(errno));
  int up = if_up(s, "lo") < 0 || if_up(s, "eth0") < 0 ? -1 : 0;
  close(s);
  if (up < 0) return -1;
  report("ready");
  // The page saves the VM here, while init waits for the line.
  char line[1024];
  size_t n = 0;
  for (;;) {
    ssize_t r = read(tty, line + n, sizeof(line) - 1 - n);
    if (r < 0 && errno == EINTR) continue;
    if (r <= 0) return fail("read ttyS1: %s", r < 0 ? errtext(errno) : "EOF");
    n += r;
    if (n >= 5 && !memcmp(line, "warm\n", 5)) {
      // a VM that the page throws away: see warm
      warm();
      n = 0;
      continue;
    }
    if (memchr(line, '\n', n) || n == sizeof(line) - 1) break;
  }
  line[n] = 0;
  long long ms;
  char seed[600];
  if (sscanf(line, "go %lld %599s", &ms, seed) != 2) return fail("ttyS1: unexpected \"%s\"", line);
  struct timeval tv = {.tv_sec = ms / 1000, .tv_usec = (ms % 1000) * 1000};
  if (settimeofday(&tv, NULL) < 0) return fail("settimeofday: %s", errtext(errno));
  if (add_entropy(seed) < 0) return -1;

  mkdir(NEW_ROOT, 0755);
  // cache=loose: only the guest changes the files, it may keep them.
  // ignoreqv: v86 sends version 0 in the qid of every file, and Linux reads
  // and writes such files without the page cache, one 9p request per read
  // call. With the page cache, a file is read once and with readahead.
  if (mount(ROOT_TAG, NEW_ROOT, "9p", 0, "trans=virtio,version=9p2000.L,cache=loose,ignoreqv,msize=262144") < 0) return fail("mount 9p: %s", errtext(errno));
  if (load_config(NEW_ROOT "/.tabernetes/config.json", &staged_config) < 0) return -1;
  if (setup_network(staged_config.ip, staged_config.gateway) < 0) return -1;
  // /dev (with /dev/pts and /dev/shm), /proc and /sys move into the root of
  // the pod
  static const char *const moves[] = {"/dev", "/proc", "/sys"};
  for (size_t i = 0; i < sizeof(moves) / sizeof(moves[0]); i++) {
    char dir[64];
    snprintf(dir, sizeof(dir), NEW_ROOT "%s", moves[i]);
    mkdir(dir, 0755);
    if (mount(moves[i], dir, NULL, MS_MOVE, NULL) < 0) return fail("move %s: %s", moves[i], errtext(errno));
  }
  staged = 1;
  // what switch_root does; the initramfs keeps only this program
  if (chdir(NEW_ROOT) < 0) return fail("chdir: %s", errtext(errno));
  if (mount(".", "/", NULL, MS_MOVE, NULL) < 0) return fail("move the root: %s", errtext(errno));
  if (chroot(".") < 0) return fail("chroot: %s", errtext(errno));
  if (chdir("/") < 0) return fail("chdir: %s", errtext(errno));
  return 0;
}

// ---- the container ----

static int setup_container(config_t *c) {
  if (test_config) {
    if (load_config(test_config, c) < 0) return -1;
    if (!c->args.n) return fail("no command specified");
    return 0;
  }
  if (!staged && mount_dev_proc_sys() < 0) return -1;
  if (control < 0) control = open("/dev/ttyS1", O_WRONLY | O_CLOEXEC);
  console = open("/dev/console", O_WRONLY | O_CLOEXEC);
  if (console < 0) return fail("open /dev/console: %s", errtext(errno));
  dup2(console, 1);
  dup2(console, 2);

  if (staged) *c = staged_config;
  else if (load_config("/.tabernetes/config.json", c) < 0) return -1;
  if (!c->args.n) return fail("no command specified");

  // what the kubelet does for every container; /etc/hosts and
  // /etc/resolv.conf come from the page (web/src/vmworker.js)
  if (c->hostname && *c->hostname) (void)!sethostname(c->hostname, strlen(c->hostname));
  if (c->gateway && *c->gateway) {
    // The network of the page learns the MAC of eth0 from the first packet:
    // send one, so that port-forward finds the pod.
    int u = socket(AF_INET, SOCK_DGRAM | SOCK_CLOEXEC, 0);
    struct sockaddr_in to = {.sin_family = AF_INET, .sin_port = htons(9)};
    if (u >= 0 && inet_aton(c->gateway, &to.sin_addr)) sendto(u, "", 1, 0, (struct sockaddr *)&to, sizeof(to));
    if (u >= 0) close(u);
  }
  return 0;
}

static int start_exec_server(void) {
  listener = socket(AF_INET, SOCK_STREAM | SOCK_NONBLOCK | SOCK_CLOEXEC, 0);
  if (listener < 0) return -1;
  int one = 1;
  setsockopt(listener, SOL_SOCKET, SO_REUSEADDR, &one, sizeof(one));
  struct sockaddr_in a = {.sin_family = AF_INET, .sin_port = htons(exec_port), .sin_addr.s_addr = htonl(INADDR_ANY)};
  if (bind(listener, (struct sockaddr *)&a, sizeof(a)) < 0 || listen(listener, 16) < 0) {
    dprintf(2, "tabernetes init: kubectl exec: %s\n", errtext(errno));
    close(listener);
    listener = -1;
    return -1;
  }
  return 0;
}

// run sets up the container and starts its entrypoint. It returns the exit
// code to report if the entrypoint did not start, with err_msg set.
static int run(void) {
  config_t c = {0};
  if (setup_container(&c) < 0) return 0;
  for (int i = 0; i < c.env.n; i++) strv_add(&ctr_env, c.env.v[i]);
  env_default(&ctr_env, "PATH", DEFAULT_PATH);
  if (c.hostname && *c.hostname) env_default(&ctr_env, "HOSTNAME", c.hostname);
  if (lookup_user(c.user, &ctr_cred) < 0) return 0;
  env_default(&ctr_env, "HOME", ctr_cred.home);
  char *path = look_path(c.args.v[0], env_get(&ctr_env, "PATH"));
  if (!path) return 128;
  if (c.cwd && *c.cwd) ctr_dir = c.cwd;
  int rows = c.tty ? 24 : 0, cols = c.tty ? 80 : 0;
  if (proc_start(&main_proc, path, c.args.v, c.tty, c.stdin_, rows, cols) < 0) return 128;
  main_started = 1;
  main_stdin_once = c.stdin_once;
  start_exec_server();
  report("started");
  return -1;
}

int main(int argc, char **argv) {
  (void)argc;
  // SIGCHLD comes through a signalfd, the others are not for PID 1
  sigset_t set;
  sigemptyset(&set);
  sigaddset(&set, SIGCHLD);
  sigprocmask(SIG_BLOCK, &set, NULL);
  signal(SIGPIPE, SIG_IGN);
  sigfd = signalfd(-1, &set, SFD_NONBLOCK | SFD_CLOEXEC);

  int code = -1;
  int ok = 0;
  test_config = getenv("TABERNETES_INIT_TEST");
  if (test_config) {
    const char *port = getenv("TABERNETES_INIT_PORT");
    if (port) exec_port = atoi(port);
    control = dup(1);
    ok = 1;
  } else if (argv[0] && !strcmp(argv[0], "warm")) return warm_child();
  else if (getpid() == 1 && !strcmp(argv[0], "/init")) ok = stage0() == 0;
  else {
    control = open("/dev/ttyS1", O_WRONLY | O_CLOEXEC);
    ok = 1;
  }
  if (ok) code = run();
  int reported = 0;
  if (code >= 0 || !ok) {
    // The kubelet reports a container that did not start as StartError with
    // exit code 128, and the error in the events, not in the log.
    if (control >= 0) report("error %s", err_msg);
    else dprintf(2, "tabernetes init: %s\n", err_msg);
    report("exit %d", code > 0 ? code : 128);
    reported = 1;
  }
  // The page ends the VM. PID 1 must not exit (the kernel panics), and the
  // kernel cannot power off without ACPI: it would say so in the log.
  loop(&reported);
  return 0;
}
