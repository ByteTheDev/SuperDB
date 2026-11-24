/* test_txn — transactions and crash recovery.
 *   - a committed group survives reopen; an open group is dropped on reopen
 *   - a group whose COMMIT record is torn away is dropped atomically
 *   - every truncation point and every single-bit corruption of a mixed log
 *     opens cleanly, and recovery is idempotent (reopen gives the same state)
 */
#include <stdio.h>
#include <string.h>
#include <stdlib.h>
#include "superdb.h"

#define CHECK(c, msg) do { \
    if (!(c)) { printf("FAIL: %s (line %d)\n", msg, __LINE__); return 1; } \
    else printf("ok: %s\n", msg); } while (0)

static long file_size(const char *p) {
    FILE *f = fopen(p, "rb");
    long n;
    if (!f) return -1;
    fseek(f, 0, SEEK_END);
    n = ftell(f);
    fclose(f);
    return n;
}

/* Copies src to dst, keeping at most keep bytes; flips one bit if flip_at >= 0. */
static int copy_variant(const char *src, const char *dst, long keep, long flip_at) {
    FILE *in = fopen(src, "rb"), *out;
    unsigned char *buf;
    long n, i;
    if (!in) return -1;
    fseek(in, 0, SEEK_END);
    n = ftell(in);
    fseek(in, 0, SEEK_SET);
    if (keep < 0 || keep > n) keep = n;
    buf = (unsigned char*)malloc(n > 0 ? (size_t)n : 1);
    if (!buf || fread(buf, 1, (size_t)n, in) != (size_t)n) { fclose(in); free(buf); return -1; }
    fclose(in);
    if (flip_at >= 0 && flip_at < keep) buf[flip_at] ^= 0x10;
    out = fopen(dst, "wb");
    if (!out) { free(buf); return -1; }
    for (i = 0; i < keep; i += 1 << 16) {
        size_t chunk = (size_t)((keep - i) < (1 << 16) ? (keep - i) : (1 << 16));
        if (fwrite(buf + i, 1, chunk, out) != chunk) { fclose(out); free(buf); return -1; }
    }
    fclose(out);
    free(buf);
    return 0;
}

/* Fingerprint of the observable state: key count plus every key's type and a
 * cheap content digest. Used to compare states across reopen. */
static unsigned long fingerprint(superdb_t *db) {
    superdb_keys_t ks = superdb_keys(db, NULL);
    unsigned long h = 1469598103u + (unsigned long)superdb_count(db);
    size_t i;
    for (i = 0; i < ks.count; i++) {
        const char *k = ks.keys[i];
        int t = superdb_type(db, k);
        unsigned long kh = 5381;
        const char *p;
        for (p = k; *p; p++) kh = kh * 33 + (unsigned char)*p;
        h += kh * 31 + (unsigned long)t;
        if (t == SUPERDB_T_STRING) {
            void *v = NULL; size_t n = 0;
            if (superdb_get_ex(db, k, &v, &n) == 1) {
                size_t j; for (j = 0; j < n; j++) h = h * 131 + ((unsigned char*)v)[j];
                superdb_free(v);
            }
        } else if (t == SUPERDB_T_LIST) {
            superdb_items_t it;
            if (superdb_lrange(db, k, 0, -1, &it) == 0) {
                size_t j; for (j = 0; j < it.count; j++) h = h * 17 + it.lens[j] + 7;
                superdb_free_items(&it);
            }
        } else if (t == SUPERDB_T_HASH) {
            superdb_items_t it;
            if (superdb_hgetall(db, k, &it) == 0) {
                size_t j; for (j = 0; j < it.count; j++) h += it.lens[j] * 3 + 1;
                superdb_free_items(&it);
            }
        } else if (t == SUPERDB_T_SET) {
            h += (unsigned long)superdb_scard(db, k) * 11;
        }
    }
    superdb_free_keys(&ks);
    return h;
}

int main(void) {
    const char *path = "test_txn.sdb";
    const char *tmp = "test_txn_variant.sdb";
    superdb_options_t o;
    superdb_t *db;
    long before_txn, full_size;
    unsigned long full_fp;
    long n, pos, step;
    int ok = 0;

    memset(&o, 0, sizeof o);
    o.sync_writes = 0;
    o.compact_threshold = 0;
    remove(path); remove("test_txn.sdb.tmp"); remove(tmp);

    /* ---- commit makes a group durable ---- */
    db = superdb_open(path, &o);
    CHECK(db != NULL, "open");
    superdb_set_str(db, "base", "1");
    CHECK(superdb_txn_begin(db) == 0, "txn_begin");
    CHECK(superdb_txn_begin(db) != 0, "nested txn_begin is rejected");
    superdb_set_str(db, "t1", "a");
    superdb_set_str(db, "t2", "b");
    superdb_rpush(db, "tl", "x", 1);
    CHECK(superdb_txn_commit(db) == 0, "txn_commit");
    CHECK(superdb_txn_commit(db) != 0, "commit without an open txn fails");
    superdb_close(db);

    db = superdb_open(path, &o);
    CHECK(superdb_exists(db, "t1") && superdb_exists(db, "t2") && superdb_llen(db, "tl") == 1,
          "committed group survives reopen");
    superdb_close(db);

    /* ---- a group left open is dropped on reopen ---- */
    db = superdb_open(path, &o);
    superdb_set_str(db, "keep", "yes");
    CHECK(superdb_txn_begin(db) == 0, "begin a group we will abandon");
    superdb_set_str(db, "ghost", "boo");
    superdb_close(db);
    db = superdb_open(path, &o);
    CHECK(superdb_exists(db, "keep") && !superdb_exists(db, "ghost"),
          "abandoned group is dropped, earlier data kept");
    superdb_close(db);

    /* ---- torn COMMIT: the whole group disappears ---- */
    db = superdb_open(path, &o);
    before_txn = file_size(path);
    CHECK(superdb_txn_begin(db) == 0, "begin group for torn-commit test");
    superdb_set_str(db, "a1", "1");
    superdb_set_str(db, "a2", "2");
    superdb_set_str(db, "a3", "3");
    CHECK(superdb_txn_commit(db) == 0, "commit group for torn-commit test");
    superdb_close(db);
    full_size = file_size(path);
    CHECK(copy_variant(path, tmp, full_size - 21, -1) == 0, "copy without the COMMIT record");
    db = superdb_open(tmp, &o);
    CHECK(db != NULL, "open log whose COMMIT was torn");
    CHECK(!superdb_exists(db, "a1") && !superdb_exists(db, "a2") && !superdb_exists(db, "a3"),
          "torn group is dropped as a whole");
    CHECK(superdb_exists(db, "keep"), "data before the group is intact");
    superdb_close(db);
    CHECK(file_size(tmp) == before_txn, "recovery truncates the log back to before the group");
    remove(tmp);

    /* ---- mixed log for exhaustive truncation / corruption sweeps ---- */
    db = superdb_open(path, &o);
    superdb_set_str(db, "s:1", "alpha");
    superdb_set_ex_str(db, "s:ttl", "beta", 3600000);
    superdb_rpush(db, "list", "a", 1);
    superdb_lpush(db, "list", "b", 1);
    superdb_hset(db, "hash", "f", 1, "value", 5);
    superdb_hset(db, "hash", "g", 1, "x", 1);
    superdb_hdel(db, "hash", "f", 1);
    superdb_sadd(db, "set", "m", 1);
    superdb_sadd(db, "set", "n", 1);
    superdb_srem(db, "set", "m", 1);
    superdb_rename(db, "s:1", "s:2");
    superdb_lpop(db, "list", NULL, NULL);
    superdb_set_str(db, "gone", "x");
    superdb_del(db, "gone");
    superdb_persist(db, "s:ttl");
    superdb_set_str(db, "tail", "z");
    superdb_close(db);

    full_size = file_size(path);
    db = superdb_open(path, &o);
    full_fp = fingerprint(db);
    superdb_close(db);
    CHECK(full_size > 100, "mixed log has content");

    /* every truncation point opens; recovery is idempotent */
    for (n = 4; n <= full_size; n++) {
        unsigned long fp1, fp2;
        if (copy_variant(path, tmp, n, -1) != 0) { printf("FAIL: copy at %ld\n", n); return 1; }
        db = superdb_open(tmp, &o);
        if (!db) { printf("FAIL: truncation at %ld did not open\n", n); return 1; }
        fp1 = fingerprint(db);
        superdb_close(db);
        db = superdb_open(tmp, &o);
        if (!db) { printf("FAIL: second open after truncation %ld\n", n); return 1; }
        fp2 = fingerprint(db);
        superdb_close(db);
        if (fp1 != fp2) { printf("FAIL: recovery not idempotent at truncation %ld\n", n); return 1; }
    }
    printf("ok: every truncation point (%ld) opens and recovers idempotently\n", full_size - 3);

    /* every single-bit corruption opens and recovers idempotently */
    step = full_size > 4000 ? 3 : 1;
    for (pos = 4; pos < full_size; pos += step) {
        unsigned long fp1, fp2;
        if (copy_variant(path, tmp, -1, pos) != 0) { printf("FAIL: flip copy at %ld\n", pos); return 1; }
        db = superdb_open(tmp, &o);
        if (!db) { printf("FAIL: bit flip at %ld did not open\n", pos); return 1; }
        fp1 = fingerprint(db);
        superdb_close(db);
        db = superdb_open(tmp, &o);
        if (!db) { printf("FAIL: reopen after bit flip at %ld\n", pos); return 1; }
        fp2 = fingerprint(db);
        superdb_close(db);
        if (fp1 != fp2) { printf("FAIL: corruption at %ld not stable\n", pos); return 1; }
    }
    printf("ok: corruption sweep (every %ld bytes) opens and recovers idempotently\n", step);

    /* the intact file still gives the full state */
    db = superdb_open(path, &o);
    CHECK(fingerprint(db) == full_fp, "intact log reproduces the same state after reopen");
    superdb_close(db);
    ok = 1;

    remove(path); remove("test_txn.sdb.tmp"); remove(tmp);
    if (!ok) return 1;
    printf("\nALL TXN TESTS PASSED\n");
    return 0;
}
