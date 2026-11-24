/* superdb - public API: open/close, strings, lists, hashes, sets, expiry,
 * scans, transactions, maintenance and replication primitives.
 * Every mutation is logged first, then applied through sdb_apply_locked(),
 * the same code replay uses, so the log always reproduces the memory state.
 * Portable C99, no dependencies. Thread-safe via the db mutex. */
#include "superdb.h"
#include "superdb_int.h"
#include "superdb_store.h"
#include "superdb_log.h"
#include "superdb_coll.h"
#include "superdb_glob.h"
#include "superdb_crc.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <stdint.h>
#include <inttypes.h>
#include <errno.h>

#define WRONGTYPE_MSG "WRONGTYPE Operation against a key holding the wrong kind of value"

static int wrongtype(superdb_t *db) {
    sdb_set_err(db, WRONGTYPE_MSG);
    return SUPERDB_WRONGTYPE;
}

static int key_ok(const char *key) {
    return key && *key && strlen(key) <= SDB_MAX_KEY;
}

/* ---------------- internal helpers (lock held) ---------------- */

static void remove_key(superdb_t *db, const char *key, size_t klen, uint64_t h) {
    sdb_entry_t *o = NULL;
    if (sdb_remove_h_locked(db, key, klen, h, &o) && o) {
        db->dead_bytes += sdb_entry_bytes(o);
        sdb_free_entry(o);
    }
}

/* Logs a DEL and removes the key. The key must exist. */
static void drop_locked(superdb_t *db, const char *key, size_t klen, uint64_t h) {
    sdb_append_locked(db, SDB_OP_DEL, 0, key, klen, NULL, 0);
    remove_key(db, key, klen, h);
}

/* Lookup that treats an expired key as absent (and drops it, logging DEL). */
static sdb_entry_t *live_locked(superdb_t *db, const char *key, size_t klen, uint64_t h) {
    sdb_entry_t *e = sdb_lookup_h_locked(db, key, klen, h);
    if (e && e->expire_at && sdb_now_ms() >= e->expire_at) {
        db->expired_keys++;
        drop_locked(db, key, klen, h);
        return NULL;
    }
    return e;
}

/* Logs a mutation and applies it. Returns 0 on success. */
static int write_locked(superdb_t *db, unsigned op, int64_t expire_at,
                        const char *key, size_t klen, const void *val, size_t vlen) {
    if (sdb_append_locked(db, op, expire_at, key, klen, val, vlen) != 0) return -1;
    return sdb_apply_locked(db, op, expire_at, key, klen, val, vlen);
}

static int keys_push(superdb_keys_t *k, const char *s, size_t n) {
    char *c;
    if (k->count == 0 || k->count % 64 == 0) {
        size_t nc = k->count + 64;
        char **nk = (char**)realloc(k->keys, nc * sizeof(*nk));
        if (!nk) return -1;
        k->keys = nk;
    }
    c = (char*)malloc(n + 1);
    if (!c) return -1;
    memcpy(c, s, n);
    c[n] = '\0';
    k->keys[k->count++] = c;
    return 0;
}

static void *dup_bytes(const void *p, size_t n) {
    char *c = (char*)malloc(n + 1);
    if (!c) return NULL;
    if (n) memcpy(c, p, n);
    c[n] = '\0';
    return c;
}

/* ---------------- lifecycle ---------------- */

static int create_file(const char *path) {
    FILE *w = fopen(path, "wb");
    if (!w) return -1;
    if (fwrite(SDB_MAGIC, 1, SDB_MAGIC_LEN, w) != SDB_MAGIC_LEN) { fclose(w); return -1; }
    sdb_fsync(w);
    fclose(w);
    return 0;
}

static void free_all_entries(superdb_t *db) {
    size_t i;
    if (!db->buckets) return;
    for (i = 0; i < db->nbuckets; i++) {
        sdb_entry_t *e = db->buckets[i];
        while (e) { sdb_entry_t *n = e->next; sdb_free_entry(e); e = n; }
        db->buckets[i] = NULL;
    }
    db->count = 0;
}

static void destroy_db(superdb_t *db) {
    free_all_entries(db);
    free(db->buckets);
    sdb_buf_free(&db->delta);
    sdb_mutex_destroy(&db->lock);
    free(db);
}

superdb_t *superdb_open(const char *path, const superdb_options_t *opts) {
    superdb_t *db;
    FILE *f;
    long size = 0;
    size_t good = SDB_MAGIC_LEN;
    int legacy = 0;

    if (!path || !*path) return NULL;
    sdb_crc_init();
    db = (superdb_t*)calloc(1, sizeof(*db));
    if (!db) return NULL;
    strncpy(db->path, path, sizeof(db->path) - 1);
    if (!opts) {
        db->sync_writes = 1;
        db->compact_threshold = 4u * 1024u * 1024u;
    } else {
        db->sync_writes = opts->sync_writes ? 1 : 0;
        db->compact_threshold = opts->compact_threshold; /* 0 = disable */
    }
    db->nbuckets = sdb_pow2(opts && opts->initial_buckets ? opts->initial_buckets : 1024);
    db->buckets = (sdb_entry_t**)calloc(db->nbuckets, sizeof(*db->buckets));
    if (!db->buckets) { free(db); return NULL; }
    db->rng = (uint64_t)sdb_now_us() ^ (uint64_t)(uintptr_t)db;
    if (sdb_mutex_init(&db->lock) != 0) { free(db->buckets); free(db); return NULL; }

    f = fopen(path, "rb");
    if (!f) {
        if (create_file(path) != 0) { sdb_set_err(db, "cannot create db file"); destroy_db(db); return NULL; }
        f = fopen(path, "rb");
    }
    if (!f) { sdb_set_err(db, "cannot read db file"); destroy_db(db); return NULL; }
    fseek(f, 0, SEEK_END);
    size = ftell(f);
    fseek(f, 0, SEEK_SET);

    if (size < SDB_MAGIC_LEN) {
        /* new or torn-header file: start over with an empty log */
        fclose(f);
        f = NULL;
        if (create_file(path) != 0) { sdb_set_err(db, "cannot reset db file"); destroy_db(db); return NULL; }
    } else {
        char magic[SDB_MAGIC_LEN];
        if (fread(magic, 1, SDB_MAGIC_LEN, f) != SDB_MAGIC_LEN ||
            (memcmp(magic, SDB_MAGIC, SDB_MAGIC_LEN) != 0 &&
             memcmp(magic, SDB_MAGIC_V1, SDB_MAGIC_LEN) != 0)) {
            sdb_set_err(db, "bad magic: not a superdb file");
            fclose(f);
            destroy_db(db);
            return NULL;
        }
        legacy = memcmp(magic, SDB_MAGIC_V1, SDB_MAGIC_LEN) == 0;
        if (sdb_replay_locked(db, f, SDB_MAGIC_LEN, &good, legacy) != 0) {
            sdb_set_err(db, "cannot replay log");
            fclose(f);
            destroy_db(db);
            return NULL;
        }
        fclose(f);
        f = NULL;
    }
    sdb_sweep_expired_locked(db);

    db->log = fopen(path, "a+b");
    if (!db->log) { sdb_set_err(db, "cannot open db file for append"); destroy_db(db); return NULL; }
    sdb_buf_log(db->log);
    if ((size_t)size > good) sdb_truncate(db->log, (long)good);  /* drop torn tail */
    fseek(db->log, 0, SEEK_END);
    db->log_size = good;
    db->replay_off = good;
    db->gen = sdb_rand64(&db->rng);

    if (legacy) {
        /* SDB1 -> SDB2: rewrite once; the new header carries the new format. */
        sdb_lock(db);
        if (sdb_compact_sync_locked(db) != 0) {
            sdb_unlock(db);
            superdb_close(db);
            return NULL;
        }
        sdb_unlock(db);
    }
    return db;
}

void superdb_close(superdb_t *db) {
    if (!db) return;
    sdb_wait_compact(db);
    sdb_lock(db);
    if (db->log) {
        fflush(db->log);
        if (db->sync_writes) sdb_fsync(db->log);
        fclose(db->log);
        db->log = NULL;
    }
    sdb_unlock(db);
    destroy_db(db);
}

const char *superdb_err(const superdb_t *db) {
    return db ? db->errmsg : "null db";
}

void superdb_free(void *p) { free(p); }

void superdb_free_items(superdb_items_t *it) {
    size_t i;
    if (!it) return;
    for (i = 0; i < it->count; i++) free(it->items[i]);
    free(it->items);
    free(it->lens);
    it->items = NULL; it->lens = NULL; it->count = 0;
}

/* ---------------- strings ---------------- */

int superdb_set(superdb_t *db, const char *key, const void *val, size_t vlen) {
    return superdb_set_ex(db, key, val, vlen, 0);
}

int superdb_set_str(superdb_t *db, const char *key, const char *val) {
    return superdb_set_ex(db, key, val, val ? strlen(val) : 0, 0);
}

int superdb_set_ex(superdb_t *db, const char *key,
                   const void *val, size_t vlen, int64_t ttl_ms) {
    size_t klen;
    int64_t expire_at = 0;
    int rc = 0;

    if (!db || !key || !*key) { sdb_set_err(db, "bad key"); return -1; }
    klen = strlen(key);
    if (klen > SDB_MAX_KEY || vlen > SDB_MAX_VAL) { sdb_set_err(db, "key/value too large"); return -1; }
    if (vlen && !val) { sdb_set_err(db, "null value"); return -1; }
    if (ttl_ms > 0) expire_at = sdb_now_ms() + ttl_ms;
    else if (ttl_ms < 0) { sdb_set_err(db, "bad ttl"); return -1; }

    sdb_lock(db);
    if (write_locked(db, SDB_OP_SET, expire_at, key, klen, val, vlen) != 0) rc = -1;
    sdb_maybe_compact_locked(db);
    sdb_unlock(db);
    return rc;
}

int superdb_set_ex_str(superdb_t *db, const char *key, const char *val, int64_t ttl_ms) {
    return superdb_set_ex(db, key, val, val ? strlen(val) : 0, ttl_ms);
}

int superdb_get_ex(superdb_t *db, const char *key, void **out, size_t *out_len) {
    sdb_entry_t *e;
    size_t klen;
    char *p;
    int rc = 1;
    if (out) *out = NULL;
    if (out_len) *out_len = 0;
    if (!db || !key || !out) return 0;
    klen = strlen(key);
    sdb_lock(db);
    e = live_locked(db, key, klen, sdb_hash_len(key, klen));
    if (!e) { db->misses++; sdb_unlock(db); return 0; }
    db->hits++;
    if (e->type != SDB_T_STRING) { sdb_unlock(db); return wrongtype(db); }
    p = (char*)malloc(e->vlen + 1);
    if (!p) { sdb_unlock(db); return SUPERDB_ERR; }
    if (e->vlen) memcpy(p, e->val, e->vlen);
    p[e->vlen] = '\0';
    *out = p;
    if (out_len) *out_len = e->vlen;
    sdb_unlock(db);
    return rc;
}

void *superdb_get(superdb_t *db, const char *key, size_t *out_len) {
    void *v = NULL;
    size_t n = 0;
    if (superdb_get_ex(db, key, &v, &n) != 1) return NULL;
    if (out_len) *out_len = n;
    return v;
}

char *superdb_get_str(superdb_t *db, const char *key) {
    return (char*)superdb_get(db, key, NULL);
}

int superdb_setnx(superdb_t *db, const char *key, const void *val, size_t vlen) {
    size_t klen;
    int rc;
    if (!db || !key_ok(key) || (vlen && !val) || vlen > SDB_MAX_VAL) return SUPERDB_ERR;
    klen = strlen(key);
    sdb_lock(db);
    if (live_locked(db, key, klen, sdb_hash_len(key, klen))) { sdb_unlock(db); return 0; }
    rc = write_locked(db, SDB_OP_SET, 0, key, klen, val, vlen) == 0 ? 1 : SUPERDB_ERR;
    sdb_maybe_compact_locked(db);
    sdb_unlock(db);
    return rc;
}

int superdb_getset(superdb_t *db, const char *key, const void *val, size_t vlen,
                   void **old, size_t *old_len) {
    sdb_entry_t *e;
    size_t klen;
    int rc = 1;
    if (old) *old = NULL;
    if (old_len) *old_len = 0;
    if (!db || !key_ok(key) || (vlen && !val) || vlen > SDB_MAX_VAL || !old) return SUPERDB_ERR;
    klen = strlen(key);
    sdb_lock(db);
    e = live_locked(db, key, klen, sdb_hash_len(key, klen));
    if (e && e->type != SDB_T_STRING) { sdb_unlock(db); return wrongtype(db); }
    if (e) {
        char *p = (char*)malloc(e->vlen + 1);
        if (!p) { sdb_unlock(db); return SUPERDB_ERR; }
        if (e->vlen) memcpy(p, e->val, e->vlen);
        p[e->vlen] = '\0';
        *old = p;
        if (old_len) *old_len = e->vlen;
    }
    if (write_locked(db, SDB_OP_SET, 0, key, klen, val, vlen) != 0) {
        free(*old);
        *old = NULL;
        if (old_len) *old_len = 0;
        rc = SUPERDB_ERR;
    }
    sdb_maybe_compact_locked(db);
    sdb_unlock(db);
    return rc;
}

int superdb_msetnx(superdb_t *db, size_t n, const char *const *keys,
                   const void *const *vals, const size_t *vlens) {
    size_t i;
    int started, rc = 1, exists = 0;
    if (!db || n == 0 || !keys || !vals || !vlens) return SUPERDB_ERR;
    for (i = 0; i < n; i++) {
        if (!key_ok(keys[i]) || (vlens[i] && !vals[i]) || vlens[i] > SDB_MAX_VAL) return SUPERDB_ERR;
    }
    sdb_lock(db);
    started = sdb_tx_begin_locked(db);
    if (started < 0) { sdb_unlock(db); return SUPERDB_ERR; }
    for (i = 0; i < n && !exists; i++) {
        size_t kl = strlen(keys[i]);
        if (live_locked(db, keys[i], kl, sdb_hash_len(keys[i], kl))) exists = 1;
    }
    if (exists) {
        sdb_tx_end_locked(db, started);
        sdb_unlock(db);
        return 0;
    }
    for (i = 0; i < n; i++) {
        if (write_locked(db, SDB_OP_SET, 0, keys[i], strlen(keys[i]), vals[i], vlens[i]) != 0) {
            rc = SUPERDB_ERR;
            break;
        }
    }
    sdb_tx_end_locked(db, started);
    sdb_maybe_compact_locked(db);
    sdb_unlock(db);
    return rc;
}

int superdb_del(superdb_t *db, const char *key) {
    size_t klen;
    uint64_t h;
    sdb_entry_t *e;
    int rc = 0;
    if (!db || !key || !*key) return 0;
    klen = strlen(key);
    h = sdb_hash_len(key, klen);
    sdb_lock(db);
    e = live_locked(db, key, klen, h);
    if (e) {
        drop_locked(db, key, klen, h);
        sdb_maybe_compact_locked(db);
        rc = 1;
    }
    sdb_unlock(db);
    return rc;
}

int superdb_exists(superdb_t *db, const char *key) {
    size_t klen;
    int rc;
    if (!db || !key) return 0;
    klen = strlen(key);
    sdb_lock(db);
    rc = live_locked(db, key, klen, sdb_hash_len(key, klen)) != NULL;
    sdb_unlock(db);
    return rc;
}

int superdb_type(superdb_t *db, const char *key) {
    size_t klen;
    sdb_entry_t *e;
    int t = SUPERDB_T_NONE;
    if (!db || !key) return t;
    klen = strlen(key);
    sdb_lock(db);
    e = live_locked(db, key, klen, sdb_hash_len(key, klen));
    if (e) t = e->type;
    sdb_unlock(db);
    return t;
}

int superdb_rename(superdb_t *db, const char *src, const char *dst) {
    size_t sl, dl;
    uint64_t hs, hd;
    sdb_entry_t *e, *d, *x = NULL;
    sdb_buf_t b = {NULL, 0, 0};
    int started, rc = 1;
    if (!db || !key_ok(src) || !key_ok(dst)) return SUPERDB_ERR;
    sl = strlen(src); dl = strlen(dst);
    hs = sdb_hash_len(src, sl);
    hd = sdb_hash_len(dst, dl);

    sdb_lock(db);
    started = sdb_tx_begin_locked(db);
    if (started < 0) { sdb_unlock(db); return SUPERDB_ERR; }
    e = live_locked(db, src, sl, hs);
    if (!e) { sdb_tx_end_locked(db, started); sdb_unlock(db); return 0; }
    if (sl == dl && memcmp(src, dst, sl) == 0) { sdb_tx_end_locked(db, started); sdb_unlock(db); return 1; }

    /* Build the destination records before touching anything. */
    if (sdb_emit_entry(&b, dst, dl, e) != 0) {
        sdb_buf_free(&b);
        sdb_tx_end_locked(db, started);
        sdb_unlock(db);
        return SUPERDB_ERR;
    }
    d = live_locked(db, dst, dl, hd);
    if (d) drop_locked(db, dst, dl, hd);
    if (sdb_append_raw_locked(db, b.p, b.n) != 0) rc = SUPERDB_ERR;
    sdb_buf_free(&b);

    if (rc == 1) {
        /* Move the entry object to its new name instead of copying it. */
        sdb_remove_h_locked(db, src, sl, hs, &x);
        sdb_append_locked(db, SDB_OP_DEL, 0, src, sl, NULL, 0);
        db->dead_bytes += sdb_entry_bytes(x);
        {
            char *nk = (char*)malloc(dl + 1);
            if (!nk) {
                /* out of memory: put it back under its old name */
                sdb_link_locked(db, x);
                rc = SUPERDB_ERR;
            } else {
                memcpy(nk, dst, dl + 1);
                free(x->key);
                x->key = nk;
                x->klen = dl;
                x->hash = hd;
                sdb_link_locked(db, x);
            }
        }
    }
    sdb_tx_end_locked(db, started);
    sdb_maybe_compact_locked(db);
    sdb_unlock(db);
    return rc;
}

/* ---------------- counters & expiry ---------------- */

int64_t superdb_incr(superdb_t *db, const char *key, int64_t by, int *ok) {
    size_t klen;
    sdb_entry_t *e;
    int64_t cur = 0, expire_at = 0;
    char numbuf[32];
    char *nv = NULL;
    int n;
    if (ok) *ok = 0;
    if (!db || !key || !*key) return 0;
    klen = strlen(key);
    if (klen > SDB_MAX_KEY) { sdb_set_err(db, "key too large"); return 0; }

    sdb_lock(db);
    e = live_locked(db, key, klen, sdb_hash_len(key, klen));
    if (e) {
        char tmp[64];
        char *end = NULL;
        if (e->type != SDB_T_STRING) { sdb_unlock(db); wrongtype(db); return 0; }
        if (e->vlen >= sizeof(tmp)) {
            sdb_set_err(db, "value is not an integer");
            sdb_unlock(db);
            return 0;
        }
        if (e->vlen) memcpy(tmp, e->val, e->vlen);
        tmp[e->vlen] = '\0';
        errno = 0;
        cur = strtoll(tmp, &end, 10);
        if (errno != 0 || end == tmp || *end != '\0') {
            sdb_set_err(db, "value is not an integer");
            sdb_unlock(db);
            return 0;
        }
        expire_at = e->expire_at; /* preserve TTL */
    }
    cur += by;
    n = snprintf(numbuf, sizeof numbuf, "%" PRId64, cur);
    nv = (char*)malloc(n > 0 ? (size_t)n : 1);
    if (!nv) { sdb_set_err(db, "out of memory"); sdb_unlock(db); return 0; }
    if (n > 0) memcpy(nv, numbuf, (size_t)n);
    if (write_locked(db, SDB_OP_SET, expire_at, key, klen, nv,
                     (size_t)(n > 0 ? n : 0)) != 0) {
        free(nv);
        sdb_unlock(db);
        return 0;
    }
    free(nv);
    sdb_maybe_compact_locked(db);
    sdb_unlock(db);
    if (ok) *ok = 1;
    return cur;
}

int superdb_expire(superdb_t *db, const char *key, int64_t ttl_ms) {
    sdb_entry_t *e;
    size_t klen;
    uint64_t h;
    int rc = 0;
    if (!db || !key || ttl_ms <= 0) return 0;
    klen = strlen(key);
    h = sdb_hash_len(key, klen);
    sdb_lock(db);
    e = live_locked(db, key, klen, h);
    if (e) {
        int64_t at = sdb_now_ms() + ttl_ms;
        if (write_locked(db, SDB_OP_EXPIRE, at, key, klen, NULL, 0) == 0) rc = 1;
        sdb_maybe_compact_locked(db);
    }
    sdb_unlock(db);
    return rc;
}

int superdb_expireat(superdb_t *db, const char *key, int64_t unix_ms) {
    sdb_entry_t *e;
    size_t klen;
    uint64_t h;
    int rc = 0;
    if (!db || !key || !*key) return 0;
    klen = strlen(key);
    h = sdb_hash_len(key, klen);
    sdb_lock(db);
    e = live_locked(db, key, klen, h);
    if (e) {
        if (unix_ms <= sdb_now_ms()) {
            drop_locked(db, key, klen, h);
        } else {
            write_locked(db, SDB_OP_EXPIRE, unix_ms, key, klen, NULL, 0);
        }
        rc = 1;
        sdb_maybe_compact_locked(db);
    }
    sdb_unlock(db);
    return rc;
}

int superdb_persist(superdb_t *db, const char *key) {
    sdb_entry_t *e;
    size_t klen;
    uint64_t h;
    int rc = 0;
    if (!db || !key || !*key) return 0;
    klen = strlen(key);
    h = sdb_hash_len(key, klen);
    sdb_lock(db);
    e = live_locked(db, key, klen, h);
    if (e && e->expire_at) {
        if (write_locked(db, SDB_OP_EXPIRE, 0, key, klen, NULL, 0) == 0) rc = 1;
        sdb_maybe_compact_locked(db);
    }
    sdb_unlock(db);
    return rc;
}

int64_t superdb_ttl(superdb_t *db, const char *key) {
    sdb_entry_t *e;
    size_t klen;
    uint64_t h;
    int64_t rc;
    if (!db || !key) return -2;
    klen = strlen(key);
    h = sdb_hash_len(key, klen);
    sdb_lock(db);
    e = live_locked(db, key, klen, h);
    if (!e) rc = -2;
    else if (!e->expire_at) rc = -1;
    else rc = e->expire_at - sdb_now_ms();
    sdb_unlock(db);
    return rc;
}

/* ---------------- key listing & scan ---------------- */

superdb_keys_t superdb_keys(superdb_t *db, const char *prefix) {
    superdb_keys_t out = {0, 0};
    size_t plen = 0, i;
    int64_t now = 0;
    if (!db) return out;
    if (prefix) plen = strlen(prefix);
    sdb_lock(db);
    now = sdb_now_ms();
    for (i = 0; i < db->nbuckets; i++) {
        sdb_entry_t *e;
        for (e = db->buckets[i]; e; e = e->next) {
            if (plen && (e->klen < plen || memcmp(e->key, prefix, plen) != 0)) continue;
            if (e->expire_at && now >= e->expire_at) continue;
            if (keys_push(&out, e->key, e->klen) != 0) {
                sdb_unlock(db);
                superdb_free_keys(&out);
                return out;
            }
        }
    }
    sdb_unlock(db);
    return out;
}

void superdb_free_keys(superdb_keys_t *k) {
    size_t i;
    if (!k) return;
    for (i = 0; i < k->count; i++) free(k->keys[i]);
    free(k->keys);
    k->keys = NULL; k->count = 0;
}

/* Bit-reverse, used by the scan cursor (same trick as Redis' dictScan). */
static uint64_t rev64(uint64_t v) {
    uint64_t r = 0;
    int i;
    for (i = 0; i < 64; i++) { r = (r << 1) | (v & 1); v >>= 1; }
    return r;
}

static uint64_t scan_next(uint64_t v, uint64_t mask) {
    v |= ~mask;
    v = rev64(v);
    v++;
    return rev64(v);
}

uint64_t superdb_scan(superdb_t *db, uint64_t cursor, const char *pattern,
                      size_t count, superdb_keys_t *out) {
    size_t plen = 0, visited = 0, maxv = count ? count : 10;
    uint64_t mask;
    int64_t now;
    int oom = 0;
    if (!db || !out) return 0;
    if (pattern) plen = strlen(pattern);
    sdb_lock(db);
    now = sdb_now_ms();
    mask = (uint64_t)db->nbuckets - 1;
    do {
        sdb_entry_t *e;
        for (e = db->buckets[(size_t)(cursor & mask)]; e && !oom; e = e->next) {
            if (e->expire_at && now >= e->expire_at) continue;
            if (pattern && !sdb_glob_match(pattern, plen, e->key, e->klen)) continue;
            if (keys_push(out, e->key, e->klen) != 0) oom = 1;
        }
        cursor = scan_next(cursor, mask);
        visited++;
    } while (cursor != 0 && visited < maxv && !oom);
    sdb_unlock(db);
    /* UINT64_MAX is never a valid cursor: it signals out-of-memory. */
    return oom ? UINT64_MAX : cursor;
}

/* ---------------- lists ---------------- */

static int64_t push_common(superdb_t *db, const char *key, unsigned op,
                           const void *val, size_t vlen) {
    size_t klen;
    sdb_entry_t *e;
    int64_t n = SUPERDB_ERR;
    if (!db || !key_ok(key) || (vlen && !val) || vlen > SDB_MAX_VAL) return SUPERDB_ERR;
    klen = strlen(key);
    sdb_lock(db);
    e = live_locked(db, key, klen, sdb_hash_len(key, klen));
    if (e && e->type != SDB_T_LIST) { sdb_unlock(db); return wrongtype(db); }
    if (write_locked(db, op, e ? e->expire_at : 0, key, klen, val, vlen) == 0) {
        e = sdb_lookup_h_locked(db, key, klen, sdb_hash_len(key, klen));
        n = e ? (int64_t)((sdb_list_t*)e->coll)->len : 0;
    }
    sdb_maybe_compact_locked(db);
    sdb_unlock(db);
    return n;
}

int64_t superdb_lpush(superdb_t *db, const char *key, const void *val, size_t vlen) {
    return push_common(db, key, SDB_OP_LPUSH, val, vlen);
}

int64_t superdb_rpush(superdb_t *db, const char *key, const void *val, size_t vlen) {
    return push_common(db, key, SDB_OP_RPUSH, val, vlen);
}

static int pop_common(superdb_t *db, const char *key, int front, void **out, size_t *out_len) {
    size_t klen;
    sdb_entry_t *e;
    sdb_list_t *l;
    const sdb_blob_t *b = NULL;
    char *copy = NULL;
    size_t bn = 0;
    int rc = 0;
    if (out) *out = NULL;
    if (out_len) *out_len = 0;
    if (!db || !key_ok(key)) return SUPERDB_ERR;
    klen = strlen(key);
    sdb_lock(db);
    e = live_locked(db, key, klen, sdb_hash_len(key, klen));
    if (!e) { sdb_unlock(db); return 0; }
    if (e->type != SDB_T_LIST) { sdb_unlock(db); return wrongtype(db); }
    l = (sdb_list_t*)e->coll;
    if (out) {
        b = sdb_list_at(l, front ? 0 : l->len - 1);
        bn = b->n;
        copy = (char*)dup_bytes(b->p, b->n);
        if (!copy) { sdb_unlock(db); return SUPERDB_ERR; }
    }
    if (write_locked(db, front ? SDB_OP_LPOP : SDB_OP_RPOP, e->expire_at,
                     key, klen, NULL, 0) != 0) {
        free(copy);
        sdb_unlock(db);
        return SUPERDB_ERR;
    }
    if (out) {
        *out = copy;
        if (out_len) *out_len = bn;
    }
    rc = 1;
    sdb_maybe_compact_locked(db);
    sdb_unlock(db);
    return rc;
}

int superdb_lpop(superdb_t *db, const char *key, void **out, size_t *out_len) {
    return pop_common(db, key, 1, out, out_len);
}

int superdb_rpop(superdb_t *db, const char *key, void **out, size_t *out_len) {
    return pop_common(db, key, 0, out, out_len);
}

int64_t superdb_llen(superdb_t *db, const char *key) {
    sdb_entry_t *e;
    size_t klen;
    int64_t n = 0;
    if (!db || !key_ok(key)) return SUPERDB_ERR;
    klen = strlen(key);
    sdb_lock(db);
    e = live_locked(db, key, klen, sdb_hash_len(key, klen));
    if (e && e->type != SDB_T_LIST) { sdb_unlock(db); return wrongtype(db); }
    if (e) n = (int64_t)((sdb_list_t*)e->coll)->len;
    sdb_unlock(db);
    return n;
}

int superdb_lrange(superdb_t *db, const char *key, int64_t start, int64_t stop,
                   superdb_items_t *out) {
    sdb_entry_t *e;
    size_t klen, i;
    int64_t len, n, s, t;
    sdb_list_t *l;
    if (!out) return SUPERDB_ERR;
    out->items = NULL; out->lens = NULL; out->count = 0;
    if (!db || !key_ok(key)) return SUPERDB_ERR;
    klen = strlen(key);
    sdb_lock(db);
    e = live_locked(db, key, klen, sdb_hash_len(key, klen));
    if (!e) { sdb_unlock(db); return 0; }
    if (e->type != SDB_T_LIST) { sdb_unlock(db); return wrongtype(db); }
    l = (sdb_list_t*)e->coll;
    len = (int64_t)l->len;
    s = start < 0 ? len + start : start;
    t = stop < 0 ? len + stop : stop;
    if (s < 0) s = 0;
    if (t >= len) t = len - 1;
    if (s > t || s >= len) { sdb_unlock(db); return 0; }
    n = t - s + 1;
    out->items = (void**)calloc((size_t)n, sizeof(void*));
    out->lens = (size_t*)calloc((size_t)n, sizeof(size_t));
    if (!out->items || !out->lens) {
        free(out->items); free(out->lens);
        out->items = NULL; out->lens = NULL;
        sdb_unlock(db);
        return SUPERDB_ERR;
    }
    for (i = 0; i < (size_t)n; i++) {
        const sdb_blob_t *b = sdb_list_at(l, (size_t)s + i);
        out->items[i] = dup_bytes(b->p, b->n);
        out->lens[i] = b->n;
        out->count++;
        if (!out->items[i]) break;
    }
    sdb_unlock(db);
    return 0;
}

/* ---------------- hashes ---------------- */

int superdb_hset(superdb_t *db, const char *key, const void *field, size_t flen,
                 const void *val, size_t vlen) {
    size_t klen;
    sdb_entry_t *e;
    sdb_fent_t *f;
    sdb_buf_t b = {NULL, 0, 0};
    int rc = SUPERDB_ERR;
    if (!db || !key_ok(key) || (flen && !field) || (vlen && !val) ||
        flen > SDB_MAX_KEY || flen + vlen + 4 > SDB_MAX_VAL) return SUPERDB_ERR;
    klen = strlen(key);
    sdb_lock(db);
    e = live_locked(db, key, klen, sdb_hash_len(key, klen));
    if (e && e->type != SDB_T_HASH) { sdb_unlock(db); return wrongtype(db); }
    f = e ? sdb_ht_find((sdb_htab_t*)e->coll, field, flen) : NULL;
    if (sdb_pack_hset(&b, field, flen, val, vlen) == 0 &&
        write_locked(db, SDB_OP_HSET, e ? e->expire_at : 0, key, klen, b.p, b.n) == 0)
        rc = f ? 0 : 1;
    sdb_buf_free(&b);
    sdb_maybe_compact_locked(db);
    sdb_unlock(db);
    return rc;
}

int superdb_hget(superdb_t *db, const char *key, const void *field, size_t flen,
                 void **out, size_t *out_len) {
    sdb_entry_t *e;
    sdb_fent_t *f;
    size_t klen;
    char *copy;
    int rc = 0;
    if (out) *out = NULL;
    if (out_len) *out_len = 0;
    if (!db || !key_ok(key) || !out || (flen && !field)) return SUPERDB_ERR;
    klen = strlen(key);
    sdb_lock(db);
    e = live_locked(db, key, klen, sdb_hash_len(key, klen));
    if (!e) { db->misses++; sdb_unlock(db); return 0; }
    if (e->type != SDB_T_HASH) { sdb_unlock(db); return wrongtype(db); }
    f = sdb_ht_find((sdb_htab_t*)e->coll, field, flen);
    if (!f) { db->misses++; sdb_unlock(db); return 0; }
    db->hits++;
    copy = (char*)dup_bytes(f->v, f->vl);
    if (!copy) rc = SUPERDB_ERR;
    else {
        *out = copy;
        if (out_len) *out_len = f->vl;
        rc = 1;
    }
    sdb_unlock(db);
    return rc;
}

int superdb_hdel(superdb_t *db, const char *key, const void *field, size_t flen) {
    sdb_entry_t *e;
    sdb_fent_t *f;
    size_t klen;
    int rc = 0;
    if (!db || !key_ok(key) || (flen && !field)) return SUPERDB_ERR;
    klen = strlen(key);
    sdb_lock(db);
    e = live_locked(db, key, klen, sdb_hash_len(key, klen));
    if (!e) { sdb_unlock(db); return 0; }
    if (e->type != SDB_T_HASH) { sdb_unlock(db); return wrongtype(db); }
    f = sdb_ht_find((sdb_htab_t*)e->coll, field, flen);
    if (f) {
        if (write_locked(db, SDB_OP_HDEL, e->expire_at, key, klen, field, flen) == 0) rc = 1;
        else rc = SUPERDB_ERR;
        sdb_maybe_compact_locked(db);
    }
    sdb_unlock(db);
    return rc;
}

int64_t superdb_hlen(superdb_t *db, const char *key) {
    sdb_entry_t *e;
    size_t klen;
    int64_t n = 0;
    if (!db || !key_ok(key)) return SUPERDB_ERR;
    klen = strlen(key);
    sdb_lock(db);
    e = live_locked(db, key, klen, sdb_hash_len(key, klen));
    if (e && e->type != SDB_T_HASH) { sdb_unlock(db); return wrongtype(db); }
    if (e) n = (int64_t)((sdb_htab_t*)e->coll)->count;
    sdb_unlock(db);
    return n;
}

int superdb_hgetall(superdb_t *db, const char *key, superdb_items_t *out) {
    sdb_entry_t *e;
    sdb_htab_t *t;
    size_t klen, i, k = 0, total;
    if (!out) return SUPERDB_ERR;
    out->items = NULL; out->lens = NULL; out->count = 0;
    if (!db || !key_ok(key)) return SUPERDB_ERR;
    klen = strlen(key);
    sdb_lock(db);
    e = live_locked(db, key, klen, sdb_hash_len(key, klen));
    if (!e) { sdb_unlock(db); return 0; }
    if (e->type != SDB_T_HASH) { sdb_unlock(db); return wrongtype(db); }
    t = (sdb_htab_t*)e->coll;
    total = t->count * 2;
    if (!total) { sdb_unlock(db); return 0; }
    out->items = (void**)calloc(total, sizeof(void*));
    out->lens = (size_t*)calloc(total, sizeof(size_t));
    if (!out->items || !out->lens) {
        free(out->items); free(out->lens);
        out->items = NULL; out->lens = NULL;
        sdb_unlock(db);
        return SUPERDB_ERR;
    }
    out->count = total;
    for (i = 0; i < t->nb; i++) {
        sdb_fent_t *x;
        for (x = t->b[i]; x; x = x->next) {
            out->items[k] = dup_bytes(x->f, x->fl);
            out->lens[k++] = x->fl;
            out->items[k] = dup_bytes(x->v, x->vl);
            out->lens[k++] = x->vl;
        }
    }
    sdb_unlock(db);
    return 0;
}

/* ---------------- sets ---------------- */

int superdb_sadd(superdb_t *db, const char *key, const void *member, size_t mlen) {
    sdb_entry_t *e;
    size_t klen;
    int rc = 0;
    if (!db || !key_ok(key) || (mlen && !member) || mlen > SDB_MAX_KEY) return SUPERDB_ERR;
    klen = strlen(key);
    sdb_lock(db);
    e = live_locked(db, key, klen, sdb_hash_len(key, klen));
    if (e && e->type != SDB_T_SET) { sdb_unlock(db); return wrongtype(db); }
    if (!e || !sdb_ht_find((sdb_htab_t*)e->coll, member, mlen)) {
        if (write_locked(db, SDB_OP_SADD, e ? e->expire_at : 0, key, klen, member, mlen) == 0)
            rc = 1;
        else
            rc = SUPERDB_ERR;
        sdb_maybe_compact_locked(db);
    }
    sdb_unlock(db);
    return rc;
}

int superdb_srem(superdb_t *db, const char *key, const void *member, size_t mlen) {
    sdb_entry_t *e;
    size_t klen;
    int rc = 0;
    if (!db || !key_ok(key) || (mlen && !member)) return SUPERDB_ERR;
    klen = strlen(key);
    sdb_lock(db);
    e = live_locked(db, key, klen, sdb_hash_len(key, klen));
    if (e && e->type != SDB_T_SET) { sdb_unlock(db); return wrongtype(db); }
    if (e && sdb_ht_find((sdb_htab_t*)e->coll, member, mlen)) {
        if (write_locked(db, SDB_OP_SREM, e->expire_at, key, klen, member, mlen) == 0)
            rc = 1;
        else
            rc = SUPERDB_ERR;
        sdb_maybe_compact_locked(db);
    }
    sdb_unlock(db);
    return rc;
}

int superdb_sismember(superdb_t *db, const char *key, const void *member, size_t mlen) {
    sdb_entry_t *e;
    size_t klen;
    int rc = 0;
    if (!db || !key_ok(key) || (mlen && !member)) return SUPERDB_ERR;
    klen = strlen(key);
    sdb_lock(db);
    e = live_locked(db, key, klen, sdb_hash_len(key, klen));
    if (!e) { db->misses++; sdb_unlock(db); return 0; }
    if (e->type != SDB_T_SET) { sdb_unlock(db); return wrongtype(db); }
    if (sdb_ht_find((sdb_htab_t*)e->coll, member, mlen)) { db->hits++; rc = 1; }
    else db->misses++;
    sdb_unlock(db);
    return rc;
}

int64_t superdb_scard(superdb_t *db, const char *key) {
    sdb_entry_t *e;
    size_t klen;
    int64_t n = 0;
    if (!db || !key_ok(key)) return SUPERDB_ERR;
    klen = strlen(key);
    sdb_lock(db);
    e = live_locked(db, key, klen, sdb_hash_len(key, klen));
    if (e && e->type != SDB_T_SET) { sdb_unlock(db); return wrongtype(db); }
    if (e) n = (int64_t)((sdb_htab_t*)e->coll)->count;
    sdb_unlock(db);
    return n;
}

int superdb_smembers(superdb_t *db, const char *key, superdb_items_t *out) {
    sdb_entry_t *e;
    sdb_htab_t *t;
    size_t klen, i, k = 0;
    if (!out) return SUPERDB_ERR;
    out->items = NULL; out->lens = NULL; out->count = 0;
    if (!db || !key_ok(key)) return SUPERDB_ERR;
    klen = strlen(key);
    sdb_lock(db);
    e = live_locked(db, key, klen, sdb_hash_len(key, klen));
    if (!e) { sdb_unlock(db); return 0; }
    if (e->type != SDB_T_SET) { sdb_unlock(db); return wrongtype(db); }
    t = (sdb_htab_t*)e->coll;
    if (!t->count) { sdb_unlock(db); return 0; }
    out->items = (void**)calloc(t->count, sizeof(void*));
    out->lens = (size_t*)calloc(t->count, sizeof(size_t));
    if (!out->items || !out->lens) {
        free(out->items); free(out->lens);
        out->items = NULL; out->lens = NULL;
        sdb_unlock(db);
        return SUPERDB_ERR;
    }
    out->count = t->count;
    for (i = 0; i < t->nb; i++) {
        sdb_fent_t *x;
        for (x = t->b[i]; x; x = x->next) {
            out->items[k] = dup_bytes(x->f, x->fl);
            out->lens[k++] = x->fl;
        }
    }
    sdb_unlock(db);
    return 0;
}

/* ---------------- counts & maintenance ---------------- */

size_t superdb_count(superdb_t *db) {
    size_t c = 0;
    if (!db) return 0;
    sdb_lock(db);
    c = db->count;
    sdb_unlock(db);
    return c;
}

size_t superdb_log_size(superdb_t *db) {
    size_t s = 0;
    if (!db) return 0;
    sdb_lock(db);
    s = db->log_size;
    sdb_unlock(db);
    return s;
}

void superdb_get_stats(superdb_t *db, superdb_stats_t *out) {
    if (!out) return;
    memset(out, 0, sizeof *out);
    if (!db) return;
    sdb_lock(db);
    out->keys = db->count;
    out->log_bytes = db->log_size;
    out->dead_bytes = db->dead_bytes;
    out->hits = db->hits;
    out->misses = db->misses;
    out->expired_keys = db->expired_keys;
    out->compactions = db->compactions;
    out->compacting = db->compacting;
    sdb_unlock(db);
}

int superdb_compact(superdb_t *db) {
    int r;
    if (!db) return -1;
    sdb_lock_idle(db);
    r = sdb_compact_sync_locked(db);
    sdb_unlock(db);
    return r == 0 ? 0 : -1;
}

int superdb_compact_async(superdb_t *db) {
    int r;
    if (!db) return -1;
    sdb_lock(db);
    r = sdb_compact_async_locked(db);
    sdb_unlock(db);
    return r;
}

static int flushall_locked(superdb_t *db) {
    FILE *f;
    free_all_entries(db);
    if (db->log) { fclose(db->log); db->log = NULL; }
    f = fopen(db->path, "wb");
    if (!f) { sdb_set_err(db, "flushall failed"); return -1; }
    sdb_buf_log(f);
    fwrite(SDB_MAGIC, 1, SDB_MAGIC_LEN, f);
    sdb_fsync(f);
    fclose(f);
    db->log = fopen(db->path, "a+b");
    if (!db->log) { sdb_set_err(db, "flushall reopen failed"); return -1; }
    sdb_buf_log(db->log);
    db->log_size = SDB_MAGIC_LEN;
    db->replay_off = SDB_MAGIC_LEN;
    db->dead_bytes = 0;
    db->pending_sync = 0;
    db->gen = sdb_rand64(&db->rng);
    return 0;
}

int superdb_flushall(superdb_t *db) {
    int r;
    if (!db) return -1;
    sdb_lock_idle(db);
    if (db->in_txn) { sdb_set_err(db, "flushall inside a transaction"); sdb_unlock(db); return -1; }
    r = flushall_locked(db);
    sdb_unlock(db);
    return r;
}

int superdb_backup(superdb_t *db, const char *dst_path) {
    sdb_buf_t b = {NULL, 0, 0};
    char tmp[1100];
    FILE *w;
    int rc = 0;
    if (!db || !dst_path || !*dst_path || strlen(dst_path) + 8 > sizeof tmp) return -1;
    sdb_lock_idle(db);
    if (db->in_txn) { sdb_unlock(db); return -1; }
    sdb_sweep_expired_locked(db);
    if (sdb_snapshot_locked(db, &b) != 0) { sdb_unlock(db); sdb_buf_free(&b); return -1; }
    sdb_unlock(db);

    snprintf(tmp, sizeof tmp, "%s.tmp", dst_path);
    w = fopen(tmp, "wb");
    if (!w) rc = -1;
    else {
        if (b.n && fwrite(b.p, 1, b.n, w) != b.n) rc = -1;
        sdb_fsync(w);
        fclose(w);
    }
    if (rc == 0 && sdb_replace(tmp, dst_path) != 0) rc = -1;
    if (rc != 0) remove(tmp);
    sdb_buf_free(&b);
    return rc;
}

/* ---------------- group commit ---------------- */

void superdb_batch_begin(superdb_t *db) {
    if (!db) return;
    sdb_lock(db);
    db->batch_depth++;
    sdb_unlock(db);
}

int superdb_batch_end(superdb_t *db) {
    if (!db) return -1;
    sdb_lock(db);
    if (db->batch_depth > 0) db->batch_depth--;
    if (db->batch_depth == 0 && db->pending_sync) sdb_flush_pending_locked(db);
    sdb_unlock(db);
    return 0;
}

int superdb_sync(superdb_t *db) {
    if (!db) return -1;
    sdb_lock(db);
    if (db->log) sdb_fsync(db->log);
    db->pending_sync = 0;
    sdb_unlock(db);
    return 0;
}

/* ---------------- transactions ---------------- */

int superdb_txn_begin(superdb_t *db) {
    int r;
    if (!db) return SUPERDB_ERR;
    sdb_lock(db);
    r = sdb_tx_begin_locked(db);
    if (r != 1) sdb_set_err(db, "transaction already open");
    sdb_unlock(db);
    return r == 1 ? 0 : SUPERDB_ERR;
}

int superdb_txn_commit(superdb_t *db) {
    int rc = 0;
    if (!db) return SUPERDB_ERR;
    sdb_lock(db);
    if (!db->in_txn) { sdb_set_err(db, "no transaction open"); rc = SUPERDB_ERR; }
    else sdb_tx_end_locked(db, 1);
    sdb_unlock(db);
    return rc;
}

/* ---------------- replication primitives ---------------- */

uint64_t superdb_log_gen(superdb_t *db) {
    uint64_t g;
    if (!db) return 0;
    sdb_lock(db);
    g = db->gen;
    sdb_unlock(db);
    return g;
}

int superdb_log_read(superdb_t *db, uint64_t off, size_t maxlen,
                     void **out, size_t *out_len, uint64_t *total, uint64_t *gen) {
    FILE *r;
    size_t n;
    void *buf;
    if (!db || !out || !out_len || !total || !gen) return SUPERDB_ERR;
    *out = NULL; *out_len = 0;
    sdb_lock(db);
    if (db->log) fflush(db->log);
    *total = db->log_size;
    *gen = db->gen;
    if (off > db->log_size) off = db->log_size;
    n = (size_t)(db->log_size - off);
    if (n > maxlen) n = maxlen;
    buf = malloc(n ? n : 1);
    r = buf ? fopen(db->path, "rb") : NULL;
    if (!r) { free(buf); sdb_unlock(db); return SUPERDB_ERR; }
    if (fseek(r, (long)off, SEEK_SET) != 0 || (n && fread(buf, 1, n, r) != n)) {
        fclose(r); free(buf); sdb_unlock(db); return SUPERDB_ERR;
    }
    fclose(r);
    sdb_unlock(db);
    *out = buf;
    *out_len = n;
    return 0;
}

void superdb_set_replica(superdb_t *db, int on) {
    if (!db) return;
    sdb_lock(db);
    db->replica = on ? 1 : 0;
    sdb_unlock(db);
}

int superdb_replica_reset(superdb_t *db, uint64_t gen) {
    int r;
    if (!db) return SUPERDB_ERR;
    r = superdb_flushall(db);
    if (r != 0) return SUPERDB_ERR;
    sdb_lock(db);
    db->gen = gen;
    sdb_unlock(db);
    return 0;
}

int superdb_replica_feed(superdb_t *db, const void *data, size_t n) {
    FILE *r;
    size_t good = 0;
    int rc = 0;
    if (!db || (n && !data)) return SUPERDB_ERR;
    sdb_lock(db);
    if (!db->replica) { sdb_unlock(db); return SUPERDB_ERR; }
    if (sdb_append_raw_locked(db, data, n) != 0) { sdb_unlock(db); return SUPERDB_ERR; }
    sdb_flush_pending_locked(db);
    r = fopen(db->path, "rb");
    if (!r) { sdb_unlock(db); return SUPERDB_ERR; }
    if (sdb_replay_locked(db, r, db->replay_off, &good, 0) != 0) rc = SUPERDB_ERR;
    fclose(r);
    if (rc == 0) {
        db->replay_off = good;
        sdb_sweep_expired_locked(db);
    }
    sdb_unlock(db);
    return rc;
}
