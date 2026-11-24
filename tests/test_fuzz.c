/* test_fuzz — deterministic fuzzing (fixed seeds, so failures reproduce).
 *   1. Random bytes into the RESP and inline parsers: never crash, results in range.
 *   2. Random typed commands through the command layer. Then the state must be
 *      identical after a reopen, and after compaction plus reopen. That is the
 *      core guarantee: the log, replayed, reproduces the live memory state.
 */
#include <stdio.h>
#include <string.h>
#include <stdlib.h>
#include "superdb.h"
#include "sdb_cmd.h"
#include "sdb_resp.h"
#include "sdb_util.h"

#define CHECK(c, msg) do { \
    if (!(c)) { printf("FAIL: %s (line %d)\n", msg, __LINE__); return 1; } \
    else printf("ok: %s\n", msg); } while (0)

static uint64_t rng_state = 0x5eed5eed12345678ULL;
static uint32_t rnd(uint32_t n) {
    return n ? (uint32_t)(sdb_rand64(&rng_state) % n) : 0;
}

/* ---------- 1. parser fuzzing ---------- */

static int parser_fuzz(void) {
    static const char alphabet[] = "*$:+-\r\n0123456789 abcSETGET";
    int iter;
    for (iter = 0; iter < 200000; iter++) {
        char buf[96];
        size_t len = rnd(sizeof buf), i;
        char **argv = NULL; size_t *al = NULL; int argc = 0; size_t consumed = 0;
        int r;
        for (i = 0; i < len; i++) {
            if (rnd(4) == 0) buf[i] = (char)rnd(256);
            else buf[i] = alphabet[rnd(sizeof alphabet - 1)];
        }
        r = sdb_parse_resp(buf, len, &argv, &al, &argc, &consumed);
        if (r < -1 || r > 1) { printf("FAIL: resp parser returned %d\n", r); return 1; }
        if (r == 1 && consumed > len) { printf("FAIL: resp consumed past input\n"); return 1; }
        sdb_free_argv(argv, al, argc);
        argv = NULL; al = NULL; argc = 0; consumed = 0;
        r = sdb_parse_inline(buf, len, &argv, &al, &argc, &consumed);
        if (r < 0 || r > 1) { printf("FAIL: inline parser returned %d\n", r); return 1; }
        if (r == 1 && consumed > len) { printf("FAIL: inline consumed past input\n"); return 1; }
        sdb_free_argv(argv, al, argc);
    }
    return 0;
}

/* ---------- 2. typed command fuzzing ---------- */

typedef struct { char *p; size_t n; } bin_t;

static int cmp_bin(const void *a, const void *b) {
    const bin_t *x = (const bin_t*)a, *y = (const bin_t*)b;
    size_t m = x->n < y->n ? x->n : y->n;
    int c = memcmp(x->p, y->p, m);
    if (c) return c;
    return x->n < y->n ? -1 : (x->n > y->n);
}

static int cmp_cstr(const void *a, const void *b) {
    return strcmp(*(char *const *)a, *(char *const *)b);
}

/* Canonical text of the whole database: independent of hash-table layout. */
static void dump_db(superdb_t *db, sdb_buf_t *out) {
    superdb_keys_t ks = superdb_keys(db, NULL);
    size_t i;
    qsort(ks.keys, ks.count, sizeof *ks.keys, cmp_cstr);
    out->n = 0;
    for (i = 0; i < ks.count; i++) {
        const char *k = ks.keys[i];
        int t = superdb_type(db, k);
        char head[32];
        int hl = snprintf(head, sizeof head, "%d|", t);
        sdb_buf_put(out, k, strlen(k));
        sdb_buf_put(out, "|", 1);
        sdb_buf_put(out, head, (size_t)hl);
        if (t == SUPERDB_T_STRING) {
            void *v = NULL; size_t n = 0;
            if (superdb_get_ex(db, k, &v, &n) == 1) { sdb_buf_put(out, v, n); superdb_free(v); }
        } else if (t == SUPERDB_T_LIST) {
            superdb_items_t it;
            if (superdb_lrange(db, k, 0, -1, &it) == 0) {
                size_t j;
                for (j = 0; j < it.count; j++) {
                    char l[24];
                    int ll = snprintf(l, sizeof l, "%lu:", (unsigned long)it.lens[j]);
                    sdb_buf_put(out, l, (size_t)ll);
                    sdb_buf_put(out, it.items[j], it.lens[j]);
                }
                superdb_free_items(&it);
            }
        } else if (t == SUPERDB_T_HASH) {
            superdb_items_t it;
            if (superdb_hgetall(db, k, &it) == 0) {
                size_t j, np = it.count / 2;
                bin_t *pairs = (bin_t*)malloc((np ? np : 1) * sizeof(bin_t) * 2);
                for (j = 0; j < np; j++) {
                    pairs[j * 2].p = (char*)it.items[j * 2];
                    pairs[j * 2].n = it.lens[j * 2];
                    pairs[j * 2 + 1].p = (char*)it.items[j * 2 + 1];
                    pairs[j * 2 + 1].n = it.lens[j * 2 + 1];
                }
                /* sort (field, value) pairs by field */
                {
                    size_t a, b;
                    for (a = 0; a < np; a++)
                        for (b = a + 1; b < np; b++)
                            if (cmp_bin(&pairs[a * 2], &pairs[b * 2]) > 0) {
                                bin_t t1 = pairs[a * 2], t2 = pairs[a * 2 + 1];
                                pairs[a * 2] = pairs[b * 2]; pairs[a * 2 + 1] = pairs[b * 2 + 1];
                                pairs[b * 2] = t1; pairs[b * 2 + 1] = t2;
                            }
                }
                for (j = 0; j < np; j++) {
                    sdb_buf_put(out, pairs[j * 2].p, pairs[j * 2].n);
                    sdb_buf_put(out, "=", 1);
                    sdb_buf_put(out, pairs[j * 2 + 1].p, pairs[j * 2 + 1].n);
                    sdb_buf_put(out, ";", 1);
                }
                free(pairs);
                superdb_free_items(&it);
            }
        } else if (t == SUPERDB_T_SET) {
            superdb_items_t it;
            if (superdb_smembers(db, k, &it) == 0) {
                bin_t *m = (bin_t*)malloc((it.count ? it.count : 1) * sizeof *m);
                size_t j;
                for (j = 0; j < it.count; j++) { m[j].p = (char*)it.items[j]; m[j].n = it.lens[j]; }
                qsort(m, it.count, sizeof *m, cmp_bin);
                for (j = 0; j < it.count; j++) { sdb_buf_put(out, m[j].p, m[j].n); sdb_buf_put(out, ";", 1); }
                free(m);
                superdb_free_items(&it);
            }
        }
        sdb_buf_put(out, "\n", 1);
    }
    superdb_free_keys(&ks);
}

/* Builds a RESP request from argument (ptr,len) pairs. Returns malloc'd bytes. */
typedef struct { const char *p; size_t n; } arg_t;

static char *build_resp(const arg_t *a, int argc, size_t *outlen) {
    sdb_buf_t b = {NULL, 0, 0};
    char hdr[32];
    int i;
    int hl = snprintf(hdr, sizeof hdr, "*%d\r\n", argc);
    sdb_buf_put(&b, hdr, (size_t)hl);
    for (i = 0; i < argc; i++) {
        int l = snprintf(hdr, sizeof hdr, "$%lu\r\n", (unsigned long)a[i].n);
        sdb_buf_put(&b, hdr, (size_t)l);
        sdb_buf_put(&b, a[i].p, a[i].n);
        sdb_buf_put(&b, "\r\n", 2);
    }
    *outlen = b.n;
    return (char*)b.p;
}

static const char *KEYS[] = {"k0", "k1", "k2", "k3", "k4", "k5", "k6", "k7"};

static void rand_bytes(char *dst, size_t *n, size_t max) {
    size_t i;
    *n = rnd((uint32_t)max + 1);
    for (i = 0; i < *n; i++) {
        uint32_t c = rnd(8);
        dst[i] = c == 0 ? '\0' : c == 1 ? '\r' : c == 2 ? '\n' : (char)('a' + rnd(6));
    }
}

static int command_fuzz(const char *path, int ops) {
    superdb_options_t o;
    superdb_t *db;
    sdb_ctx_t *ctx;
    sdb_session_t s;
    sdb_buf_t before = {NULL, 0, 0}, after = {NULL, 0, 0};
    int i;

    memset(&o, 0, sizeof o);
    o.sync_writes = 0;
    o.compact_threshold = 64 * 1024;   /* exercise background compaction too */
    remove(path); remove("fuzz.tmp");
    db = superdb_open(path, &o);
    if (!db) { printf("FAIL: open fuzz db\n"); return 1; }
    ctx = sdb_ctx_new(db, NULL, -1);
    sdb_session_init(&s, ctx);

    for (i = 0; i < ops; i++) {
        static const char *cmds[] = {
            "SET", "GET", "DEL", "EXISTS", "INCR", "INCRBY", "SETNX", "GETSET",
            "MSETNX", "RENAME", "EXPIRE", "PERSIST", "EXPIREAT", "TYPE",
            "LPUSH", "RPUSH", "LPOP", "RPOP", "LLEN", "LRANGE",
            "HSET", "HGET", "HDEL", "HLEN", "HGETALL",
            "SADD", "SREM", "SISMEMBER", "SCARD", "SMEMBERS", "SCAN", "KEYS"
        };
        const char *cmd = cmds[rnd(sizeof cmds / sizeof cmds[0])];
        arg_t a[8];
        char bufs[8][40];
        size_t lens[8];
        int argc = 0, j;
        char *req;
        size_t reqlen = 0;
        sbuf_t out;
        char **argv = NULL; size_t *al = NULL; int pargc = 0; size_t consumed = 0;
        int quit = 0;

        a[argc].p = cmd; a[argc].n = strlen(cmd); argc++;
        if (!strcmp(cmd, "SCAN")) {
            snprintf(bufs[0], sizeof bufs[0], "%u", (unsigned)rnd(64));
            a[argc].p = bufs[0]; a[argc].n = strlen(bufs[0]); argc++;
        } else if (!strcmp(cmd, "KEYS")) {
            a[argc].p = "k*"; a[argc].n = 2; argc++;
        } else {
            /* key: sometimes a binary field/value, otherwise one of the fixed keys */
            a[argc].p = KEYS[rnd(8)]; a[argc].n = 2; argc++;
            for (j = 0; j < 3; j++) {
                if (rnd(2)) {
                    rand_bytes(bufs[j], &lens[j], 6);
                    a[argc].p = bufs[j]; a[argc].n = lens[j]; argc++;
                } else {
                    snprintf(bufs[j], sizeof bufs[j], "%d", (int)rnd(20));
                    a[argc].p = bufs[j]; a[argc].n = strlen(bufs[j]); argc++;
                }
            }
            if (!strcmp(cmd, "MSETNX")) {
                a[argc].p = KEYS[rnd(8)]; a[argc].n = 2; argc++;
                a[argc].p = "v"; a[argc].n = 1; argc++;
            }
            if (!strcmp(cmd, "EXPIRE")) { a[2].p = "3600"; a[2].n = 4; }
            if (!strcmp(cmd, "EXPIREAT")) { a[2].p = "9999999999"; a[2].n = 10; }
            if (!strcmp(cmd, "RENAME")) { a[2].p = KEYS[rnd(8)]; a[2].n = 2; }
        }
        req = build_resp(a, argc, &reqlen);
        if (sdb_parse_resp(req, reqlen, &argv, &al, &pargc, &consumed) != 1) {
            printf("FAIL: built request did not parse (op %d)\n", i);
            return 1;
        }
        free(req);
        sb_init(&out);
        if (!sdb_dispatch(ctx, &s, argv, al, pargc, &out, &quit))
            sdb_free_argv(argv, al, pargc);   /* queued requests are owned by the session */
        if (out.len == 0 || !strchr("+-:$*", out.data[0])) {
            printf("FAIL: reply does not start with a RESP type byte (op %d, %s)\n", i, cmd);
            return 1;
        }
        sb_free(&out);

        /* sprinkle transactions into the stream */
        if (rnd(200) == 0) {
            sbuf_t o2; char **av = NULL; size_t *ql = NULL; int qc = 0; int qq = 0;
            const char *m = "MULTI", *e = "EXEC";
            arg_t ma = {m, 5}, ea = {e, 4};
            char *r1 = build_resp(&ma, 1, &reqlen);
            char *r2 = build_resp(&ea, 1, &reqlen);
            sb_init(&o2);
            sdb_parse_resp(r1, strlen(r1), &av, &ql, &qc, &consumed);
            sdb_dispatch(ctx, &s, av, ql, qc, &o2, &qq); sdb_free_argv(av, ql, qc);
            sb_free(&o2);
            sdb_parse_resp(r2, strlen(r2), &av, &ql, &qc, &consumed);
            sb_init(&o2);
            sdb_dispatch(ctx, &s, av, ql, qc, &o2, &qq); sdb_free_argv(av, ql, qc);
            sb_free(&o2);
            free(r1); free(r2);
        }
    }

    dump_db(db, &before);
    sdb_session_free(&s);
    sdb_ctx_free(ctx);
    superdb_close(db);

    /* reopen: the replayed log must give exactly the same state */
    db = superdb_open(path, &o);
    if (!db) { printf("FAIL: reopen fuzz db\n"); return 1; }
    dump_db(db, &after);
    if (before.n != after.n || memcmp(before.p, after.p, before.n) != 0) {
        printf("FAIL: state after reopen differs from live state\n");
        return 1;
    }
    printf("ok: %d random commands; state identical after reopen (%lu bytes of dump)\n",
           ops, (unsigned long)before.n);

    /* compaction must not change the state either */
    CHECK(superdb_compact(db) == 0, "compact the fuzzed database");
    after.n = 0;
    dump_db(db, &after);
    if (before.n != after.n || memcmp(before.p, after.p, before.n) != 0) {
        printf("FAIL: compaction changed the state\n");
        return 1;
    }
    superdb_close(db);
    db = superdb_open(path, &o);
    after.n = 0;
    dump_db(db, &after);
    if (before.n != after.n || memcmp(before.p, after.p, before.n) != 0) {
        printf("FAIL: state after compaction and reopen differs\n");
        return 1;
    }
    superdb_close(db);
    printf("ok: state identical after compaction and reopen\n");

    sdb_buf_free(&before);
    sdb_buf_free(&after);
    remove(path); remove("fuzz.tmp");
    return 0;
}

int main(void) {
    setvbuf(stdout, NULL, _IONBF, 0);
    if (parser_fuzz() != 0) return 1;
    printf("ok: 200000 random inputs through the RESP and inline parsers\n");
    if (command_fuzz("test_fuzz.sdb", 20000) != 0) return 1;
    printf("\nALL FUZZ TESTS PASSED\n");
    return 0;
}
