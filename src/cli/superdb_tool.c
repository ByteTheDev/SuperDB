/* superdb — project manager + offline toolkit for SuperDB.
 *
 * Local-first workflow without a running server:
 *   superdb init [DIR]        scaffold superdb.json + .vscode + .gitignore
 *   superdb install [DIR]     vendor the embeddable C sources into your repo
 *   superdb set/get/del/...   work directly on a .sdb file (no server needed)
 *   superdb dump/restore      JSON export / import (backups, seeding)
 *   superdb inspect FILE.sdb  peek at a database file (magic, size, keys)
 *
 * Server lifecycle:
 *   superdb serve             run superdb-server in the foreground
 *   superdb start/stop/status/logs   background server via a pid file
 *
 * Environment:
 *   superdb doctor            checklist: binaries, config, db health, assoc
 *   superdb assoc             register the .sdb file association (OS-specific)
 *   superdb shell             open superdb-cli against the configured server
 *
 * Pure C99, no dependencies beyond the SuperDB core + libc.
 * Portable: POSIX + Windows (MSVC-safe, no POSIX-only headers).
 */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <stdint.h>
#include <ctype.h>
#include <time.h>

#include "superdb.h"

#ifdef _WIN32
#  ifndef WIN32_LEAN_AND_MEAN
#  define WIN32_LEAN_AND_MEAN
#  endif
#  include <windows.h>
#  include <winsock2.h>
#  include <ws2tcpip.h>
#  include <direct.h>
#  include <process.h>
#  include <io.h>
#  define PATH_SEP '\\'
#  define EXE_SUF ".exe"
#else
#  include <unistd.h>
#  include <errno.h>
#  include <signal.h>
#  include <fcntl.h>
#  include <sys/stat.h>
#  include <sys/types.h>
#  include <sys/wait.h>
#  include <sys/socket.h>
#  include <sys/time.h>
#  include <netinet/in.h>
#  include <arpa/inet.h>
#  include <netdb.h>
#  define PATH_SEP '/'
#  define EXE_SUF ""
#endif

#ifndef PID_FILE_NAME
#define PID_FILE_NAME ".superdb.pid"
#endif
#ifndef LOG_FILE_NAME
#define LOG_FILE_NAME "superdb.log"
#endif
#ifndef CONFIG_FILE_NAME
#define CONFIG_FILE_NAME "superdb.json"
#endif

static const char *g_db = NULL;
static const char *g_host = NULL;
static int g_port = -1;
static const char *g_argv0 = "superdb";

/* ---------------- small utils ---------------- */

static void msg(const char *s) { printf("%s\n", s); fflush(stdout); }
static void warnmsg(const char *s) { fprintf(stderr, "superdb: %s\n", s); }
static void diemsg(const char *s) { fprintf(stderr, "superdb: error: %s\n", s); exit(1); }

static int streq(const char *a, const char *b) { return strcmp(a, b) == 0; }

static int has_flag(int argc, char **argv, const char *name, const char **val) {
    int i;
    size_t n = strlen(name);
    for (i = 0; i < argc; i++) {
        if (strncmp(argv[i], name, n) == 0 &&
            (argv[i][n] == 0 || argv[i][n] == '=')) {
            if (val) *val = argv[i][n] == '=' ? argv[i] + n + 1 : NULL;
            return 1;
        }
    }
    return 0;
}

/* value flag: --name value  or  --name=value. Returns 1 if found. */
static int take_value(int *argc, char ***argv, const char *name, const char **out) {
    int i, j;
    size_t n = strlen(name);
    for (i = 0; i < *argc; i++) {
        char *a = (*argv)[i];
        if (strncmp(a, name, n) == 0 && (a[n] == 0 || a[n] == '=')) {
            if (a[n] == '=') {
                *out = a + n + 1;
                for (j = i; j + 1 < *argc; j++) (*argv)[j] = (*argv)[j + 1];
                (*argc)--;
                return 1;
            }
            if (i + 1 < *argc) {
                *out = (*argv)[i + 1];
                for (j = i; j + 2 < *argc; j++) (*argv)[j] = (*argv)[j + 2];
                *argc -= 2;
                return 1;
            }
            return -1; /* missing value */
        }
    }
    return 0;
}

static int file_exists(const char *p) {
    FILE *f = fopen(p, "rb");
    if (f) { fclose(f); return 1; }
    return 0;
}

static int dir_exists(const char *p) {
#ifdef _WIN32
    DWORD a = GetFileAttributesA(p);
    return a != INVALID_FILE_ATTRIBUTES && (a & FILE_ATTRIBUTE_DIRECTORY);
#else
    struct stat st;
    return stat(p, &st) == 0 && S_ISDIR(st.st_mode);
#endif
}

static long file_size(const char *p) {
    FILE *f = fopen(p, "rb");
    long n;
    if (!f) return -1;
    fseek(f, 0, SEEK_END);
    n = ftell(f);
    fclose(f);
    return n;
}

static void path_join(char *out, size_t cap, const char *a, const char *b) {
    size_t al = strlen(a);
    if (al == 0) {
        snprintf(out, cap, "%s", b);
        return;
    }
    if (a[al - 1] == '/' || a[al - 1] == '\\')
        snprintf(out, cap, "%s%s", a, b);
    else
        snprintf(out, cap, "%s%c%s", a, PATH_SEP, b);
}

/* mkdir -p (handles / and \ separators, skips "C:" drive prefixes). */
static int make_dirs(const char *path) {
    char tmp[4096];
    size_t i, n;
    if (!path || !*path) return -1;
    snprintf(tmp, sizeof tmp, "%s", path);
    n = strlen(tmp);
    for (i = 0; i < n; i++) {
        if (tmp[i] == '/' || tmp[i] == '\\') {
            char save = tmp[i];
            tmp[i] = 0;
            if (tmp[0] && !(i == 2 && tmp[1] == ':')) {
                if (!dir_exists(tmp)) {
#ifdef _WIN32
                    _mkdir(tmp);
#else
                    mkdir(tmp, 0755);
#endif
                }
            }
            tmp[i] = save;
        }
    }
    if (!dir_exists(tmp)) {
#ifdef _WIN32
        if (_mkdir(tmp) != 0 && !dir_exists(tmp)) return -1;
#else
        if (mkdir(tmp, 0755) != 0 && !dir_exists(tmp)) return -1;
#endif
    }
    return 0;
}

static void ensure_parent_dirs(const char *filepath) {
    char tmp[4096];
    char *s;
    snprintf(tmp, sizeof tmp, "%s", filepath);
    s = strrchr(tmp, '/');
#ifdef _WIN32
    {
        char *b = strrchr(tmp, '\\');
        if (b && (!s || b > s)) s = b;
    }
#endif
    if (s && s != tmp) {
        *s = 0;
        if (tmp[0] && strcmp(tmp, ".") != 0) make_dirs(tmp);
    }
}

static int write_file(const char *path, const char *data, int force) {
    FILE *f;
    if (!force && file_exists(path)) return 0; /* skipped */
    ensure_parent_dirs(path);
    f = fopen(path, "wb");
    if (!f) return -1;
    if (data) fwrite(data, 1, strlen(data), f);
    fclose(f);
    return 1; /* written */
}

static char *read_file(const char *path, size_t *out_len) {
    FILE *f = fopen(path, "rb");
    char *buf;
    long n;
    if (!f) return NULL;
    fseek(f, 0, SEEK_END);
    n = ftell(f);
    fseek(f, 0, SEEK_SET);
    if (n < 0) { fclose(f); return NULL; }
    buf = (char *)malloc((size_t)n + 1);
    if (!buf) { fclose(f); return NULL; }
    if (n && fread(buf, 1, (size_t)n, f) != (size_t)n) { free(buf); fclose(f); return NULL; }
    buf[n] = 0;
    fclose(f);
    if (out_len) *out_len = (size_t)n;
    return buf;
}

static int copy_file(const char *src, const char *dst) {
    FILE *in = fopen(src, "rb"), *out;
    char buf[65536];
    size_t n;
    if (!in) return -1;
    ensure_parent_dirs(dst);
    out = fopen(dst, "wb");
    if (!out) { fclose(in); return -1; }
    while ((n = fread(buf, 1, sizeof buf, in)) > 0)
        if (fwrite(buf, 1, n, out) != n) { fclose(in); fclose(out); return -1; }
    fclose(in);
    fclose(out);
    return 0;
}

static int append_line_if_missing(const char *path, const char *line) {
    char *cur;
    FILE *f;
    cur = read_file(path, NULL);
    if (cur && strstr(cur, line)) { free(cur); return 0; }
    free(cur);
    ensure_parent_dirs(path);
    f = fopen(path, "ab");
    if (!f) return -1;
    if (file_size(path) > 0) {
        /* ensure trailing newline before appending */
        char *c2 = read_file(path, NULL);
        if (c2 && strlen(c2) && c2[strlen(c2) - 1] != '\n') fputc('\n', f);
        free(c2);
    }
    fputs(line, f);
    fputc('\n', f);
    fclose(f);
    return 1;
}

/* directory of argv[0] (no trailing sep). Empty string if unknown. */
static void exe_dir(char *out, size_t cap) {
    char tmp[4096];
    char *s;
    out[0] = 0;
#ifdef _WIN32
    if (GetModuleFileNameA(NULL, tmp, (DWORD)sizeof tmp) == 0) return;
#else
    {
        ssize_t n = -1;
#ifdef __linux__
        n = readlink("/proc/self/exe", tmp, sizeof tmp - 1);
#endif
        if (n <= 0) {
            /* fallback: argv0 as given */
            snprintf(tmp, sizeof tmp, "%s", g_argv0);
        } else tmp[n] = 0;
    }
#endif
    snprintf(out, cap, "%s", tmp);
    s = strrchr(out, '/');
#ifdef _WIN32
    {
        char *b = strrchr(out, '\\');
        if (b && (!s || b > s)) s = b;
    }
#endif
    if (s) *s = 0;
    else out[0] = 0;
}

/* ---------------- config resolution ---------------- */

/* naive "key": "string" extractor for flat superdb.json */
static int json_get_string(const char *json, const char *key, char *out, size_t cap) {
    char pat[128];
    const char *p, *q;
    size_t i = 0;
    snprintf(pat, sizeof pat, "\"%s\"", key);
    p = strstr(json, pat);
    if (!p) return 0;
    p = strchr(p + strlen(pat), ':');
    if (!p) return 0;
    p++;
    while (*p && isspace((unsigned char)*p)) p++;
    if (*p != '"') return 0;
    p++;
    while (*p && *p != '"' && i + 1 < cap) {
        if (*p == '\\' && p[1]) {
            p++;
            switch (*p) {
                case 'n': out[i++] = '\n'; break;
                case 't': out[i++] = '\t'; break;
                default: out[i++] = *p; break;
            }
            p++;
        } else out[i++] = *p++;
    }
    out[i] = 0;
    q = p;
    (void)q;
    return 1;
}

static int json_get_int(const char *json, const char *key, long *out) {
    char pat[128];
    const char *p;
    snprintf(pat, sizeof pat, "\"%s\"", key);
    p = strstr(json, pat);
    if (!p) return 0;
    p = strchr(p + strlen(pat), ':');
    if (!p) return 0;
    p++;
    while (*p && isspace((unsigned char)*p)) p++;
    if (!isdigit((unsigned char)*p) && *p != '-') return 0;
    *out = strtol(p, NULL, 10);
    return 1;
}

static char g_cfg_db[4096] = "";
static char g_cfg_host[256] = "";
static long g_cfg_port = -1;
static int g_cfg_loaded = 0;

static void load_config(void) {
    char *j;
    long v;
    if (g_cfg_loaded) return;
    g_cfg_loaded = 1;
    j = read_file(CONFIG_FILE_NAME, NULL);
    if (!j) return;
    if (json_get_string(j, "db", g_cfg_db, sizeof g_cfg_db) == 0) g_cfg_db[0] = 0;
    if (json_get_string(j, "host", g_cfg_host, sizeof g_cfg_host) == 0) g_cfg_host[0] = 0;
    if (json_get_int(j, "port", &v)) g_cfg_port = v;
    free(j);
}

static const char *resolve_db(void) {
    const char *e;
    if (g_db && *g_db) return g_db;
    e = getenv("SUPERDB_DB");
    if (e && *e) return e;
    load_config();
    if (g_cfg_db[0]) return g_cfg_db;
    return "./superdb.sdb";
}

static const char *resolve_host(void) {
    const char *e;
    if (g_host && *g_host) return g_host;
    e = getenv("SUPERDB_HOST");
    if (e && *e) return e;
    load_config();
    if (g_cfg_host[0]) return g_cfg_host;
    return "127.0.0.1";
}

static int resolve_port(void) {
    const char *e;
    long v;
    if (g_port > 0) return g_port;
    e = getenv("SUPERDB_PORT");
    if (e && *e) { v = strtol(e, NULL, 10); if (v > 0 && v < 65536) return (int)v; }
    load_config();
    if (g_cfg_port > 0 && g_cfg_port < 65536) return (int)g_cfg_port;
    return 7381;
}

/* ---------------- source + helper binary lookup ---------------- */

static const char * const kVendorFiles[] = {
    "include/superdb.h",
    "src/core/superdb.c",
    "src/core/superdb_int.h",
    "src/core/superdb_util.c", "src/core/superdb_util.h",
    "src/core/superdb_crc.c",  "src/core/superdb_crc.h",
    "src/core/superdb_store.c", "src/core/superdb_store.h",
    "src/core/superdb_log.c",  "src/core/superdb_log.h",
    NULL
};

static int is_source_root(const char *dir) {
    char a[4096], b[4096];
    path_join(a, sizeof a, dir, "include/superdb.h");
    path_join(b, sizeof b, dir, "src/core/superdb.c");
    return file_exists(a) && file_exists(b);
}

/* find the SuperDB source checkout (for `install`). */
static int find_source_root(char *out, size_t cap) {
    const char *from = getenv("SUPERDB_SRC");
    char probe[4096];
    int up;
    if (from && *from && is_source_root(from)) {
        snprintf(out, cap, "%s", from);
        return 1;
    }
    /* walk up from cwd */
    for (up = 0; up < 6; up++) {
        if (up == 0) snprintf(probe, sizeof probe, ".");
        else snprintf(probe, sizeof probe, "%.*s", up * 3 - 1, "../../../../../../..");
        if (is_source_root(probe)) {
            snprintf(out, cap, "%s", probe);
            return 1;
        }
    }
    /* exe-relative */
    {
        char ed[4096];
        exe_dir(ed, sizeof ed);
        if (ed[0]) {
            path_join(probe, sizeof probe, ed, ".");
            if (is_source_root(probe)) { snprintf(out, cap, "%s", probe); return 1; }
            path_join(probe, sizeof probe, ed, "..");
            if (is_source_root(probe)) { snprintf(out, cap, "%s", probe); return 1; }
        }
    }
    return 0;
}

/* locate superdb-server / superdb-cli next to us, in ./bin, or on PATH. */
static void find_helper(const char *base, char *out, size_t cap) {
    char ed[4096], cand[4096];
    exe_dir(ed, sizeof ed);
#ifdef _WIN32
    if (ed[0]) {
        snprintf(cand, sizeof cand, "%s\\%s.exe", ed, base);
        if (file_exists(cand)) { snprintf(out, cap, "%s", cand); return; }
    }
    snprintf(cand, sizeof cand, "bin\\%s.exe", base);
    if (file_exists(cand)) { snprintf(out, cap, "%s", cand); return; }
    snprintf(cand, sizeof cand, "%s.exe", base);
    if (file_exists(cand)) { snprintf(out, cap, "%s", cand); return; }
    snprintf(out, cap, "%s.exe", base);
#else
    if (ed[0]) {
        path_join(cand, sizeof cand, ed, base);
        if (file_exists(cand)) { snprintf(out, cap, "%s", cand); return; }
    }
    snprintf(cand, sizeof cand, "bin/%s", base);
    if (file_exists(cand)) { snprintf(out, cap, "%s", cand); return; }
    if (file_exists(base)) { snprintf(out, cap, "./%s", base); return; }
    snprintf(out, cap, "%s", base);
#endif
}

/* ---------------- base64 (for dump/restore) ---------------- */

static const char kB64[] =
    "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";

static char *b64_encode(const unsigned char *in, size_t n) {
    size_t o = ((n + 2) / 3) * 4;
    char *out = (char *)malloc(o + 1);
    size_t i, j = 0;
    if (!out) return NULL;
    for (i = 0; i < n; i += 3) {
        unsigned v = in[i] << 16;
        int rem = (int)(n - i);
        if (rem > 1) v |= in[i + 1] << 8;
        if (rem > 2) v |= in[i + 2];
        out[j++] = kB64[(v >> 18) & 63];
        out[j++] = kB64[(v >> 12) & 63];
        out[j++] = rem > 1 ? kB64[(v >> 6) & 63] : '=';
        out[j++] = rem > 2 ? kB64[v & 63] : '=';
    }
    out[j] = 0;
    return out;
}

static int b64_val(char c) {
    if (c >= 'A' && c <= 'Z') return c - 'A';
    if (c >= 'a' && c <= 'z') return c - 'a' + 26;
    if (c >= '0' && c <= '9') return c - '0' + 52;
    if (c == '+') return 62;
    if (c == '/') return 63;
    return -1;
}

static unsigned char *b64_decode(const char *in, size_t *out_len) {
    size_t n = strlen(in), i, j = 0;
    unsigned char *out;
    if (n % 4 != 0) return NULL;
    out = (unsigned char *)malloc(n / 4 * 3 + 1);
    if (!out) return NULL;
    for (i = 0; i < n; i += 4) {
        int a = b64_val(in[i]), b = b64_val(in[i + 1]);
        int c = in[i + 2] == '=' ? 0 : b64_val(in[i + 2]);
        int d = in[i + 3] == '=' ? 0 : b64_val(in[i + 3]);
        if (a < 0 || b < 0 || (in[i + 2] != '=' && c < 0) || (in[i + 3] != '=' && d < 0)) {
            free(out);
            return NULL;
        }
        out[j++] = (unsigned char)((a << 2) | (b >> 4));
        if (in[i + 2] != '=') out[j++] = (unsigned char)((b << 4) | (c >> 2));
        if (in[i + 3] != '=') out[j++] = (unsigned char)((c << 6) | d);
    }
    *out_len = j;
    return out;
}

/* ---------------- JSON string escape/unescape ---------------- */

static char *json_escape(const char *s) {
    size_t n = strlen(s), i, j = 0;
    char *o = (char *)malloc(n * 6 + 1);
    static const char *hex = "0123456789abcdef";
    if (!o) return NULL;
    for (i = 0; i < n; i++) {
        unsigned char c = (unsigned char)s[i];
        if (c == '"' || c == '\\') { o[j++] = '\\'; o[j++] = (char)c; }
        else if (c == '\n') { o[j++] = '\\'; o[j++] = 'n'; }
        else if (c == '\r') { o[j++] = '\\'; o[j++] = 'r'; }
        else if (c == '\t') { o[j++] = '\\'; o[j++] = 't'; }
        else if (c < 0x20) {
            o[j++] = '\\'; o[j++] = 'u'; o[j++] = '0'; o[j++] = '0';
            o[j++] = hex[c >> 4]; o[j++] = hex[c & 15];
        } else o[j++] = (char)c;
    }
    o[j] = 0;
    return o;
}

/* parse a "..." JSON string starting at *pp (which points at '"').
 * Advances *pp past the closing quote. Returns malloc'd UTF-8 (\\uXXXX
 * above 0x7F is replaced by '?'; keys are ASCII in practice). */
static char *json_parse_string(const char **pp) {
    const char *p = *pp;
    char *o, *w;
    if (*p != '"') return NULL;
    p++;
    o = (char *)malloc(strlen(p) + 1);
    if (!o) return NULL;
    w = o;
    while (*p && *p != '"') {
        if (*p == '\\' && p[1]) {
            p++;
            switch (*p) {
                case 'n': *w++ = '\n'; break;
                case 'r': *w++ = '\r'; break;
                case 't': *w++ = '\t'; break;
                case 'u': {
                    unsigned v = 0;
                    int k;
                    for (k = 0; k < 4; k++) {
                        char h = p[1 + k];
                        v *= 16;
                        if (h >= '0' && h <= '9') v += (unsigned)(h - '0');
                        else if (h >= 'a' && h <= 'f') v += (unsigned)(h - 'a' + 10);
                        else if (h >= 'A' && h <= 'F') v += (unsigned)(h - 'A' + 10);
                    }
                    p += 4;
                    *w++ = v < 0x80 ? (char)v : '?';
                    break;
                }
                default: *w++ = *p; break;
            }
            p++;
        } else *w++ = *p++;
    }
    if (*p != '"') { free(o); return NULL; }
    *w = 0;
    *pp = p + 1;
    return o;
}

/* ---------------- db open helper ---------------- */

static superdb_t *open_db(const char *path) {
    superdb_t *db = superdb_open(path, NULL);
    if (!db) {
        fprintf(stderr, "superdb: cannot open db '%s'\n", path);
        exit(1);
    }
    return db;
}

/* strip a single trailing '*' (server KEYS semantics: "*" == all). */
static const char *strip_star(const char *pat, char *buf, size_t cap) {
    size_t n;
    if (!pat || !*pat || streq(pat, "*")) return NULL;
    n = strlen(pat);
    if (pat[n - 1] == '*') n--;
    if (n >= cap) n = cap - 1;
    memcpy(buf, pat, n);
    buf[n] = 0;
    return buf;
}

/* ---------------- offline key commands ---------------- */

static int cmd_get(int argc, char **argv) {
    superdb_t *db;
    size_t len = 0;
    void *v;
    if (argc != 1) { warnmsg("usage: superdb get KEY"); return 2; }
    db = open_db(resolve_db());
    v = superdb_get(db, argv[0], &len);
    if (!v) {
        printf("(nil)\n");
        superdb_close(db);
        return 1;
    }
    if (len) fwrite(v, 1, len, stdout);
    fputc('\n', stdout);
    superdb_free(v);
    superdb_close(db);
    return 0;
}

static int cmd_set(int argc, char **argv) {
    superdb_t *db;
    int i, rc;
    long secs = 0;
    int has_ex = 0;
    if (argc < 2) { warnmsg("usage: superdb set KEY VALUE [EX seconds]"); return 2; }
    for (i = 2; i < argc; i++) {
        if ((streq(argv[i], "EX") || streq(argv[i], "ex")) && i + 1 < argc) {
            secs = strtol(argv[++i], NULL, 10);
            if (secs <= 0) { warnmsg("EX needs a positive number of seconds"); return 2; }
            has_ex = 1;
        } else { warnmsg("usage: superdb set KEY VALUE [EX seconds]"); return 2; }
    }
    db = open_db(resolve_db());
    if (has_ex)
        rc = superdb_set_ex(db, argv[0], argv[1], strlen(argv[1]), (int64_t)secs * 1000);
    else
        rc = superdb_set(db, argv[0], argv[1], strlen(argv[1]));
    superdb_close(db);
    if (rc != 0) { warnmsg("set failed"); return 1; }
    msg("OK");
    return 0;
}

static int cmd_del(int argc, char **argv) {
    superdb_t *db;
    int i, n = 0;
    if (argc < 1) { warnmsg("usage: superdb del KEY [KEY ...]"); return 2; }
    db = open_db(resolve_db());
    for (i = 0; i < argc; i++) n += superdb_del(db, argv[i]);
    superdb_close(db);
    printf("(integer) %d\n", n);
    return 0;
}

static int cmd_exists(int argc, char **argv) {
    superdb_t *db;
    int i, n = 0;
    if (argc < 1) { warnmsg("usage: superdb exists KEY [KEY ...]"); return 2; }
    db = open_db(resolve_db());
    for (i = 0; i < argc; i++) n += superdb_exists(db, argv[i]);
    superdb_close(db);
    printf("(integer) %d\n", n);
    return 0;
}

static int cmd_keys(int argc, char **argv) {
    superdb_t *db;
    superdb_keys_t ks;
    char pfx[65536];
    const char *p = NULL;
    size_t i;
    if (argc > 1) { warnmsg("usage: superdb keys [pattern]  (e.g. *, user:*)"); return 2; }
    if (argc == 1) p = strip_star(argv[0], pfx, sizeof pfx);
    db = open_db(resolve_db());
    ks = superdb_keys(db, p);
    if (ks.count == 0) msg("(empty array)");
    for (i = 0; i < ks.count; i++) printf("%s\n", ks.keys[i]);
    superdb_free_keys(&ks);
    superdb_close(db);
    return 0;
}

static int cmd_incr(int argc, char **argv) {
    superdb_t *db;
    int64_t by = 1, r;
    int ok = 0;
    char *end = NULL;
    if (argc < 1 || argc > 2) { warnmsg("usage: superdb incr KEY [BY]"); return 2; }
    if (argc == 2) {
        by = strtoll(argv[1], &end, 10);
        if (!end || *end) { warnmsg("BY must be an integer"); return 2; }
    }
    db = open_db(resolve_db());
    r = superdb_incr(db, argv[0], by, &ok);
    superdb_close(db);
    if (!ok) { warnmsg("value is not an integer"); return 1; }
    printf("(integer) %lld\n", (long long)r);
    return 0;
}

static int cmd_expire(int argc, char **argv) {
    superdb_t *db;
    long secs;
    int r;
    if (argc != 2) { warnmsg("usage: superdb expire KEY SECONDS"); return 2; }
    secs = strtol(argv[1], NULL, 10);
    if (secs <= 0) { warnmsg("SECONDS must be positive"); return 2; }
    db = open_db(resolve_db());
    r = superdb_expire(db, argv[0], (int64_t)secs * 1000);
    superdb_close(db);
    printf("(integer) %d\n", r ? 1 : 0);
    return 0;
}

static int cmd_ttl(int argc, char **argv, int ms) {
    superdb_t *db;
    int64_t t;
    if (argc != 1) { warnmsg(ms ? "usage: superdb pttl KEY" : "usage: superdb ttl KEY"); return 2; }
    db = open_db(resolve_db());
    t = superdb_ttl(db, argv[0]);
    superdb_close(db);
    if (!ms) {
        if (t > 0) t = (t + 999) / 1000; /* ceil to seconds, like the server */
        printf("(integer) %lld\n", (long long)t);
    } else {
        printf("(integer) %lld\n", (long long)t);
    }
    return 0;
}

static int cmd_dbsize(int argc, char **argv) {
    superdb_t *db;
    (void)argv;
    if (argc != 0) { warnmsg("usage: superdb dbsize"); return 2; }
    db = open_db(resolve_db());
    printf("(integer) %lu\n", (unsigned long)superdb_count(db));
    superdb_close(db);
    return 0;
}

static int cmd_info(int argc, char **argv) {
    superdb_t *db;
    const char *path;
    long sz;
    (void)argv;
    if (argc != 0) { warnmsg("usage: superdb info"); return 2; }
    path = resolve_db();
    db = open_db(path);
    printf("version: %s\n", SUPERDB_VERSION);
    printf("db: %s\n", path);
    sz = file_size(path);
    printf("file_bytes: %ld\n", sz);
    printf("keys: %lu\n", (unsigned long)superdb_count(db));
    printf("log_bytes: %lu\n", (unsigned long)superdb_log_size(db));
    superdb_close(db);
    return 0;
}

static int cmd_compact(int argc, char **argv) {
    superdb_t *db;
    (void)argv;
    if (argc != 0) { warnmsg("usage: superdb compact"); return 2; }
    db = open_db(resolve_db());
    if (superdb_compact(db) != 0) {
        fprintf(stderr, "superdb: compact failed: %s\n", superdb_err(db));
        superdb_close(db);
        return 1;
    }
    superdb_close(db);
    msg("OK");
    return 0;
}

static int cmd_flushall(int argc, char **argv) {
    superdb_t *db;
    (void)argv;
    if (argc != 0) { warnmsg("usage: superdb flushall [--yes]"); return 2; }
    if (!has_flag(argc, argv, "--yes", NULL) && !has_flag(argc, argv, "-y", NULL)) {
        warnmsg("this deletes everything. Re-run with --yes to confirm.");
        return 2;
    }
    db = open_db(resolve_db());
    /* filter out the flag before passing on (no keys expected anyway) */
    if (superdb_flushall(db) != 0) {
        fprintf(stderr, "superdb: flushall failed: %s\n", superdb_err(db));
        superdb_close(db);
        return 1;
    }
    superdb_close(db);
    msg("OK");
    return 0;
}

/* ---------------- dump / restore ---------------- */

static int cmd_dump(int argc, char **argv) {
    const char *out = NULL;
    const char *dbpath;
    superdb_t *db;
    superdb_keys_t ks;
    FILE *f = stdout;
    size_t i;
    int rc = 0, j;
    for (j = 0; j < argc; j++) {
        if (streq(argv[j], "--out") && j + 1 < argc) out = argv[++j];
        else if (strncmp(argv[j], "--out=", 6) == 0) out = argv[j] + 6;
        else { warnmsg("usage: superdb dump [--out FILE.json]"); return 2; }
    }
    dbpath = resolve_db();
    db = open_db(dbpath);
    if (out && !streq(out, "-")) {
        f = fopen(out, "wb");
        if (!f) {
            fprintf(stderr, "superdb: cannot write '%s'\n", out);
            superdb_close(db);
            return 1;
        }
    }
    ks = superdb_keys(db, NULL);
    fprintf(f, "{\"superdb\":1,\"keys\":[");
    for (i = 0; i < ks.count; i++) {
        size_t vlen = 0;
        void *v = superdb_get(db, ks.keys[i], &vlen);
        int64_t ttl = superdb_ttl(db, ks.keys[i]);
        char *ke = json_escape(ks.keys[i]);
        char *b64 = b64_encode(v ? (const unsigned char *)v : (const unsigned char *)"", v ? vlen : 0);
        if (!ke || !b64) { rc = 1; superdb_free(ke); free(b64); superdb_free(v); break; }
        fprintf(f, "%s{\"k\":\"%s\"", i ? "," : "", ke);
        if (ttl > 0) fprintf(f, ",\"ttl_ms\":%lld", (long long)ttl);
        fprintf(f, ",\"b64\":\"%s\"}", b64);
        free(ke);
        free(b64);
        superdb_free(v);
    }
    fprintf(f, "]}\n");
    superdb_free_keys(&ks);
    superdb_close(db);
    if (f != stdout) {
        fclose(f);
        if (rc == 0) printf("dumped to %s\n", out);
    }
    return rc;
}

static int cmd_restore(int argc, char **argv) {
    const char *in = NULL;
    const char *dbpath;
    int flush = 0, j, n = 0;
    char *json = NULL;
    const char *p;
    superdb_t *db = NULL;
    for (j = 0; j < argc; j++) {
        if (streq(argv[j], "--in") && j + 1 < argc) in = argv[++j];
        else if (strncmp(argv[j], "--in=", 5) == 0) in = argv[j] + 5;
        else if (streq(argv[j], "--flush")) flush = 1;
        else { warnmsg("usage: superdb restore --in FILE.json [--flush]"); return 2; }
    }
    if (!in) { warnmsg("usage: superdb restore --in FILE.json [--flush]"); return 2; }
    json = read_file(in, NULL);
    if (!json) { fprintf(stderr, "superdb: cannot read '%s'\n", in); return 1; }
    dbpath = resolve_db();
    db = open_db(dbpath);
    if (flush) superdb_flushall(db);
    superdb_batch_begin(db);
    p = strstr(json, "\"keys\"");
    if (p) p = strchr(p, '[');
    if (p) {
        p++;
        for (;;) {
            const char *o;
            char *k = NULL, *b64 = NULL;
            int64_t ttl = -1;
            while (*p && *p != '{' && *p != ']') p++;
            if (*p != '{') break;
            o = p + 1;
            /* parse one {...} object: look for "k", "ttl_ms", "b64" */
            while (*o && *o != '}') {
                while (*o && (isspace((unsigned char)*o) || *o == ',')) o++;
                if (*o != '"') { o++; continue; }
                {
                    const char *q = o;
                    char *name = json_parse_string(&q);
                    o = q;
                    if (!name) break;
                    while (*o && (isspace((unsigned char)*o) || *o == ':')) o++;
                    if (streq(name, "k") && *o == '"') {
                        const char *q2 = o;
                        free(k);
                        k = json_parse_string(&q2);
                        o = q2 ? q2 : o + 1;
                    } else if (streq(name, "b64") && *o == '"') {
                        const char *q2 = o;
                        free(b64);
                        b64 = json_parse_string(&q2);
                        o = q2 ? q2 : o + 1;
                    } else if (streq(name, "ttl_ms")) {
                        ttl = strtoll(o, (char **)&o, 10);
                    } else {
                        /* skip unknown value */
                        if (*o == '"') {
                            const char *q2 = o;
                            char *tmp = json_parse_string(&q2);
                            free(tmp);
                            if (q2) o = q2;
                        } else {
                            while (*o && *o != ',' && *o != '}') o++;
                        }
                    }
                    free(name);
                }
            }
            if (*o == '}') p = o + 1; else p = o;
            if (k && b64) {
                size_t vlen = 0;
                unsigned char *v = b64_decode(b64, &vlen);
                if (v || vlen == 0) {
                    int rc;
                    if (ttl > 0)
                        rc = superdb_set_ex(db, k, v ? v : (const unsigned char *)"", vlen, ttl);
                    else
                        rc = superdb_set(db, k, v ? v : (const unsigned char *)"", vlen);
                    if (rc == 0) n++;
                    free(v);
                }
            }
            free(k);
            free(b64);
        }
    }
    free(json);
    if (superdb_batch_end(db) != 0) {
        fprintf(stderr, "superdb: restore sync failed: %s\n", superdb_err(db));
        superdb_close(db);
        return 1;
    }
    superdb_close(db);
    printf("restored %d key(s) into %s\n", n, dbpath);
    return 0;
}

/* ---------------- inspect ---------------- */

static int cmd_inspect(int argc, char **argv) {
    const char *path = resolve_db();
    FILE *f;
    long sz;
    unsigned char magic[4];
    superdb_t *db;
    superdb_keys_t ks;
    size_t i, show;
    if (argc > 1) { warnmsg("usage: superdb inspect [FILE.sdb]"); return 2; }
    if (argc == 1) path = argv[0];
    sz = file_size(path);
    if (sz < 0) { fprintf(stderr, "superdb: no such file '%s'\n", path); return 1; }
    printf("file: %s\n", path);
    printf("size: %ld bytes\n", sz);
    f = fopen(path, "rb");
    if (f) {
        if (fread(magic, 1, 4, f) == 4)
            printf("magic: %c%c%c%c (%s)\n", magic[0], magic[1], magic[2], magic[3],
                   memcmp(magic, "SDB1", 4) == 0 ? "SuperDB v1" : "UNKNOWN");
        fclose(f);
    }
    if (memcmp(magic, "SDB1", 4) != 0) {
        warnmsg("not a SuperDB file (bad magic).");
        return 1;
    }
    db = open_db(path);
    ks = superdb_keys(db, NULL);
    printf("keys: %lu\n", (unsigned long)ks.count);
    printf("log_bytes: %lu\n", (unsigned long)superdb_log_size(db));
    show = ks.count < 20 ? ks.count : 20;
    for (i = 0; i < show; i++) {
        size_t vlen = 0;
        void *v = superdb_get(db, ks.keys[i], &vlen);
        int64_t ttl = superdb_ttl(db, ks.keys[i]);
        printf("  %s (%lu bytes%s", ks.keys[i], (unsigned long)vlen,
               ttl > 0 ? ", ttl" : "");
        if (ttl > 0) printf(" %lldms", (long long)ttl);
        printf(")\n");
        superdb_free(v);
    }
    if (ks.count > show) printf("  ... and %lu more\n", (unsigned long)(ks.count - show));
    superdb_free_keys(&ks);
    superdb_close(db);
    return 0;
}

/* ---------------- init: scaffold a repo ---------------- */

static const char kInitGitignore[] = "*.sdb\n*.sdb.tmp\n.superdb.pid\nsuperdb.log\n";

static const char kInitExampleC[] =
    "/* SuperDB example — local-first key/value store (scaffolded by `superdb init`).\n"
    " *\n"
    " * Compile against the vendored sources (see third_party/superdb/README.md):\n"
    " *   gcc -std=c99 -O2 -Ithird_party/superdb/include superdb_example.c\n"
    " *       third_party/superdb/src/*.c -o superdb_example\n"
    " */\n"
    "#include <stdio.h>\n"
    "#include \"superdb.h\"\n"
    "\n"
    "int main(void) {\n"
    "    superdb_t *db = superdb_open(\"./superdb.sdb\", NULL);\n"
    "    char *v;\n"
    "    if (!db) { printf(\"open failed\\n\"); return 1; }\n"
    "    superdb_set_str(db, \"greeting\", \"hello, local-first!\");\n"
    "    v = superdb_get_str(db, \"greeting\");\n"
    "    printf(\"greeting = %s\\n\", v ? v : \"(nil)\");\n"
    "    superdb_free(v);\n"
    "    superdb_close(db);\n"
    "    return 0;\n"
    "}\n";

static int cmd_init(int argc, char **argv) {
    const char *dir = ".";
    const char *dbp = "./superdb.sdb";
    const char *host = "127.0.0.1";
    const char *port = "7381";
    const char *v;
    int force = 0, no_vscode = 0, j;
    char cfg[4096], gi[4096], vs[4096], ve[4096], ex[4096], buf[2048];
    char cfgdata[1024];
    if (take_value(&argc, &argv, "--db", &v) > 0) dbp = v;
    if (take_value(&argc, &argv, "--host", &v) > 0) host = v;
    if (take_value(&argc, &argv, "--port", &v) > 0) port = v;
    for (j = 0; j < argc; j++) {
        if (streq(argv[j], "--force") || streq(argv[j], "-f")) force = 1;
        else if (streq(argv[j], "--no-vscode")) no_vscode = 1;
        else if (argv[j][0] == '-') { warnmsg("usage: superdb init [DIR] [--db PATH] [--host ADDR] [--port N] [--force] [--no-vscode]"); return 2; }
        else dir = argv[j];
    }
    if (make_dirs(dir) != 0) { fprintf(stderr, "superdb: cannot create '%s'\n", dir); return 1; }

    path_join(cfg, sizeof cfg, dir, CONFIG_FILE_NAME);
    snprintf(cfgdata, sizeof cfgdata,
             "{\n  \"db\": \"%s\",\n  \"host\": \"%s\",\n  \"port\": %s\n}\n",
             dbp, host, port);
    if (!force && file_exists(cfg)) {
        printf("keep %s (exists, use --force to overwrite)\n", cfg);
    } else {
        ensure_parent_dirs(cfg);
        { FILE *f = fopen(cfg, "wb"); if (!f) { warnmsg("cannot write config"); return 1; } fputs(cfgdata, f); fclose(f); }
        printf("wrote %s\n", cfg);
    }

    path_join(gi, sizeof gi, dir, ".gitignore");
    if (append_line_if_missing(gi, "*.sdb") >= 0) { append_line_if_missing(gi, "*.sdb.tmp"); append_line_if_missing(gi, PID_FILE_NAME); append_line_if_missing(gi, LOG_FILE_NAME); }
    printf("updated %s\n", gi);

    path_join(ex, sizeof ex, dir, "superdb_example.c");
    switch (write_file(ex, kInitExampleC, force)) {
        case 1: printf("wrote %s\n", ex); break;
        case 0: printf("keep %s (exists)\n", ex); break;
        default: warnmsg("cannot write example"); return 1;
    }

    if (!no_vscode) {
        path_join(vs, sizeof vs, dir, ".vscode/settings.json");
        snprintf(buf, sizeof buf,
                 "{\n"
                 "  \"files.associations\": { \"*.sdb\": \"binary\" },\n"
                 "  \"superdb.db\": \"%s\",\n"
                 "  \"superdb.host\": \"%s\",\n"
                 "  \"superdb.port\": %s\n"
                 "}\n", dbp, host, port);
        switch (write_file(vs, buf, force)) {
            case 1: printf("wrote %s\n", vs); break;
            case 0: printf("keep %s (exists)\n", vs); break;
            default: warnmsg("cannot write vscode settings"); return 1;
        }
        path_join(ve, sizeof ve, dir, ".vscode/extensions.json");
        switch (write_file(ve, "{\n  \"recommendations\": [\"superdb.vscode-superdb\"]\n}\n", force)) {
            case 1: printf("wrote %s\n", ve); break;
            case 0: printf("keep %s (exists)\n", ve); break;
            default: warnmsg("cannot write vscode extensions"); return 1;
        }
    }

    printf("\nSuperDB is ready in %s\n", dir);
    printf("  db:   %s\n", dbp);
    printf("  next: superdb set hello world   # offline, no server needed\n");
    printf("        superdb serve              # start the TCP server\n");
    (void)kInitGitignore;
    return 0;
}

/* ---------------- install: vendor the C sources ---------------- */

static const char kVendorReadme[] =
    "# SuperDB (vendored)\n"
    "\n"
    "Embeddable key-value store, vendored by `superdb install`.\n"
    "Upstream: https://github.com/superdb/superdb (local checkout used).\n"
    "Only `src/core/` is vendored — the embeddable engine (no server,\n"
    "protocol, or CLI code).\n"
    "\n"
    "## Use it\n"
    "\n"
    "Add all `src/core/*.c` to your build and put `include/` on the header path:\n"
    "\n"
    "```sh\n"
    "# plain gcc\n"
    "gcc -std=c99 -O2 -Ithird_party/superdb/include your_app.c \\\n"
    "    third_party/superdb/src/core/*.c -o app\n"
    "```\n"
    "\n"
    "See `superdb_example.c` in the repo root and the upstream\n"
    "`include/superdb.h` for the full API.\n"
    "\n"
    "## Refresh\n"
    "\n"
    "Re-run `superdb install --force` from a newer SuperDB checkout.\n"
    "\n"
    "## CMake\n"
    "\n"
    "```cmake\n"
    "file(GLOB SUPERDB_CORE third_party/superdb/src/core/*.c)\n"
    "add_library(superdb STATIC ${SUPERDB_CORE})\n"
    "target_include_directories(superdb PUBLIC third_party/superdb/include)\n"
    "```\n"
    "\n"
    "## Make\n"
    "\n"
    "```make\n"
    "SUPERDB_CORE = $(wildcard third_party/superdb/src/core/*.c)\n"
    "CFLAGS += -Ithird_party/superdb/include\n"
    "```\n";

static int cmd_install(int argc, char **argv) {
    const char *dir = ".";
    const char *from = NULL;
    const char *v;
    int force = 0, j, r;
    char srcroot[4096], dest[4096], s[4096], d[4096], rm[4096];
    const char * const *f;
    int copied = 0;
    r = take_value(&argc, &argv, "--from", &v);
    if (r > 0) from = v;
    else if (r < 0) { warnmsg("usage: superdb install [DIR] [--from SRC] [--force]"); return 2; }
    for (j = 0; j < argc; j++) {
        if (streq(argv[j], "--force") || streq(argv[j], "-f")) force = 1;
        else if (argv[j][0] == '-') { warnmsg("usage: superdb install [DIR] [--from SRC] [--force]"); return 2; }
        else dir = argv[j];
    }
    if (from) {
        if (!is_source_root(from)) {
            fprintf(stderr, "superdb: '%s' is not a SuperDB checkout\n", from);
            return 1;
        }
        snprintf(srcroot, sizeof srcroot, "%s", from);
    } else if (!find_source_root(srcroot, sizeof srcroot)) {
        warnmsg("cannot find SuperDB sources. Re-run with --from <path-to-superdb>.");
        return 1;
    }
    path_join(dest, sizeof dest, dir, "third_party/superdb");
    for (f = kVendorFiles; *f; f++) {
        path_join(s, sizeof s, srcroot, *f);
        path_join(d, sizeof d, dest, *f);
        if (!force && file_exists(d)) continue;
        if (copy_file(s, d) != 0) {
            fprintf(stderr, "superdb: copy failed: %s\n", s);
            return 1;
        }
        copied++;
    }
    path_join(rm, sizeof rm, dest, "README.md");
    write_file(rm, kVendorReadme, force ? 1 : (file_exists(rm) ? 0 : 1));
    printf("vendored %d file(s) into %s (from %s)\n", copied, dest, srcroot);
    printf("build: gcc -std=c99 -O2 -I%s/include <app>.c %s/src/core/*.c\n", dest, dest);
    return 0;
}

/* ---------------- server lifecycle ---------------- */

static int tcp_probe(const char *host, int port) {
#ifdef _WIN32
    SOCKET s;
    struct sockaddr_in a;
    WSADATA wd;
    u_long ip;
    if (WSAStartup(MAKEWORD(2, 2), &wd) != 0) return 0;
    s = socket(AF_INET, SOCK_STREAM, 0);
    if (s == INVALID_SOCKET) { WSACleanup(); return 0; }
    memset(&a, 0, sizeof a);
    a.sin_family = AF_INET;
    a.sin_port = htons((unsigned short)port);
    ip = inet_addr(host);
    if (ip == INADDR_NONE) {
        struct hostent *h = gethostbyname(host);
        if (!h) { closesocket(s); WSACleanup(); return 0; }
        memcpy(&a.sin_addr, h->h_addr, (size_t)h->h_length);
    } else a.sin_addr.s_addr = ip;
    /* short timeout so `status` never hangs */
    {
        DWORD tv = 500;
        setsockopt(s, SOL_SOCKET, SO_RCVTIMEO, (const char *)&tv, sizeof tv);
    }
    {
        int ok = connect(s, (struct sockaddr *)&a, sizeof a) == 0;
        closesocket(s);
        WSACleanup();
        return ok;
    }
#else
    int s, ok;
    struct sockaddr_in a;
    struct timeval tv;
    uint32_t ip = inet_addr(host);
    s = socket(AF_INET, SOCK_STREAM, 0);
    if (s < 0) return 0;
    tv.tv_sec = 0;
    tv.tv_usec = 500000;
    setsockopt(s, SOL_SOCKET, SO_RCVTIMEO, &tv, sizeof tv);
    setsockopt(s, SOL_SOCKET, SO_SNDTIMEO, &tv, sizeof tv);
    memset(&a, 0, sizeof a);
    a.sin_family = AF_INET;
    a.sin_port = htons((unsigned short)port);
    if (ip == INADDR_NONE) {
        struct hostent *h = gethostbyname(host);
        if (!h) { close(s); return 0; }
        memcpy(&a.sin_addr, h->h_addr_list[0], sizeof a.sin_addr);
    } else a.sin_addr.s_addr = ip;
    ok = connect(s, (struct sockaddr *)&a, sizeof a) == 0;
    close(s);
    return ok;
#endif
}

static long read_pidfile(const char *path, long *pid) {
    char *c = read_file(path, NULL);
    if (!c) return 0;
    *pid = strtol(c, NULL, 10);
    free(c);
    return *pid > 0;
}

static int pid_alive(long pid) {
    if (pid <= 0) return 0;
#ifdef _WIN32
    {
        HANDLE h = OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION, FALSE, (DWORD)pid);
        DWORD code = 0;
        if (!h) return 0;
        if (!GetExitCodeProcess(h, &code)) { CloseHandle(h); return 0; }
        CloseHandle(h);
        return code == STILL_ACTIVE;
    }
#else
    return kill((pid_t)pid, 0) == 0;
#endif
}

static int cmd_start(int argc, char **argv);

static int cmd_serve(int argc, char **argv) {
    char server[4096];
    char cmd[8192];
    int daemon = 0, j;
    const char *dbp, *host;
    int port;
    for (j = 0; j < argc; j++) {
        if (streq(argv[j], "--daemon") || streq(argv[j], "-d")) daemon = 1;
        else if (argv[j][0] == '-') { warnmsg("usage: superdb serve [--daemon]"); return 2; }
        else { warnmsg("usage: superdb serve [--daemon]"); return 2; }
    }
    if (daemon) return cmd_start(0, argv); /* detach into the background */
    dbp = resolve_db();
    host = resolve_host();
    port = resolve_port();
    find_helper("superdb-server", server, sizeof server);
    snprintf(cmd, sizeof cmd, "\"%s\" --db \"%s\" --host %s --port %d",
             server, dbp, host, port);
    printf("serving %s on %s:%d\n", dbp, host, port);
    fflush(stdout);
#ifdef _WIN32
    return system(cmd);
#else
    {
        /* exec so signals reach the server directly */
        char ports[32];
        snprintf(ports, sizeof ports, "%d", port);
        execl(server, server, "--db", dbp, "--host", host, "--port", ports,
              (char *)NULL);
        /* execl failed: fall back to PATH lookup via system */
        return system(cmd);
    }
#endif
}

static int cmd_start(int argc, char **argv) {
    char server[4096];
    const char *dbp, *host;
    int port, j;
    long oldpid = 0;
    for (j = 0; j < argc; j++) {
        warnmsg("usage: superdb start");
        (void)j;
        return 2;
    }
    if (read_pidfile(PID_FILE_NAME, &oldpid) && pid_alive(oldpid)) {
        printf("already running (pid %ld)\n", oldpid);
        return 0;
    }
    find_helper("superdb-server", server, sizeof server);
    dbp = resolve_db();
    host = resolve_host();
    port = resolve_port();
#ifdef _WIN32
    {
        char cmdline[8192];
        STARTUPINFOA si;
        PROCESS_INFORMATION pi;
        SECURITY_ATTRIBUTES sa;
        HANDLE logh;
        memset(&si, 0, sizeof si);
        memset(&pi, 0, sizeof pi);
        si.cb = sizeof si;
        sa.nLength = sizeof sa;
        sa.bInheritHandle = TRUE;
        sa.lpSecurityDescriptor = NULL;
        logh = CreateFileA(LOG_FILE_NAME, FILE_APPEND_DATA, FILE_SHARE_READ,
                           &sa, OPEN_ALWAYS, FILE_ATTRIBUTE_NORMAL, NULL);
        if (logh == INVALID_HANDLE_VALUE) { warnmsg("cannot open log file"); return 1; }
        snprintf(cmdline, sizeof cmdline, "\"%s\" --db \"%s\" --host %s --port %d",
                 server, dbp, host, port);
        si.dwFlags = STARTF_USESTDHANDLES;
        si.hStdOutput = logh;
        si.hStdError = logh;
        si.hStdInput = NULL;
        if (!CreateProcessA(NULL, cmdline, NULL, NULL, TRUE,
                            CREATE_NO_WINDOW | DETACHED_PROCESS,
                            NULL, NULL, &si, &pi)) {
            CloseHandle(logh);
            warnmsg("failed to start server (is superdb-server on PATH?)");
            return 1;
        }
        CloseHandle(pi.hThread);
        CloseHandle(pi.hProcess);
        CloseHandle(logh);
        {
            FILE *pf = fopen(PID_FILE_NAME, "wb");
            if (pf) { fprintf(pf, "%lu\n", (unsigned long)pi.dwProcessId); fclose(pf); }
        }
        printf("started (pid %lu), db %s, %s:%d\n",
               (unsigned long)pi.dwProcessId, dbp, host, port);
        return 0;
    }
#else
    {
        pid_t pid = fork();
        if (pid < 0) { warnmsg("fork failed"); return 1; }
        if (pid == 0) {
            /* child: detach + redirect to log */
            int logfd;
            setsid();
            logfd = open(LOG_FILE_NAME, O_WRONLY | O_CREAT | O_APPEND, 0644);
            if (logfd >= 0) {
                dup2(logfd, STDOUT_FILENO);
                dup2(logfd, STDERR_FILENO);
                if (logfd > 2) close(logfd);
            }
            {
                int devnull = open("/dev/null", O_RDONLY);
                if (devnull >= 0) { dup2(devnull, STDIN_FILENO); if (devnull > 2) close(devnull); }
            }
            {
                char ports[32];
                snprintf(ports, sizeof ports, "%d", port);
                execl(server, server, "--db", dbp, "--host", host, "--port", ports,
                      (char *)NULL);
                _exit(127);
            }
        }
        {
            FILE *pf = fopen(PID_FILE_NAME, "wb");
            if (pf) { fprintf(pf, "%ld\n", (long)pid); fclose(pf); }
        }
        printf("started (pid %ld), db %s, %s:%d\n", (long)pid, dbp, host, port);
        return 0;
    }
#endif
}

static int cmd_stop(int argc, char **argv) {
    long pid = 0;
    int j;
    for (j = 0; j < argc; j++) { warnmsg("usage: superdb stop"); return 2; }
    if (!read_pidfile(PID_FILE_NAME, &pid)) {
        warnmsg("not running (no " PID_FILE_NAME ").");
        return 1;
    }
    if (!pid_alive(pid)) {
        remove(PID_FILE_NAME);
        printf("stale pid file removed (pid %ld was not alive)\n", pid);
        return 0;
    }
#ifdef _WIN32
    {
        HANDLE h = OpenProcess(PROCESS_TERMINATE, FALSE, (DWORD)pid);
        if (!h) { warnmsg("cannot signal process"); return 1; }
        if (!TerminateProcess(h, 0)) { CloseHandle(h); warnmsg("stop failed"); return 1; }
        CloseHandle(h);
    }
#else
    if (kill((pid_t)pid, SIGTERM) != 0) { warnmsg("stop failed"); return 1; }
#endif
    remove(PID_FILE_NAME);
    printf("stopped (pid %ld)\n", pid);
    return 0;
}

static int cmd_status(int argc, char **argv) {
    long pid = 0;
    const char *host;
    int port, j;
    int alive = 0, listening = 0;
    for (j = 0; j < argc; j++) { warnmsg("usage: superdb status"); return 2; }
    host = resolve_host();
    port = resolve_port();
    if (read_pidfile(PID_FILE_NAME, &pid) && pid_alive(pid)) alive = 1;
    listening = tcp_probe(host, port);
    printf("db: %s\n", resolve_db());
    printf("endpoint: %s:%d\n", host, port);
    if (alive) printf("process: running (pid %ld)\n", pid);
    else printf("process: not running%s\n", pid ? " (stale pid file)" : "");
    printf("port: %s\n", listening ? "listening" : "closed");
    return (alive || listening) ? 0 : 1;
}

static int cmd_logs(int argc, char **argv) {
    long lines = 50, sz, i, start, nl = 0;
    char *data;
    int j;
    for (j = 0; j < argc; j++) {
        if ((streq(argv[j], "--lines") || streq(argv[j], "-n")) && j + 1 < argc) {
            lines = strtol(argv[++j], NULL, 10);
            if (lines <= 0) lines = 50;
        } else if (strncmp(argv[j], "--lines=", 8) == 0) {
            lines = strtol(argv[j] + 8, NULL, 10);
            if (lines <= 0) lines = 50;
        } else { warnmsg("usage: superdb logs [--lines N]"); return 2; }
    }
    data = read_file(LOG_FILE_NAME, NULL);
    if (!data) { printf("(no %s yet)\n", LOG_FILE_NAME); return 0; }
    sz = (long)strlen(data);
    start = sz;
    for (i = sz - 1; i >= 0 && nl < lines; i--) {
        if (data[i] == '\n') {
            nl++;
            if (nl == lines) { start = i + 1; break; }
            if (i == 0) start = 0;
        }
        if (i == 0) { start = 0; break; }
    }
    fputs(data + start, stdout);
    free(data);
    return 0;
}

/* ---------------- assoc: .sdb file association ---------------- */

#ifdef _WIN32
static int reg_add(const char *key, const char *defval, const char *valname, const char *val) {
    char cmd[8192];
    if (valname)
        snprintf(cmd, sizeof cmd, "reg add \"%s\" /v \"%s\" /d \"%s\" /f >NUL 2>&1",
                 key, valname, val);
    else if (val)
        snprintf(cmd, sizeof cmd, "reg add \"%s\" /ve /d \"%s\" /f >NUL 2>&1", key, val);
    else
        snprintf(cmd, sizeof cmd, "reg add \"%s\" /f >NUL 2>&1", key);
    (void)defval;
    return system(cmd);
}
#endif

static int cmd_assoc(int argc, char **argv) {
    int uninstall = 0, j;
    for (j = 0; j < argc; j++) {
        if (streq(argv[j], "--uninstall")) uninstall = 1;
        else { warnmsg("usage: superdb assoc [--uninstall]"); return 2; }
    }
#ifdef _WIN32
    if (uninstall) {
        system("reg delete \"HKCU\\Software\\Classes\\.sdb\" /f >NUL 2>&1");
        system("reg delete \"HKCU\\Software\\Classes\\SuperDB.Database\" /f >NUL 2>&1");
        msg("removed .sdb association (HKCU).");
        return 0;
    }
    {
        char tool[4096], open[8192];
        GetModuleFileNameA(NULL, tool, (DWORD)sizeof tool);
        snprintf(open, sizeof open, "\"%s\" inspect \"%%1\"", tool);
        reg_add("HKCU\\Software\\Classes\\.sdb", NULL, NULL, "SuperDB.Database");
        reg_add("HKCU\\Software\\Classes\\SuperDB.Database", NULL, NULL, "SuperDB Database");
        {
            char cmd[8192];
            snprintf(cmd, sizeof cmd,
                     "reg add \"HKCU\\Software\\Classes\\SuperDB.Database\\shell\\open\\command\""
                     " /ve /d \"%s\" /f >NUL 2>&1", open);
            if (system(cmd) != 0) { warnmsg("failed to write registry"); return 1; }
        }
        msg("associated .sdb with SuperDB (double-click inspects the database).");
        return 0;
    }
#elif defined(__linux__)
    {
        const char *home = getenv("HOME");
        char dir1[4096], dir2[4096], f1[4096], f2[4096];
        const char *mime =
            "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n"
            "<mime-info xmlns=\"http://www.freedesktop.org/standards/shared-mime-info\">\n"
            "  <mime-type type=\"application/x-superdb\">\n"
            "    <comment>SuperDB database</comment>\n"
            "    <glob pattern=\"*.sdb\"/>\n"
            "    <magic><match type=\"string\" value=\"SDB1\" offset=\"0\"/></magic>\n"
            "  </mime-type>\n"
            "</mime-info>\n";
        const char *desk =
            "[Desktop Entry]\n"
            "Type=Application\n"
            "Name=SuperDB Inspector\n"
            "Comment=Inspect SuperDB .sdb databases\n"
            "Exec=superdb inspect %f\n"
            "MimeType=application/x-superdb;\n"
            "Categories=Development;Database;\n"
            "Terminal=true\n";
        FILE *f;
        if (!home || !*home) { warnmsg("HOME is not set"); return 1; }
        snprintf(dir1, sizeof dir1, "%s/.local/share/mime/packages", home);
        snprintf(dir2, sizeof dir2, "%s/.local/share/applications", home);
        snprintf(f1, sizeof f1, "%s/superdb.xml", dir1);
        snprintf(f2, sizeof f2, "%s/superdb-inspector.desktop", dir2);
        if (uninstall) {
            remove(f1);
            remove(f2);
            system("update-mime-database ~/.local/share/mime >/dev/null 2>&1");
            system("update-desktop-database ~/.local/share/applications >/dev/null 2>&1");
            msg("removed .sdb association.");
            return 0;
        }
        make_dirs(dir1);
        make_dirs(dir2);
        f = fopen(f1, "wb"); if (!f) { warnmsg("cannot write mime file"); return 1; }
        fputs(mime, f); fclose(f);
        f = fopen(f2, "wb"); if (!f) { warnmsg("cannot write desktop file"); return 1; }
        fputs(desk, f); fclose(f);
        system("update-mime-database ~/.local/share/mime >/dev/null 2>&1");
        system("update-desktop-database ~/.local/share/applications >/dev/null 2>&1");
        msg("associated *.sdb with `superdb inspect` (MIME + desktop entry).");
        return 0;
    }
#elif defined(__APPLE__)
    if (uninstall) { msg("nothing to remove on macOS (association is manual)."); return 0; }
    printf("macOS: right-click a .sdb file > Get Info > Open with > SuperDB Inspector.\n");
    printf("Tip: `duti -s org.superdb.inspector .sdb all` once an app bundle exists.\n");
    return 0;
#else
    (void)uninstall;
    warnmsg("file association is not supported on this platform.");
    return 1;
#endif
}

/* ---------------- doctor ---------------- */

static int cmd_doctor(int argc, char **argv) {
    char server[4096], cli[4096];
    const char *dbp;
    long sz;
    int j, problems = 0;
    for (j = 0; j < argc; j++) { warnmsg("usage: superdb doctor"); return 2; }
    printf("superdb %s\n", SUPERDB_VERSION);
    find_helper("superdb-server", server, sizeof server);
    find_helper("superdb-cli", cli, sizeof cli);
    printf("[%s] server binary: %s\n",
           (strchr(server, PATH_SEP) ? file_exists(server) : 1) ? "ok" : "MISSING", server);
    printf("[%s] cli binary: %s\n",
           (strchr(cli, PATH_SEP) ? file_exists(cli) : 1) ? "ok" : "MISSING", cli);
    if (file_exists(CONFIG_FILE_NAME)) {
        load_config();
        printf("[ok] config: %s (db=%s host=%s port=%ld)\n", CONFIG_FILE_NAME,
               g_cfg_db[0] ? g_cfg_db : "(default)",
               g_cfg_host[0] ? g_cfg_host : "(default)",
               g_cfg_port > 0 ? g_cfg_port : 7381);
    } else {
        printf("[--] config: no %s (defaults will be used; run `superdb init`)\n", CONFIG_FILE_NAME);
    }
    dbp = resolve_db();
    sz = file_size(dbp);
    if (sz < 0) {
        printf("[--] db: '%s' does not exist yet (created on first write)\n", dbp);
    } else {
        FILE *f = fopen(dbp, "rb");
        char m[4] = {0, 0, 0, 0};
        if (f) { fread(m, 1, 4, f); fclose(f); }
        if (sz >= 4 && memcmp(m, "SDB1", 4) == 0)
            printf("[ok] db: '%s' (%ld bytes, valid magic)\n", dbp, sz);
        else {
            printf("[!!] db: '%s' (%ld bytes, BAD magic)\n", dbp, sz);
            problems++;
        }
    }
    {
        long pid = 0;
        if (read_pidfile(PID_FILE_NAME, &pid) && pid_alive(pid))
            printf("[ok] server: running (pid %ld, %s:%d)\n", pid, resolve_host(), resolve_port());
        else if (tcp_probe(resolve_host(), resolve_port()))
            printf("[ok] server: port %d is listening (not started via `superdb start`)\n", resolve_port());
        else
            printf("[--] server: not running\n");
    }
#ifdef _WIN32
    printf("[..] assoc: run `superdb assoc` to register .sdb (HKCU\\Software\\Classes)\n");
#elif defined(__linux__)
    {
        const char *h = getenv("HOME");
        char f1[4096];
        if (h) snprintf(f1, sizeof f1, "%s/.local/share/mime/packages/superdb.xml", h);
        printf("[%s] assoc: %s\n", (h && file_exists(f1)) ? "ok" : "..",
               (h && file_exists(f1)) ? "registered" : "run `superdb assoc`");
    }
#else
    printf("[..] assoc: manual on this platform (see `superdb assoc`)\n");
#endif
    return problems ? 1 : 0;
}

/* ---------------- shell (REPL via superdb-cli) ---------------- */

static int cmd_shell(int argc, char **argv) {
    char cli[4096];
    char cmd[8192];
    int j;
    for (j = 0; j < argc; j++) { warnmsg("usage: superdb shell"); return 2; }
    find_helper("superdb-cli", cli, sizeof cli);
    snprintf(cmd, sizeof cmd, "\"%s\" -h %s -p %d -i", cli, resolve_host(), resolve_port());
    return system(cmd);
}

/* ---------------- help / version ---------------- */

static void usage(void) {
    printf(
        "superdb %s — project manager + offline toolkit for SuperDB\n"
        "\n"
        "Usage: superdb [--db FILE] [--host ADDR] [--port N] <command> [args]\n"
        "\n"
        "Project setup:\n"
        "  init [DIR] [--db PATH] [--host A] [--port N] [--force] [--no-vscode]\n"
        "      scaffold superdb.json, .vscode/, .gitignore, example\n"
        "  install [DIR] [--from SRC] [--force]\n"
        "      vendor the embeddable C sources into DIR/third_party/superdb\n"
        "\n"
        "Offline data (no server needed, works on FILE directly):\n"
        "  get KEY | set KEY VALUE [EX sec] | del KEY... | exists KEY...\n"
        "  keys [PAT] | incr KEY [BY] | expire KEY SEC | ttl KEY | pttl KEY\n"
        "  dbsize | info | compact | flushall --yes\n"
        "  dump [--out FILE.json] | restore --in FILE.json [--flush]\n"
        "  inspect [FILE.sdb]\n"
        "\n"
        "Server lifecycle:\n"
        "  serve [--daemon]     run superdb-server in the foreground\n"
        "  start | stop | status | logs [--lines N] | shell\n"
        "\n"
        "Environment:\n"
        "  doctor               checklist: binaries, config, db, server, assoc\n"
        "  assoc [--uninstall]  register the .sdb file association\n"
        "  version | help\n"
        "\n"
        "Config: leading --db/--host/--port, else $SUPERDB_DB/...,\n"
        "        else ./superdb.json, else ./superdb.sdb + 127.0.0.1:7381\n"
        "Docs: docs/CLI.md\n",
        SUPERDB_VERSION);
}

static int dispatch(int argc, char **argv) {
    const char *cmd;
    if (argc == 0) { usage(); return 2; }
    cmd = argv[0];
    argc--; argv++;
    if (streq(cmd, "get")) return cmd_get(argc, argv);
    if (streq(cmd, "set")) return cmd_set(argc, argv);
    if (streq(cmd, "del") || streq(cmd, "delete")) return cmd_del(argc, argv);
    if (streq(cmd, "exists")) return cmd_exists(argc, argv);
    if (streq(cmd, "keys")) return cmd_keys(argc, argv);
    if (streq(cmd, "incr") || streq(cmd, "incrby")) return cmd_incr(argc, argv);
    if (streq(cmd, "expire")) return cmd_expire(argc, argv);
    if (streq(cmd, "ttl")) return cmd_ttl(argc, argv, 0);
    if (streq(cmd, "pttl")) return cmd_ttl(argc, argv, 1);
    if (streq(cmd, "dbsize")) return cmd_dbsize(argc, argv);
    if (streq(cmd, "info")) return cmd_info(argc, argv);
    if (streq(cmd, "compact") || streq(cmd, "save")) return cmd_compact(argc, argv);
    if (streq(cmd, "flushall")) return cmd_flushall(argc, argv);
    if (streq(cmd, "dump")) return cmd_dump(argc, argv);
    if (streq(cmd, "restore")) return cmd_restore(argc, argv);
    if (streq(cmd, "inspect")) return cmd_inspect(argc, argv);
    if (streq(cmd, "init")) return cmd_init(argc, argv);
    if (streq(cmd, "install")) return cmd_install(argc, argv);
    if (streq(cmd, "serve")) return cmd_serve(argc, argv);
    if (streq(cmd, "start")) return cmd_start(argc, argv);
    if (streq(cmd, "stop")) return cmd_stop(argc, argv);
    if (streq(cmd, "status")) return cmd_status(argc, argv);
    if (streq(cmd, "logs")) return cmd_logs(argc, argv);
    if (streq(cmd, "shell")) return cmd_shell(argc, argv);
    if (streq(cmd, "assoc")) return cmd_assoc(argc, argv);
    if (streq(cmd, "doctor")) return cmd_doctor(argc, argv);
    if (streq(cmd, "version") || streq(cmd, "--version") || streq(cmd, "-V")) {
        printf("superdb %s\n", SUPERDB_VERSION);
        return 0;
    }
    if (streq(cmd, "help") || streq(cmd, "--help") || streq(cmd, "-h")) { usage(); return 0; }
    fprintf(stderr, "superdb: unknown command '%s' (try: superdb help)\n", cmd);
    return 2;
}

int main(int argc, char **argv) {
    int cmd_at = 1;
    g_argv0 = argv[0];
    /* Leading globals only: `superdb --db X get k` works, but
     * `superdb init --db Y` leaves --db for the init command. */
    while (cmd_at < argc) {
        char *a = argv[cmd_at];
        if (streq(a, "--db") || streq(a, "--host") || streq(a, "--port")) {
            if (cmd_at + 1 >= argc) {
                fprintf(stderr, "superdb: %s needs a value\n", a);
                return 2;
            }
            if (streq(a, "--db")) g_db = argv[cmd_at + 1];
            else if (streq(a, "--host")) g_host = argv[cmd_at + 1];
            else {
                g_port = atoi(argv[cmd_at + 1]);
                if (g_port <= 0 || g_port > 65535) {
                    warnmsg("--port must be 1..65535");
                    return 2;
                }
            }
            cmd_at += 2;
        } else if (strncmp(a, "--db=", 5) == 0) { g_db = a + 5; cmd_at++; }
        else if (strncmp(a, "--host=", 7) == 0) { g_host = a + 7; cmd_at++; }
        else if (strncmp(a, "--port=", 7) == 0) {
            g_port = atoi(a + 7);
            if (g_port <= 0 || g_port > 65535) { warnmsg("--port must be 1..65535"); return 2; }
            cmd_at++;
        } else break;
    }
    return dispatch(argc - cmd_at, argv + cmd_at);
}
