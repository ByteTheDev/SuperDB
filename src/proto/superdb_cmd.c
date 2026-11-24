/* superdb_cmd - implementation. See superdb_cmd.h.
 * Commands are table-driven: name, handler, arity, and flags (write / noauth
 * / transaction control). Dispatch handles auth, read-only replicas, MULTI
 * queueing, per-command stats and the slowlog. */
#include "superdb_cmd.h"
#include "superdb_thread.h"
#include "superdb_util.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <stdarg.h>
#include <errno.h>
#include <inttypes.h>
#include <ctype.h>

#define SLOWLOG_MAX       128
#define SLOWLOG_ARGS      8
#define SLOWLOG_ARG_BYTES 128
#define SYNCLOG_DEFAULT   (1u << 20)
#define SYNCLOG_MAX       (8u << 20)

typedef struct {
    uint64_t id;
    int64_t  ts_sec;
    int64_t  usec;
    int      argc;
    char     args[SLOWLOG_ARGS][SLOWLOG_ARG_BYTES];
    size_t   alen[SLOWLOG_ARGS];
} slow_ent_t;

struct sdb_ctx {
    superdb_t *db;
    char *password;             /* NULL = no auth required */
    int readonly;               /* read-only replica: reject writes */
    char primary[300];          /* "host:port" for INFO, "" if none */
    long long slowlog_us;
    int shutdown;
    sdb_mutex_t mu;             /* guards the fields below */
    uint64_t conns_total, conns_open, cmds_total;
    int64_t start_ms;
    uint64_t *calls;            /* per command */
    int64_t  *usec;             /* per command */
    slow_ent_t slow[SLOWLOG_MAX];
    size_t slow_head, slow_count;
    uint64_t slow_next_id;
};

typedef struct call call_t;
typedef void (*cmd_fn)(call_t *c);

typedef struct {
    const char *name;
    cmd_fn fn;
    int min, max;               /* argc bounds, max < 0 = unlimited */
    int write;                  /* rejected on read-only replicas */
    int noauth;                 /* allowed before AUTH */
    int ctl;                    /* MULTI/EXEC/DISCARD: never queued */
} cmd_def_t;

struct call {
    sdb_ctx_t *ctx;
    sdb_session_t *s;
    int argc;
    char **argv;
    size_t *al;
    sbuf_t *out;
    int *quit;
    const cmd_def_t *def;
};

static const cmd_def_t *cmd_table(size_t *n);
static const cmd_def_t *cmd_lookup(const char *name);
static void run_cmd(sdb_ctx_t *ctx, sdb_session_t *s, const cmd_def_t *def,
                    char **argv, size_t *arglen, int argc, sbuf_t *out, int *quit);

static const char *WRONGTYPE_TXT = "WRONGTYPE Operation against a key holding the wrong kind of value";

/* ---------------- argument helpers ---------------- */

/* Key argument as a C string, or NULL if it contains NUL bytes. */
static const char *karg(call_t *c, int i) {
    if (strlen(c->argv[i]) != c->al[i]) return NULL;
    return c->argv[i];
}

static int parse_i64(call_t *c, int i, int64_t *out) {
    char *end = NULL;
    long long v;
    if (c->al[i] == 0 || c->al[i] > 31) return 0;
    errno = 0;
    v = strtoll(c->argv[i], &end, 10);
    if (errno != 0 || end != c->argv[i] + c->al[i]) return 0;
    *out = (int64_t)v;
    return 1;
}

static int parse_u64(call_t *c, int i, uint64_t *out) {
    char *end = NULL;
    unsigned long long v;
    if (c->al[i] == 0 || c->al[i] > 31 || c->argv[i][0] < '0' || c->argv[i][0] > '9') return 0;
    errno = 0;
    v = strtoull(c->argv[i], &end, 10);
    if (errno != 0 || end != c->argv[i] + c->al[i]) return 0;
    *out = (uint64_t)v;
    return 1;
}

static void key_err(call_t *c) {
    sb_err(c->out, "keys may not contain NUL bytes");
}

static void syntax_err(call_t *c) {
    sb_err(c->out, "syntax error");
}

static void err_rc(call_t *c, int rc) {
    if (rc == SUPERDB_WRONGTYPE) sb_raw_err(c->out, WRONGTYPE_TXT);
    else sb_err(c->out, superdb_err(c->ctx->db));
}

/* Constant-time comparison so password checks do not leak a prefix length. */
static int secure_eq(const char *a, size_t an, const char *b, size_t bn) {
    unsigned char d = (unsigned char)(an != bn);
    size_t i, n = an < bn ? an : bn;
    for (i = 0; i < n; i++) d |= (unsigned char)(a[i] ^ b[i]);
    return d == 0;
}

/* ---------------- connection & auth ---------------- */

static void h_ping(call_t *c) {
    if (c->argc == 1) sb_str(c->out, "+PONG\r\n");
    else sb_bulk(c->out, c->argv[1], c->al[1]);
}

static void h_echo(call_t *c) {
    sb_bulk(c->out, c->argv[1], c->al[1]);
}

static void h_auth(call_t *c) {
    sdb_ctx_t *ctx = c->ctx;
    if (!ctx->password) {
        sb_err(c->out, "AUTH <password> called without any password configured for the server");
        return;
    }
    if (secure_eq(c->argv[1], c->al[1], ctx->password, strlen(ctx->password))) {
        c->s->authed = 1;
        sb_ok(c->out);
    } else {
        sb_raw_err(c->out, "WRONGPASS invalid password");
    }
}

static void h_quit(call_t *c) {
    sb_ok(c->out);
    *c->quit = 1;
}

static void h_shutdown(call_t *c) {
    sb_ok(c->out);
    *c->quit = 2;
}

/* ---------------- strings ---------------- */

static void h_set(call_t *c) {
    const char *k = karg(c, 1);
    int64_t ttl_ms = 0;
    int rc;
    if (!k) { key_err(c); return; }
    if (c->argc == 5) {
        int64_t v = 0;
        if (sdb_cmd_eq(c->argv[3], "EX")) {
            if (!parse_i64(c, 4, &v) || v <= 0) { sb_err(c->out, "bad EX seconds"); return; }
            ttl_ms = v * 1000;
        } else if (sdb_cmd_eq(c->argv[3], "PX")) {
            if (!parse_i64(c, 4, &v) || v <= 0) { sb_err(c->out, "bad PX milliseconds"); return; }
            ttl_ms = v;
        } else {
            syntax_err(c); return;
        }
    } else if (c->argc != 3) {
        syntax_err(c); return;
    }
    rc = ttl_ms > 0 ? superdb_set_ex(c->ctx->db, k, c->argv[2], c->al[2], ttl_ms)
                    : superdb_set(c->ctx->db, k, c->argv[2], c->al[2]);
    if (rc != 0) sb_err(c->out, superdb_err(c->ctx->db));
    else sb_ok(c->out);
}

static void h_setnx(call_t *c) {
    const char *k = karg(c, 1);
    int rc;
    if (!k) { key_err(c); return; }
    rc = superdb_setnx(c->ctx->db, k, c->argv[2], c->al[2]);
    if (rc < 0) err_rc(c, rc);
    else sb_int(c->out, rc);
}

static void h_get(call_t *c) {
    const char *k = karg(c, 1);
    void *v = NULL;
    size_t n = 0;
    int rc;
    if (!k) { key_err(c); return; }
    rc = superdb_get_ex(c->ctx->db, k, &v, &n);
    if (rc == 1) { sb_bulk(c->out, v, n); superdb_free(v); }
    else if (rc == 0) sb_nil(c->out);
    else err_rc(c, rc);
}

static void h_getset(call_t *c) {
    const char *k = karg(c, 1);
    void *old = NULL;
    size_t n = 0;
    int rc;
    if (!k) { key_err(c); return; }
    rc = superdb_getset(c->ctx->db, k, c->argv[2], c->al[2], &old, &n);
    if (rc < 0) { err_rc(c, rc); return; }
    if (old) { sb_bulk(c->out, old, n); superdb_free(old); }
    else sb_nil(c->out);
}

static void h_msetnx(call_t *c) {
    size_t n, i;
    const char **keys;
    const void **vals;
    size_t *vlens;
    int rc;
    if ((c->argc - 1) % 2) {
        sb_err(c->out, "wrong number of arguments for 'msetnx' command");
        return;
    }
    n = (size_t)(c->argc - 1) / 2;
    keys = (const char**)malloc(n * sizeof *keys);
    vals = (const void**)malloc(n * sizeof *vals);
    vlens = (size_t*)malloc(n * sizeof *vlens);
    if (!keys || !vals || !vlens) {
        free(keys); free(vals); free(vlens);
        sb_err(c->out, "out of memory");
        return;
    }
    for (i = 0; i < n; i++) {
        keys[i] = karg(c, (int)(1 + 2 * i));
        vals[i] = c->argv[2 + 2 * i];
        vlens[i] = c->al[2 + 2 * i];
        if (!keys[i]) { key_err(c); free(keys); free(vals); free(vlens); return; }
    }
    rc = superdb_msetnx(c->ctx->db, n, keys, vals, vlens);
    free(keys); free(vals); free(vlens);
    if (rc < 0) err_rc(c, rc);
    else sb_int(c->out, rc);
}

static void h_incr(call_t *c) {
    const char *k = karg(c, 1);
    int64_t by = 1;
    int ok = 0;
    int64_t v;
    if (!k) { key_err(c); return; }
    if (c->argc == 3 && !parse_i64(c, 2, &by)) {
        sb_err(c->out, "value is not an integer or out of range");
        return;
    }
    v = superdb_incr(c->ctx->db, k, by, &ok);
    if (!ok) {
        const char *msg = superdb_err(c->ctx->db);
        if (strncmp(msg, "WRONGTYPE", 9) == 0) sb_raw_err(c->out, msg);
        else sb_err(c->out, msg);
    } else {
        sb_int(c->out, v);
    }
}

/* ---------------- keyspace ---------------- */

static void h_del(call_t *c) {
    int64_t n = 0;
    int i;
    for (i = 1; i < c->argc; i++) {
        const char *k = karg(c, i);
        if (!k) { key_err(c); return; }
        n += superdb_del(c->ctx->db, k);
    }
    sb_int(c->out, n);
}

static void h_exists(call_t *c) {
    int64_t n = 0;
    int i;
    for (i = 1; i < c->argc; i++) {
        const char *k = karg(c, i);
        if (!k) { key_err(c); return; }
        n += superdb_exists(c->ctx->db, k);
    }
    sb_int(c->out, n);
}

static void reply_keys(call_t *c, superdb_keys_t *ks) {
    size_t i;
    sb_array(c->out, ks->count);
    for (i = 0; i < ks->count; i++) sb_bulk_str(c->out, ks->keys[i]);
}

static void h_keys(call_t *c) {
    superdb_keys_t ks = {NULL, 0};
    char *pfx;
    size_t pn = c->argc == 2 ? c->al[1] : 0;
    if (pn > 0 && c->argv[1][pn - 1] == '*') pn--;     /* "prefix*" form */
    pfx = (char*)malloc(pn + 1);
    if (!pfx) { sb_err(c->out, "out of memory"); return; }
    if (pn) memcpy(pfx, c->argv[1], pn);
    pfx[pn] = '\0';
    if (strlen(pfx) != pn) { free(pfx); key_err(c); return; }
    ks = superdb_keys(c->ctx->db, pfx);
    free(pfx);
    reply_keys(c, &ks);
    superdb_free_keys(&ks);
}

static void h_scan(call_t *c) {
    uint64_t cur, next;
    size_t count = 10;
    const char *pat = NULL;
    superdb_keys_t ks = {NULL, 0};
    int i;
    char nb[32];
    int nl;
    if (!parse_u64(c, 1, &cur)) { sb_err(c->out, "invalid cursor"); return; }
    for (i = 2; i < c->argc; i += 2) {
        if (i + 1 >= c->argc) { syntax_err(c); return; }
        if (sdb_cmd_eq(c->argv[i], "MATCH")) {
            pat = karg(c, i + 1);
            if (!pat) { key_err(c); return; }
        } else if (sdb_cmd_eq(c->argv[i], "COUNT")) {
            int64_t v = 0;
            if (!parse_i64(c, i + 1, &v) || v < 1) { sb_err(c->out, "COUNT must be >= 1"); return; }
            count = (size_t)v;
        } else {
            syntax_err(c); return;
        }
    }
    next = superdb_scan(c->ctx->db, cur, pat, count, &ks);
    if (next == UINT64_MAX) {
        superdb_free_keys(&ks);
        sb_err(c->out, "out of memory");
        return;
    }
    nl = snprintf(nb, sizeof nb, "%" PRIu64, next);
    sb_array(c->out, 2);
    sb_bulk(c->out, nb, (size_t)nl);
    reply_keys(c, &ks);
    superdb_free_keys(&ks);
}

static void h_type(call_t *c) {
    const char *k = karg(c, 1);
    int t;
    if (!k) { key_err(c); return; }
    t = superdb_type(c->ctx->db, k);
    switch (t) {
    case SUPERDB_T_STRING: sb_str(c->out, "+string\r\n"); break;
    case SUPERDB_T_LIST:   sb_str(c->out, "+list\r\n"); break;
    case SUPERDB_T_HASH:   sb_str(c->out, "+hash\r\n"); break;
    case SUPERDB_T_SET:    sb_str(c->out, "+set\r\n"); break;
    default:               sb_str(c->out, "+none\r\n"); break;
    }
}

static void h_rename(call_t *c) {
    const char *a = karg(c, 1), *b = karg(c, 2);
    int rc;
    if (!a || !b) { key_err(c); return; }
    rc = superdb_rename(c->ctx->db, a, b);
    if (rc == 1) sb_ok(c->out);
    else if (rc == 0) sb_err(c->out, "no such key");
    else sb_err(c->out, superdb_err(c->ctx->db));
}

/* ---------------- expiry ---------------- */

static void h_expire(call_t *c) {
    const char *k = karg(c, 1);
    int64_t secs = 0;
    if (!k) { key_err(c); return; }
    if (!parse_i64(c, 2, &secs) || secs <= 0) { sb_err(c->out, "bad seconds"); return; }
    sb_int(c->out, superdb_expire(c->ctx->db, k, secs * 1000));
}

static void h_expireat(call_t *c) {
    const char *k = karg(c, 1);
    int64_t secs = 0;
    if (!k) { key_err(c); return; }
    if (!parse_i64(c, 2, &secs) || secs < 0) { sb_err(c->out, "bad timestamp"); return; }
    sb_int(c->out, superdb_expireat(c->ctx->db, k, secs * 1000));
}

static void h_persist(call_t *c) {
    const char *k = karg(c, 1);
    if (!k) { key_err(c); return; }
    sb_int(c->out, superdb_persist(c->ctx->db, k));
}

static void h_ttl(call_t *c) {
    const char *k = karg(c, 1);
    int64_t t;
    if (!k) { key_err(c); return; }
    t = superdb_ttl(c->ctx->db, k);
    /* TTL reports whole seconds (rounded up); PTTL reports milliseconds. */
    if (t >= 0 && sdb_cmd_eq(c->def->name, "TTL")) t = (t + 999) / 1000;
    sb_int(c->out, t);
}

/* ---------------- lists ---------------- */

static void h_push(call_t *c, int front) {
    const char *k = karg(c, 1);
    int64_t n = 0;
    int i;
    if (!k) { key_err(c); return; }
    for (i = 2; i < c->argc; i++) {
        n = front ? superdb_lpush(c->ctx->db, k, c->argv[i], c->al[i])
                  : superdb_rpush(c->ctx->db, k, c->argv[i], c->al[i]);
        if (n < 0) { err_rc(c, (int)n); return; }
    }
    sb_int(c->out, n);
}
static void h_lpush(call_t *c) { h_push(c, 1); }
static void h_rpush(call_t *c) { h_push(c, 0); }

static void h_pop(call_t *c, int front) {
    const char *k = karg(c, 1);
    void *v = NULL;
    size_t n = 0;
    int rc;
    if (!k) { key_err(c); return; }
    rc = front ? superdb_lpop(c->ctx->db, k, &v, &n) : superdb_rpop(c->ctx->db, k, &v, &n);
    if (rc == 1) { sb_bulk(c->out, v, n); superdb_free(v); }
    else if (rc == 0) sb_nil(c->out);
    else err_rc(c, rc);
}
static void h_lpop(call_t *c) { h_pop(c, 1); }
static void h_rpop(call_t *c) { h_pop(c, 0); }

static void h_llen(call_t *c) {
    const char *k = karg(c, 1);
    int64_t n;
    if (!k) { key_err(c); return; }
    n = superdb_llen(c->ctx->db, k);
    if (n < 0) err_rc(c, (int)n);
    else sb_int(c->out, n);
}

static void h_lrange(call_t *c) {
    const char *k = karg(c, 1);
    int64_t a, b;
    superdb_items_t it;
    size_t i;
    int rc;
    if (!k) { key_err(c); return; }
    if (!parse_i64(c, 2, &a) || !parse_i64(c, 3, &b)) {
        sb_err(c->out, "value is not an integer or out of range");
        return;
    }
    rc = superdb_lrange(c->ctx->db, k, a, b, &it);
    if (rc < 0) { err_rc(c, rc); return; }
    sb_array(c->out, it.count);
    for (i = 0; i < it.count; i++) sb_bulk(c->out, it.items[i], it.lens[i]);
    superdb_free_items(&it);
}

/* ---------------- hashes ---------------- */

static void h_hset(call_t *c) {
    const char *k = karg(c, 1);
    int64_t added = 0;
    int i;
    if (!k) { key_err(c); return; }
    if ((c->argc - 2) % 2) { sb_err(c->out, "wrong number of arguments for 'hset' command"); return; }
    for (i = 2; i < c->argc; i += 2) {
        int r = superdb_hset(c->ctx->db, k, c->argv[i], c->al[i], c->argv[i + 1], c->al[i + 1]);
        if (r < 0) { err_rc(c, r); return; }
        added += r;
    }
    sb_int(c->out, added);
}

static void h_hget(call_t *c) {
    const char *k = karg(c, 1);
    void *v = NULL;
    size_t n = 0;
    int rc;
    if (!k) { key_err(c); return; }
    rc = superdb_hget(c->ctx->db, k, c->argv[2], c->al[2], &v, &n);
    if (rc == 1) { sb_bulk(c->out, v, n); superdb_free(v); }
    else if (rc == 0) sb_nil(c->out);
    else err_rc(c, rc);
}

static void h_hdel(call_t *c) {
    const char *k = karg(c, 1);
    int64_t n = 0;
    int i;
    if (!k) { key_err(c); return; }
    for (i = 2; i < c->argc; i++) {
        int r = superdb_hdel(c->ctx->db, k, c->argv[i], c->al[i]);
        if (r < 0) { err_rc(c, r); return; }
        n += r;
    }
    sb_int(c->out, n);
}

static void h_hlen(call_t *c) {
    const char *k = karg(c, 1);
    int64_t n;
    if (!k) { key_err(c); return; }
    n = superdb_hlen(c->ctx->db, k);
    if (n < 0) err_rc(c, (int)n);
    else sb_int(c->out, n);
}

static void h_hgetall(call_t *c) {
    const char *k = karg(c, 1);
    superdb_items_t it;
    size_t i;
    int rc;
    if (!k) { key_err(c); return; }
    rc = superdb_hgetall(c->ctx->db, k, &it);
    if (rc < 0) { err_rc(c, rc); return; }
    sb_array(c->out, it.count);
    for (i = 0; i < it.count; i++) sb_bulk(c->out, it.items[i], it.lens[i]);
    superdb_free_items(&it);
}

/* ---------------- sets ---------------- */

static void h_sadd_srem(call_t *c, int add) {
    const char *k = karg(c, 1);
    int64_t n = 0;
    int i;
    if (!k) { key_err(c); return; }
    for (i = 2; i < c->argc; i++) {
        int r = add ? superdb_sadd(c->ctx->db, k, c->argv[i], c->al[i])
                    : superdb_srem(c->ctx->db, k, c->argv[i], c->al[i]);
        if (r < 0) { err_rc(c, r); return; }
        n += r;
    }
    sb_int(c->out, n);
}
static void h_sadd(call_t *c) { h_sadd_srem(c, 1); }
static void h_srem(call_t *c) { h_sadd_srem(c, 0); }

static void h_sismember(call_t *c) {
    const char *k = karg(c, 1);
    int r;
    if (!k) { key_err(c); return; }
    r = superdb_sismember(c->ctx->db, k, c->argv[2], c->al[2]);
    if (r < 0) err_rc(c, r);
    else sb_int(c->out, r);
}

static void h_scard(call_t *c) {
    const char *k = karg(c, 1);
    int64_t n;
    if (!k) { key_err(c); return; }
    n = superdb_scard(c->ctx->db, k);
    if (n < 0) err_rc(c, (int)n);
    else sb_int(c->out, n);
}

static void h_smembers(call_t *c) {
    const char *k = karg(c, 1);
    superdb_items_t it;
    size_t i;
    int rc;
    if (!k) { key_err(c); return; }
    rc = superdb_smembers(c->ctx->db, k, &it);
    if (rc < 0) { err_rc(c, rc); return; }
    sb_array(c->out, it.count);
    for (i = 0; i < it.count; i++) sb_bulk(c->out, it.items[i], it.lens[i]);
    superdb_free_items(&it);
}

/* ---------------- maintenance ---------------- */

static void h_dbsize(call_t *c) {
    sb_int(c->out, (int64_t)superdb_count(c->ctx->db));
}

static void h_flushall(call_t *c) {
    if (superdb_flushall(c->ctx->db) != 0) sb_err(c->out, superdb_err(c->ctx->db));
    else sb_ok(c->out);
}

static void h_compact(call_t *c) {
    if (superdb_compact(c->ctx->db) != 0) sb_err(c->out, superdb_err(c->ctx->db));
    else sb_ok(c->out);
}

static void h_bgrewriteaof(call_t *c) {
    int r = superdb_compact_async(c->ctx->db);
    if (r == 0) sb_str(c->out, "+Background append only file rewriting started\r\n");
    else if (r == 1) sb_str(c->out, "+Background append only file rewriting already in progress\r\n");
    else sb_err(c->out, superdb_err(c->ctx->db));
}

static void h_backup(call_t *c) {
    char *path = (char*)malloc(c->al[1] + 1);
    int rc;
    if (!path) { sb_err(c->out, "out of memory"); return; }
    memcpy(path, c->argv[1], c->al[1]);
    path[c->al[1]] = '\0';
    rc = superdb_backup(c->ctx->db, path);
    free(path);
    if (rc != 0) sb_err(c->out, "backup failed");
    else sb_ok(c->out);
}

static void sb_line(sbuf_t *s, const char *fmt, ...) {
    char b[512];
    int n;
    va_list ap;
    va_start(ap, fmt);
    n = vsnprintf(b, sizeof b, fmt, ap);
    va_end(ap);
    if (n > 0) sb_append(s, b, (size_t)(n < (int)sizeof b ? n : (int)sizeof b - 1));
}

static void h_info(call_t *c) {
    sdb_ctx_t *ctx = c->ctx;
    superdb_stats_t st;
    sbuf_t b;
    size_t i;
    int64_t now = sdb_now_ms();
    uint64_t conns_total, conns_open, cmds_total;
    superdb_get_stats(ctx->db, &st);
    sdb_mutex_lock(&ctx->mu);
    conns_total = ctx->conns_total;
    conns_open = ctx->conns_open;
    cmds_total = ctx->cmds_total;
    sdb_mutex_unlock(&ctx->mu);

    sb_init(&b);
    sb_line(&b, "# Server\r\n");
    sb_line(&b, "superdb_version:%s\r\n", SUPERDB_VERSION);
    sb_line(&b, "uptime_in_seconds:%lld\r\n", (long long)((now - ctx->start_ms) / 1000));
    sb_line(&b, "role:%s\r\n", ctx->readonly ? "replica" : "primary");
    sb_line(&b, "# Clients\r\n");
    sb_line(&b, "connected_clients:%llu\r\n", (unsigned long long)conns_open);
    sb_line(&b, "total_connections_received:%llu\r\n", (unsigned long long)conns_total);
    sb_line(&b, "# Stats\r\n");
    sb_line(&b, "total_commands_processed:%llu\r\n", (unsigned long long)cmds_total);
    sb_line(&b, "keyspace_hits:%llu\r\n", (unsigned long long)st.hits);
    sb_line(&b, "keyspace_misses:%llu\r\n", (unsigned long long)st.misses);
    sb_line(&b, "expired_keys:%llu\r\n", (unsigned long long)st.expired_keys);
    sb_line(&b, "# Keyspace\r\n");
    sb_line(&b, "keys:%lu\r\n", (unsigned long)st.keys);
    sb_line(&b, "# Persistence\r\n");
    sb_line(&b, "log_bytes:%lu\r\n", (unsigned long)st.log_bytes);
    sb_line(&b, "dead_bytes:%lu\r\n", (unsigned long)st.dead_bytes);
    sb_line(&b, "compactions:%llu\r\n", (unsigned long long)st.compactions);
    sb_line(&b, "compacting:%d\r\n", st.compacting);
    sb_line(&b, "# Replication\r\n");
    sb_line(&b, "role:%s\r\n", ctx->readonly ? "replica" : "primary");
    if (ctx->primary[0]) sb_line(&b, "primary:%s\r\n", ctx->primary);
    sb_line(&b, "# Commandstats\r\n");
    {
        size_t ncmd = 0;
        const cmd_def_t *tab = cmd_table(&ncmd);
        sdb_mutex_lock(&ctx->mu);
        for (i = 0; i < ncmd; i++) {
            if (!ctx->calls[i]) continue;   /* only commands that ran */
            {
                char name[64];
                size_t j;
                for (j = 0; j < sizeof name - 1 && tab[i].name[j]; j++)
                    name[j] = (char)tolower((unsigned char)tab[i].name[j]);
                name[j] = '\0';
                sb_line(&b, "cmdstat_%s:calls=%llu,usec=%lld\r\n",
                        name, (unsigned long long)ctx->calls[i], (long long)ctx->usec[i]);
            }
        }
        sdb_mutex_unlock(&ctx->mu);
    }
    sb_bulk(c->out, b.data, b.len);
    sb_free(&b);
}

/* ---------------- transactions ---------------- */

static void h_multi(call_t *c) {
    if (c->s->in_multi) { sb_err(c->out, "MULTI calls can not be nested"); return; }
    c->s->in_multi = 1;
    sb_ok(c->out);
}

static void session_clear_queue(sdb_session_t *s) {
    size_t i;
    for (i = 0; i < s->nq; i++) sdb_free_argv(s->q[i].argv, s->q[i].arglen, s->q[i].argc);
    s->nq = 0;
}

static void h_discard(call_t *c) {
    if (!c->s->in_multi) { sb_err(c->out, "DISCARD without MULTI"); return; }
    session_clear_queue(c->s);
    c->s->in_multi = 0;
    sb_ok(c->out);
}

static void h_exec(call_t *c) {
    sdb_session_t *s = c->s;
    size_t i, n;
    if (!s->in_multi) { sb_err(c->out, "EXEC without MULTI"); return; }
    s->in_multi = 0;
    n = s->nq;
    sb_array(c->out, n);
    superdb_txn_begin(c->ctx->db);   /* one TXN group: atomic on disk */
    for (i = 0; i < n; i++) {
        sdb_queued_t *q = &s->q[i];
        int dquit = 0;
        const cmd_def_t *def = cmd_lookup(q->argv[0]);
        if (!def) sb_err(c->out, "unknown command");
        else run_cmd(c->ctx, s, def, q->argv, q->arglen, q->argc, c->out, &dquit);
        sdb_free_argv(q->argv, q->arglen, q->argc);
    }
    superdb_txn_commit(c->ctx->db);
    s->nq = 0;
}

/* ---------------- observability ---------------- */

static void slowlog_add(sdb_ctx_t *ctx, call_t *c, int64_t usec) {
    slow_ent_t *e = &ctx->slow[ctx->slow_head];
    int i, n = c->argc < SLOWLOG_ARGS ? c->argc : SLOWLOG_ARGS;
    e->id = ctx->slow_next_id++;
    e->ts_sec = sdb_now_ms() / 1000;
    e->usec = usec;
    e->argc = c->argc;
    for (i = 0; i < n; i++) {
        size_t l = c->al[i] < SLOWLOG_ARG_BYTES ? c->al[i] : SLOWLOG_ARG_BYTES - 1;
        memcpy(e->args[i], c->argv[i], l);
        e->args[i][l] = '\0';
        e->alen[i] = l;
    }
    ctx->slow_head = (ctx->slow_head + 1) % SLOWLOG_MAX;
    if (ctx->slow_count < SLOWLOG_MAX) ctx->slow_count++;
}

static void h_slowlog(call_t *c) {
    sdb_ctx_t *ctx = c->ctx;
    if (sdb_cmd_eq(c->argv[1], "RESET") && c->argc == 2) {
        sdb_mutex_lock(&ctx->mu);
        ctx->slow_count = 0;
        sdb_mutex_unlock(&ctx->mu);
        sb_ok(c->out);
    } else if (sdb_cmd_eq(c->argv[1], "LEN") && c->argc == 2) {
        sdb_mutex_lock(&ctx->mu);
        sb_int(c->out, (int64_t)ctx->slow_count);
        sdb_mutex_unlock(&ctx->mu);
    } else if (sdb_cmd_eq(c->argv[1], "GET") && (c->argc == 2 || c->argc == 3)) {
        int64_t want = 10, i;
        int64_t take;
        if (c->argc == 3 && (!parse_i64(c, 2, &want) || want < 0)) {
            sb_err(c->out, "count must be >= 0"); return;
        }
        sdb_mutex_lock(&ctx->mu);
        take = (int64_t)ctx->slow_count < want ? (int64_t)ctx->slow_count : want;
        sb_array(c->out, (size_t)take);
        for (i = 0; i < take; i++) {   /* newest first */
            size_t idx = (ctx->slow_head + SLOWLOG_MAX - 1 - (size_t)i) % SLOWLOG_MAX;
            slow_ent_t *e = &ctx->slow[idx];
            int a, na = e->argc < SLOWLOG_ARGS ? e->argc : SLOWLOG_ARGS;
            sb_array(c->out, 4);
            sb_int(c->out, (int64_t)e->id);
            sb_int(c->out, e->ts_sec);
            sb_int(c->out, e->usec);
            sb_array(c->out, (size_t)na);
            for (a = 0; a < na; a++) sb_bulk(c->out, e->args[a], e->alen[a]);
        }
        sdb_mutex_unlock(&ctx->mu);
    } else {
        sb_err(c->out, "SLOWLOG GET [count] | LEN | RESET");
    }
}

/* SYNCLOG <gen> <offset> [max]: raw log bytes for replicas.
 * Reply: *3 $gen $total $data  (data is nil when gen does not match). */
static void h_synclog(call_t *c) {
    uint64_t gen, off, maxb = SYNCLOG_DEFAULT, total = 0, rgen = 0;
    void *buf = NULL;
    size_t n = 0;
    int rc;
    char g[32], t[32];
    int gl, tl;
    if (!parse_u64(c, 1, &gen) || !parse_u64(c, 2, &off)) { sb_err(c->out, "bad SYNCLOG arguments"); return; }
    if (c->argc == 4 && (!parse_u64(c, 3, &maxb) || maxb == 0)) { sb_err(c->out, "bad SYNCLOG max"); return; }
    if (maxb > SYNCLOG_MAX) maxb = SYNCLOG_MAX;
    rc = superdb_log_read(c->ctx->db, off, (size_t)maxb, &buf, &n, &total, &rgen);
    gl = snprintf(g, sizeof g, "%" PRIu64, rgen);
    tl = snprintf(t, sizeof t, "%" PRIu64, total);
    sb_array(c->out, 3);
    sb_bulk(c->out, g, (size_t)gl);
    sb_bulk(c->out, t, (size_t)tl);
    if (rgen != gen) {
        sb_nil(c->out);
    } else if (off > total) {
        sb_err(c->out, "offset is past the end of the log");
    } else if (rc != 0) {
        sb_err(c->out, "offset is past the end of the log");
    } else {
        sb_bulk(c->out, buf, n);
    }
    superdb_free(buf);
}

static void h_command(call_t *c) {
    size_t n = 0, i;
    const cmd_def_t *tab = cmd_table(&n);
    sb_array(c->out, n);
    for (i = 0; i < n; i++) sb_bulk_str(c->out, tab[i].name);
}

/* ---------------- command table ---------------- */

static const cmd_def_t cmd_tab[] = {
    {"PING",        h_ping,        1, 2,  0, 0, 0},
    {"ECHO",        h_echo,        2, 2,  0, 0, 0},
    {"AUTH",        h_auth,        2, 2,  0, 1, 0},
    {"QUIT",        h_quit,        1, 1,  0, 1, 0},
    {"EXIT",        h_quit,        1, 1,  0, 1, 0},
    {"SHUTDOWN",    h_shutdown,    1, 1,  0, 0, 0},
    {"SET",         h_set,         3, 5,  1, 0, 0},
    {"SETNX",       h_setnx,       3, 3,  1, 0, 0},
    {"GET",         h_get,         2, 2,  0, 0, 0},
    {"GETSET",      h_getset,      3, 3,  1, 0, 0},
    {"MSETNX",      h_msetnx,      3, -1, 1, 0, 0},
    {"DEL",         h_del,         2, -1, 1, 0, 0},
    {"EXISTS",      h_exists,      2, -1, 0, 0, 0},
    {"KEYS",        h_keys,        1, 2,  0, 0, 0},
    {"SCAN",        h_scan,        2, -1, 0, 0, 0},
    {"TYPE",        h_type,        2, 2,  0, 0, 0},
    {"RENAME",      h_rename,      3, 3,  1, 0, 0},
    {"EXPIRE",      h_expire,      3, 3,  1, 0, 0},
    {"EXPIREAT",    h_expireat,    3, 3,  1, 0, 0},
    {"PERSIST",     h_persist,     2, 2,  1, 0, 0},
    {"TTL",         h_ttl,         2, 2,  0, 0, 0},
    {"PTTL",        h_ttl,         2, 2,  0, 0, 0},
    {"INCR",        h_incr,        2, 3,  1, 0, 0},
    {"INCRBY",      h_incr,        3, 3,  1, 0, 0},
    {"LPUSH",       h_lpush,       3, -1, 1, 0, 0},
    {"RPUSH",       h_rpush,       3, -1, 1, 0, 0},
    {"LPOP",        h_lpop,        2, 2,  1, 0, 0},
    {"RPOP",        h_rpop,        2, 2,  1, 0, 0},
    {"LLEN",        h_llen,        2, 2,  0, 0, 0},
    {"LRANGE",      h_lrange,      4, 4,  0, 0, 0},
    {"HSET",        h_hset,        4, -1, 1, 0, 0},
    {"HGET",        h_hget,        3, 3,  0, 0, 0},
    {"HDEL",        h_hdel,        3, -1, 1, 0, 0},
    {"HLEN",        h_hlen,        2, 2,  0, 0, 0},
    {"HGETALL",     h_hgetall,     2, 2,  0, 0, 0},
    {"SADD",        h_sadd,        3, -1, 1, 0, 0},
    {"SREM",        h_srem,        3, -1, 1, 0, 0},
    {"SISMEMBER",   h_sismember,   3, 3,  0, 0, 0},
    {"SCARD",       h_scard,       2, 2,  0, 0, 0},
    {"SMEMBERS",    h_smembers,    2, 2,  0, 0, 0},
    {"DBSIZE",      h_dbsize,      1, 1,  0, 0, 0},
    {"FLUSHALL",    h_flushall,    1, 1,  1, 0, 0},
    {"COMPACT",     h_compact,     1, 1,  1, 0, 0},
    {"SAVE",        h_compact,     1, 1,  1, 0, 0},
    {"BGREWRITEAOF",h_bgrewriteaof,1, 1,  1, 0, 0},
    {"BACKUP",      h_backup,      2, 2,  0, 0, 0},
    {"INFO",        h_info,        1, 2,  0, 0, 0},
    {"SLOWLOG",     h_slowlog,     2, 3,  0, 0, 0},
    {"SYNCLOG",     h_synclog,     3, 4,  0, 0, 0},
    {"MULTI",       h_multi,       1, 1,  0, 0, 1},
    {"EXEC",        h_exec,        1, 1,  0, 0, 1},
    {"DISCARD",     h_discard,     1, 1,  0, 0, 1},
    {"COMMAND",     h_command,     1, -1, 0, 0, 0},
    {"HELP",        h_command,     1, -1, 0, 0, 0}
};

static const cmd_def_t *cmd_table(size_t *n) {
    *n = sizeof cmd_tab / sizeof cmd_tab[0];
    return cmd_tab;
}

static const cmd_def_t *cmd_lookup(const char *name) {
    size_t i, n = sizeof cmd_tab / sizeof cmd_tab[0];
    for (i = 0; i < n; i++)
        if (sdb_cmd_eq(name, cmd_tab[i].name)) return &cmd_tab[i];
    return NULL;
}

/* ---------------- context & session ---------------- */

sdb_ctx_t *sdb_ctx_new(superdb_t *db, const char *password, long long slowlog_us) {
    sdb_ctx_t *ctx = (sdb_ctx_t*)calloc(1, sizeof *ctx);
    size_t n = sizeof cmd_tab / sizeof cmd_tab[0];
    if (!ctx) return NULL;
    ctx->db = db;
    ctx->slowlog_us = slowlog_us;
    ctx->start_ms = sdb_now_ms();
    if (password && *password) {
        ctx->password = (char*)malloc(strlen(password) + 1);
        if (!ctx->password) { free(ctx); return NULL; }
        strcpy(ctx->password, password);
    }
    ctx->calls = (uint64_t*)calloc(n, sizeof *ctx->calls);
    ctx->usec = (int64_t*)calloc(n, sizeof *ctx->usec);
    if (!ctx->calls || !ctx->usec || sdb_mutex_init(&ctx->mu) != 0) {
        free(ctx->calls); free(ctx->usec); free(ctx->password); free(ctx);
        return NULL;
    }
    return ctx;
}

void sdb_ctx_free(sdb_ctx_t *ctx) {
    if (!ctx) return;
    sdb_mutex_destroy(&ctx->mu);
    free(ctx->calls);
    free(ctx->usec);
    free(ctx->password);
    free(ctx);
}

superdb_t *sdb_ctx_db(sdb_ctx_t *ctx) { return ctx->db; }

void sdb_ctx_set_readonly(sdb_ctx_t *ctx, int ro) { ctx->readonly = ro ? 1 : 0; }

void sdb_ctx_set_primary(sdb_ctx_t *ctx, const char *host, int port) {
    snprintf(ctx->primary, sizeof ctx->primary, "%s:%d", host, port);
}

void sdb_ctx_request_shutdown(sdb_ctx_t *ctx) {
    sdb_mutex_lock(&ctx->mu);
    ctx->shutdown = 1;
    sdb_mutex_unlock(&ctx->mu);
}

int sdb_ctx_shutdown_requested(sdb_ctx_t *ctx) {
    int r;
    sdb_mutex_lock(&ctx->mu);
    r = ctx->shutdown;
    sdb_mutex_unlock(&ctx->mu);
    return r;
}

void sdb_ctx_conn_opened(sdb_ctx_t *ctx) {
    sdb_mutex_lock(&ctx->mu);
    ctx->conns_total++;
    ctx->conns_open++;
    sdb_mutex_unlock(&ctx->mu);
}

void sdb_ctx_conn_closed(sdb_ctx_t *ctx) {
    sdb_mutex_lock(&ctx->mu);
    if (ctx->conns_open) ctx->conns_open--;
    sdb_mutex_unlock(&ctx->mu);
}

void sdb_session_init(sdb_session_t *s, sdb_ctx_t *ctx) {
    memset(s, 0, sizeof *s);
    s->authed = ctx->password == NULL;
}

void sdb_session_free(sdb_session_t *s) {
    session_clear_queue(s);
    free(s->q);
    s->q = NULL;
    s->qcap = 0;
}

/* ---------------- dispatch ---------------- */

static void lowercase_into(char *dst, size_t cap, const char *src) {
    size_t i;
    for (i = 0; src[i] && i + 1 < cap; i++)
        dst[i] = (char)((src[i] >= 'A' && src[i] <= 'Z') ? src[i] + 32 : src[i]);
    dst[i < cap ? i : cap - 1] = '\0';
}

static void run_cmd(sdb_ctx_t *ctx, sdb_session_t *s, const cmd_def_t *def,
                    char **argv, size_t *arglen, int argc, sbuf_t *out, int *quit) {
    call_t c;
    int64_t t0, dt;
    size_t idx = (size_t)(def - cmd_tab);
    c.ctx = ctx; c.s = s; c.argc = argc; c.argv = argv; c.al = arglen;
    c.out = out; c.quit = quit; c.def = def;
    *quit = 0;

    if (argc < def->min || (def->max >= 0 && argc > def->max)) {
        char nm[32];
        char msg[128];
        lowercase_into(nm, sizeof nm, def->name);
        snprintf(msg, sizeof msg, "wrong number of arguments for '%s' command", nm);
        sb_err(out, msg);
        return;
    }
    if (def->write && ctx->readonly) {
        sb_raw_err(out, "READONLY You can't write against a read only replica.");
        return;
    }
    t0 = sdb_now_us();
    def->fn(&c);
    dt = sdb_now_us() - t0;

    sdb_mutex_lock(&ctx->mu);
    ctx->cmds_total++;
    ctx->calls[idx]++;
    ctx->usec[idx] += dt;
    if (ctx->slowlog_us >= 0 && dt >= ctx->slowlog_us) slowlog_add(ctx, &c, dt);
    sdb_mutex_unlock(&ctx->mu);
}

static int queue_request(sdb_session_t *s, char **argv, size_t *arglen, int argc) {
    if (s->nq == s->qcap) {
        size_t nc = s->qcap ? s->qcap * 2 : 8;
        sdb_queued_t *nq = (sdb_queued_t*)realloc(s->q, nc * sizeof *nq);
        if (!nq) return -1;
        s->q = nq;
        s->qcap = nc;
    }
    s->q[s->nq].argv = argv;
    s->q[s->nq].arglen = arglen;
    s->q[s->nq].argc = argc;
    s->nq++;
    return 0;
}

int sdb_dispatch(sdb_ctx_t *ctx, sdb_session_t *s, char **argv, size_t *arglen,
                 int argc, sbuf_t *out, int *quit) {
    const cmd_def_t *def;
    *quit = 0;
    if (argc < 1) { sb_err(out, "empty command"); return 0; }
    def = cmd_lookup(argv[0]);
    if (!def) { sb_err(out, "unknown command"); return 0; }
    if (!s->authed && !def->noauth) {
        sb_raw_err(out, "NOAUTH Authentication required.");
        return 0;
    }
    if (s->in_multi && !def->ctl) {
        if (queue_request(s, argv, arglen, argc) != 0) {
            sb_err(out, "out of memory");
            return 0;
        }
        sb_str(out, "+QUEUED\r\n");
        return 1;
    }
    run_cmd(ctx, s, def, argv, arglen, argc, out, quit);
    if (*quit == 2) sdb_ctx_request_shutdown(ctx);
    return 0;
}
