/* superdb — public API: open/close, CRUD, expiry, counters, batching.
 * Wires together the core modules:
 *   sdb_util  time/codecs/fsync      sdb_crc   record checksums
 *   sdb_store in-memory hash index   sdb_log   append-only log + compaction
 * Portable C99, no dependencies. Thread-safe via the db mutex.
 */
#include "superdb.h"
#include "sdb_int.h"
#include "sdb_store.h"
#include "sdb_log.h"
#include "sdb_crc.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <stdint.h>
#include <inttypes.h>
#include <errno.h>

#ifdef _WIN32
#  ifndef WIN32_LEAN_AND_MEAN
#  define WIN32_LEAN_AND_MEAN
#  endif
#  include <windows.h>
#  include <io.h>
#else
#  include <pthread.h>
#  include <unistd.h>
#endif

superdb_t *superdb_open(const char *path, const superdb_options_t *opts) {
    superdb_t *db;
    FILE *f;
    long good_off;
    unsigned char magic[SDB_MAGIC_LEN];

    if (!path || !*path) return NULL;
    sdb_crc_init();
    db = (superdb_t*)calloc(1, sizeof(*db));
    if (!db) return NULL;
    strncpy(db->path, path, sizeof(db->path)-1);
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
#ifdef _WIN32
    InitializeCriticalSection(&db->lock);
#else
    pthread_mutex_init(&db->lock, NULL);
#endif

    /* open or create */
    f = fopen(path, "r+b");
    if (!f) {
        f = fopen(path, "w+b");
        if (!f) {
            sdb_set_err(db, "cannot create db file");
            goto fail;
        }
        sdb_buf_log(f);
        if (fwrite(SDB_MAGIC, 1, SDB_MAGIC_LEN, f) != SDB_MAGIC_LEN) {
            sdb_set_err(db, "cannot write header");
            fclose(f); goto fail;
        }
        fflush(f); sdb_fsync(f);
    }

    /* validate + replay */
    fseek(f, 0, SEEK_END);
    db->log_size = (size_t)ftell(f);
    fseek(f, 0, SEEK_SET);
    if (db->log_size < SDB_MAGIC_LEN) {
        /* empty/corrupt file: reinit */
        fclose(f);
        f = fopen(path, "w+b");
        if (!f) goto fail;
        sdb_buf_log(f);
        fwrite(SDB_MAGIC, 1, SDB_MAGIC_LEN, f);
        fflush(f); sdb_fsync(f);
        db->log_size = SDB_MAGIC_LEN;
    } else {
        if (fread(magic, 1, SDB_MAGIC_LEN, f) != SDB_MAGIC_LEN ||
            memcmp(magic, SDB_MAGIC, SDB_MAGIC_LEN) != 0) {
            sdb_set_err(db, "bad magic: not a superdb file");
            fclose(f); goto fail;
        }
        good_off = (long)SDB_MAGIC_LEN;
        for (;;) {
            unsigned char hdr[SDB_REC_HDR_LEN];
            unsigned char op;
            uint64_t expire_u;
            uint32_t klen, vlen, crc_want, crc_got;
            char *kbuf = NULL, *vbuf = NULL;
            size_t bodylen;
            unsigned char *body = NULL;
            long rec_start = ftell(f);
            size_t got = fread(hdr, 1, SDB_REC_HDR_LEN, f);
            if (got == 0 && feof(f)) break;          /* clean EOF */
            if (got != SDB_REC_HDR_LEN) break;       /* torn header -> truncate */
            crc_want = sdb_get32le(hdr);
            op = hdr[4];
            expire_u = sdb_get64le(hdr + 5);
            klen = sdb_get32le(hdr + 13);
            vlen = sdb_get32le(hdr + 17);
            if (op != SDB_OP_SET && op != SDB_OP_DEL) break;
            if (klen == 0 || klen > SDB_MAX_KEY || vlen > SDB_MAX_VAL) break;
            if (op == SDB_OP_DEL && vlen != 0) break;
            bodylen = 1 + 8 + 4 + 4 + klen + vlen;
            body = (unsigned char*)malloc(bodylen);
            if (!body) { sdb_set_err(db, "out of memory"); fclose(f); goto fail; }
            body[0] = op;
            memcpy(body + 1, hdr + 5, 8 + 4 + 4);
            if (klen && fread(body + 17, 1, klen, f) != klen) { free(body); break; }
            if (vlen && fread(body + 17 + klen, 1, vlen, f) != vlen) { free(body); break; }
            crc_got = sdb_crc32(body, bodylen);
            free(body); body = NULL;
            if (crc_got != crc_want) break;          /* torn write -> truncate */
            /* apply */
            kbuf = (char*)malloc(klen + 1);
            if (!kbuf) { fclose(f); goto fail; }
            fseek(f, rec_start + (long)SDB_REC_HDR_LEN, SEEK_SET);
            if (klen && fread(kbuf, 1, klen, f) != klen) { free(kbuf); break; }
            kbuf[klen] = '\0';
            if (vlen) {
                vbuf = (char*)malloc(vlen ? vlen : 1);
                if (!vbuf) { free(kbuf); fclose(f); goto fail; }
                if (fread(vbuf, 1, vlen, f) != vlen) { free(kbuf); free(vbuf); break; }
            }
            if (op == SDB_OP_SET) {
                size_t kl = strlen(kbuf);
                uint64_t h = sdb_hash_len(kbuf, kl);
                sdb_entry_t *e = sdb_lookup_h_locked(db, kbuf, kl, h);
                int64_t now = 0;
                /* skip already-expired records at load */
                if ((int64_t)expire_u != 0) {
                    now = sdb_now_ms();
                    if (now >= (int64_t)expire_u) {
                        /* expired: ensure absent */
                        if (e) { sdb_entry_t *o=NULL; sdb_remove_h_locked(db,kbuf,kl,h,&o);
                                 if(o) sdb_free_entry(o); }
                        free(kbuf); free(vbuf);
                        good_off = ftell(f);
                        continue;
                    }
                }
                if (e) {
                    free(e->val);
                    e->val = vbuf; e->vlen = vlen;
                    e->expire_at = (int64_t)expire_u;
                    free(kbuf);
                } else {
                    e = (sdb_entry_t*)calloc(1, sizeof(*e));
                    if (!e) { free(kbuf); free(vbuf); fclose(f); goto fail; }
                    e->key = kbuf; e->val = vbuf; e->vlen = vlen;
                    e->klen = kl; e->hash = h;
                    e->expire_at = (int64_t)expire_u;
                    {
                        size_t b = (size_t)(h & (uint64_t)(db->nbuckets - 1));
                        e->next = db->buckets[b]; db->buckets[b] = e;
                        db->count++;
                        if (db->count > db->nbuckets * 3 / 4)
                            sdb_rehash(db, db->nbuckets * 2);
                    }
                }
            } else {
                size_t kl = strlen(kbuf);
                uint64_t h = sdb_hash_len(kbuf, kl);
                sdb_entry_t *o = NULL;
                if (sdb_remove_h_locked(db, kbuf, kl, h, &o) && o) sdb_free_entry(o);
                free(kbuf);
            }
            good_off = ftell(f);
        }
        /* truncate torn tail if any */
        {
            long endpos;
            fseek(f, 0, SEEK_END);
            endpos = ftell(f);
            if (endpos > good_off) {
                sdb_truncate(f, good_off);
                db->log_size = (size_t)good_off;
            }
        }
        fclose(f);
        f = NULL;
    }

    /* reopen in append mode */
    db->log = fopen(db->path, "a+b");
    if (!db->log) { sdb_set_err(db, "cannot reopen db file"); goto fail; }
    sdb_buf_log(db->log);
    fseek(db->log, 0, SEEK_END);
    db->log_size = (size_t)ftell(db->log);
    db->dead_bytes = 0;
    return db;

fail:
    if (db) {
        free(db->buckets);
#ifdef _WIN32
        DeleteCriticalSection(&db->lock);
#else
        pthread_mutex_destroy(&db->lock);
#endif
        free(db);
    }
    return NULL;
}

void superdb_close(superdb_t *db) {
    size_t i;
    if (!db) return;
    sdb_lock(db);
    if (db->log) { fflush(db->log); if (db->sync_writes) sdb_fsync(db->log); fclose(db->log); db->log = NULL; }
    for (i = 0; i < db->nbuckets; i++) {
        sdb_entry_t *e = db->buckets[i];
        while (e) { sdb_entry_t *n = e->next; sdb_free_entry(e); e = n; }
    }
    free(db->buckets);
    sdb_unlock(db);
#ifdef _WIN32
    DeleteCriticalSection(&db->lock);
#else
    pthread_mutex_destroy(&db->lock);
#endif
    free(db);
}

const char *superdb_err(const superdb_t *db) {
    return db ? db->errmsg : "null db";
}

void superdb_free(void *p) { free(p); }

int superdb_set(superdb_t *db, const char *key, const void *val, size_t vlen) {
    return superdb_set_ex(db, key, val, vlen, 0);
}

int superdb_set_str(superdb_t *db, const char *key, const char *val) {
    return superdb_set_ex(db, key, val, val ? strlen(val) : 0, 0);
}

int superdb_set_ex(superdb_t *db, const char *key,
                    const void *val, size_t vlen, int64_t ttl_ms) {
    size_t klen;
    uint64_t h;
    int64_t expire_at = 0;
    sdb_entry_t *e;
    char *kcopy = NULL, *vcopy = NULL;

    if (!db || !key || !*key) { sdb_set_err(db, "bad key"); return -1; }
    klen = strlen(key);
    if (klen > SDB_MAX_KEY || vlen > SDB_MAX_VAL) { sdb_set_err(db, "key/value too large"); return -1; }
    if (vlen && !val) { sdb_set_err(db, "null value"); return -1; }
    if (ttl_ms > 0) expire_at = sdb_now_ms() + ttl_ms;
    else if (ttl_ms < 0) { sdb_set_err(db, "bad ttl"); return -1; }
    h = sdb_hash_len(key, klen);

    kcopy = (char*)malloc(klen + 1);
    if (!kcopy) return -1;
    memcpy(kcopy, key, klen + 1);
    if (vlen) {
        vcopy = (char*)malloc(vlen);
        if (!vcopy) { free(kcopy); return -1; }
        memcpy(vcopy, val, vlen);
    }

    sdb_lock(db);
    if (sdb_append_locked(db, SDB_OP_SET, expire_at, key,
                          (uint32_t)klen, val, (uint32_t)vlen) != 0) {
        sdb_unlock(db);
        free(kcopy); free(vcopy);
        return -1;
    }
    e = sdb_lookup_h_locked(db, key, klen, h);
    if (e) {
        db->dead_bytes += e->vlen + 32;
        free(e->val);
        e->val = vcopy; e->vlen = vlen; e->expire_at = expire_at;
        free(kcopy);
    } else {
        e = (sdb_entry_t*)calloc(1, sizeof(*e));
        if (!e) { sdb_unlock(db); free(kcopy); free(vcopy); return -1; }
        e->key = kcopy; e->val = vcopy; e->vlen = vlen;
        e->klen = klen; e->hash = h;
        e->expire_at = expire_at;
        {
            size_t b = (size_t)(h & (uint64_t)(db->nbuckets - 1));
            e->next = db->buckets[b]; db->buckets[b] = e;
            db->count++;
            if (db->count > db->nbuckets * 3 / 4)
                sdb_rehash(db, db->nbuckets * 2);
        }
    }
    sdb_maybe_compact_locked(db);
    sdb_unlock(db);
    return 0;
}

int superdb_set_ex_str(superdb_t *db, const char *key, const char *val, int64_t ttl_ms) {
    return superdb_set_ex(db, key, val, val ? strlen(val) : 0, ttl_ms);
}

void *superdb_get(superdb_t *db, const char *key, size_t *out_len) {
    sdb_entry_t *e;
    void *out = NULL;
    size_t klen;
    uint64_t h;
    if (!db || !key) return NULL;
    klen = strlen(key);
    h = sdb_hash_len(key, klen);
    sdb_lock(db);
    e = sdb_lookup_h_locked(db, key, klen, h);
    if (!e) { sdb_unlock(db); return NULL; }
    if (e->expire_at) {
        if (sdb_now_ms() >= e->expire_at) {
            /* lazy expire */
            sdb_entry_t *o = NULL;
            sdb_remove_h_locked(db, key, klen, h, &o);
            if (o) {
                db->dead_bytes += o->vlen + 32;
                sdb_free_entry(o);
            }
            sdb_append_locked(db, SDB_OP_DEL, 0, key, (uint32_t)klen, NULL, 0);
            sdb_unlock(db);
            return NULL;
        }
    }
    out = malloc(e->vlen + 1);
    if (out) {
        if (e->vlen) memcpy(out, e->val, e->vlen);
        ((char*)out)[e->vlen] = '\0';
        if (out_len) *out_len = e->vlen;
    }
    sdb_unlock(db);
    return out;
}

char *superdb_get_str(superdb_t *db, const char *key) {
    size_t n = 0;
    return (char*)superdb_get(db, key, &n);
}

int superdb_del(superdb_t *db, const char *key) {
    sdb_entry_t *o = NULL;
    size_t klen;
    uint64_t h;
    if (!db || !key || !*key) return 0;
    klen = strlen(key);
    h = sdb_hash_len(key, klen);
    sdb_lock(db);
    /* check expiry first: treat expired as missing */
    {
        sdb_entry_t *e = sdb_lookup_h_locked(db, key, klen, h);
        if (e && e->expire_at && sdb_now_ms() >= e->expire_at) {
            sdb_remove_h_locked(db, key, klen, h, &o);
            if (o) { db->dead_bytes += o->vlen + 32; sdb_free_entry(o); o = NULL; }
            sdb_append_locked(db, SDB_OP_DEL, 0, key, (uint32_t)klen, NULL, 0);
            sdb_unlock(db);
            return 0;
        }
    }
    if (!sdb_remove_h_locked(db, key, klen, h, &o)) { sdb_unlock(db); return 0; }
    db->dead_bytes += o->vlen + 32;
    sdb_append_locked(db, SDB_OP_DEL, 0, key, (uint32_t)klen, NULL, 0);
    sdb_free_entry(o);
    sdb_maybe_compact_locked(db);
    sdb_unlock(db);
    return 1;
}

int superdb_exists(superdb_t *db, const char *key) {
    size_t klen;
    uint64_t h;
    if (!db || !key) return 0;
    klen = strlen(key);
    h = sdb_hash_len(key, klen);
    sdb_lock(db);
    {
        sdb_entry_t *e = sdb_lookup_h_locked(db, key, klen, h);
        if (!e) { sdb_unlock(db); return 0; }
        if (e->expire_at && sdb_now_ms() >= e->expire_at) {
            sdb_entry_t *o = NULL;
            sdb_remove_h_locked(db, key, klen, h, &o);
            if (o) { db->dead_bytes += o->vlen + 32; sdb_free_entry(o); }
            sdb_append_locked(db, SDB_OP_DEL, 0, key, (uint32_t)klen, NULL, 0);
            sdb_unlock(db);
            return 0;
        }
    }
    sdb_unlock(db);
    return 1;
}

int64_t superdb_incr(superdb_t *db, const char *key, int64_t by, int *ok) {
    size_t klen;
    uint64_t h;
    sdb_entry_t *e;
    int64_t cur = 0, expire_at = 0;
    char numbuf[32];
    char *nv = NULL;
    int n;
    if (ok) *ok = 0;
    if (!db || !key || !*key) return 0;
    klen = strlen(key);
    if (klen > SDB_MAX_KEY) { sdb_set_err(db, "key too large"); return 0; }
    h = sdb_hash_len(key, klen);

    /* single lock from lookup through update: no relock, no tmp malloc */
    sdb_lock(db);
    e = sdb_lookup_h_locked(db, key, klen, h);
    if (e) {
        if (e->expire_at && sdb_now_ms() >= e->expire_at) {
            sdb_entry_t *o = NULL;
            sdb_remove_h_locked(db, key, klen, h, &o);
            if (o) { db->dead_bytes += o->vlen + 32; sdb_free_entry(o); }
            sdb_append_locked(db, SDB_OP_DEL, 0, key, (uint32_t)klen, NULL, 0);
            e = NULL;
        } else {
            char tmp[64];
            char *end = NULL;
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
    }
    cur += by;
    n = snprintf(numbuf, sizeof numbuf, "%" PRId64, cur);
    nv = (char*)malloc(n > 0 ? (size_t)n : 1);
    if (!nv) { sdb_set_err(db, "out of memory"); sdb_unlock(db); return 0; }
    if (n > 0) memcpy(nv, numbuf, (size_t)n);
    if (sdb_append_locked(db, SDB_OP_SET, expire_at, key,
                          (uint32_t)klen, nv, (uint32_t)(n > 0 ? n : 0)) != 0) {
        free(nv);
        sdb_unlock(db);
        return 0;
    }
    if (e) {
        db->dead_bytes += e->vlen + 32;
        free(e->val);
        e->val = nv; e->vlen = (size_t)(n > 0 ? n : 0);
    } else {
        char *kcopy = (char*)malloc(klen + 1);
        sdb_entry_t *ne;
        if (!kcopy) { free(nv); sdb_unlock(db); return 0; }
        memcpy(kcopy, key, klen + 1);
        ne = (sdb_entry_t*)calloc(1, sizeof(*ne));
        if (!ne) { free(kcopy); free(nv); sdb_unlock(db); return 0; }
        ne->key = kcopy; ne->val = nv; ne->vlen = (size_t)(n > 0 ? n : 0);
        ne->klen = klen; ne->hash = h; ne->expire_at = expire_at;
        {
            size_t b = (size_t)(h & (uint64_t)(db->nbuckets - 1));
            ne->next = db->buckets[b]; db->buckets[b] = ne;
            db->count++;
            if (db->count > db->nbuckets * 3 / 4)
                sdb_rehash(db, db->nbuckets * 2);
        }
    }
    sdb_maybe_compact_locked(db);
    sdb_unlock(db);
    if (ok) *ok = 1;
    return cur;
}

int superdb_expire(superdb_t *db, const char *key, int64_t ttl_ms) {
    sdb_entry_t *e;
    int64_t expire_at;
    size_t klen;
    uint64_t h;
    if (!db || !key || ttl_ms <= 0) return 0;
    klen = strlen(key);
    h = sdb_hash_len(key, klen);
    expire_at = sdb_now_ms() + ttl_ms;
    sdb_lock(db);
    e = sdb_lookup_h_locked(db, key, klen, h);
    if (!e) { sdb_unlock(db); return 0; }
    if (e->expire_at && sdb_now_ms() >= e->expire_at) {
        sdb_entry_t *o = NULL;
        sdb_remove_h_locked(db, key, klen, h, &o);
        if (o) sdb_free_entry(o);
        sdb_append_locked(db, SDB_OP_DEL, 0, key, (uint32_t)klen, NULL, 0);
        sdb_unlock(db);
        return 0;
    }
    e->expire_at = expire_at;
    /* persist new expiry by rewriting value */
    sdb_append_locked(db, SDB_OP_SET, expire_at, key,
                      (uint32_t)klen, e->val, (uint32_t)e->vlen);
    db->dead_bytes += 32 + e->vlen;
    sdb_maybe_compact_locked(db);
    sdb_unlock(db);
    return 1;
}

int64_t superdb_ttl(superdb_t *db, const char *key) {
    sdb_entry_t *e;
    int64_t left;
    size_t klen;
    uint64_t h;
    if (!db || !key) return -2;
    klen = strlen(key);
    h = sdb_hash_len(key, klen);
    sdb_lock(db);
    e = sdb_lookup_h_locked(db, key, klen, h);
    if (!e) { sdb_unlock(db); return -2; }
    if (!e->expire_at) { sdb_unlock(db); return -1; }
    left = e->expire_at - sdb_now_ms();
    if (left <= 0) {
        sdb_entry_t *o = NULL;
        sdb_remove_h_locked(db, key, klen, h, &o);
        if (o) { db->dead_bytes += o->vlen + 32; sdb_free_entry(o); }
        sdb_append_locked(db, SDB_OP_DEL, 0, key, (uint32_t)klen, NULL, 0);
        sdb_unlock(db);
        return -2;
    }
    sdb_unlock(db);
    return left;
}

superdb_keys_t superdb_keys(superdb_t *db, const char *prefix) {
    superdb_keys_t out = {0, 0};
    size_t cap = 0, plen = 0;
    size_t i;
    int64_t now = 0;
    int need_now = 0;
    size_t j;
    if (!db) return out;
    if (prefix) plen = strlen(prefix);
    sdb_lock(db);
    /* only pay for a clock call if some key actually has a TTL */
    for (j = 0; j < db->nbuckets && !need_now; j++) {
        sdb_entry_t *x = db->buckets[j];
        while (x) { if (x->expire_at) { need_now = 1; break; } x = x->next; }
    }
    if (need_now) now = sdb_now_ms();
    for (i = 0; i < db->nbuckets; i++) {
        sdb_entry_t *e = db->buckets[i];
        while (e) {
            if ((!plen || strncmp(e->key, prefix, plen) == 0) &&
                !(e->expire_at && now >= e->expire_at)) {
                if (out.count >= cap) {
                    size_t nc = cap ? cap * 2 : 64;
                    char **nk = (char**)realloc(out.keys, nc * sizeof(*nk));
                    if (!nk) break;
                    out.keys = nk; cap = nc;
                }
                out.keys[out.count] = (char*)malloc(e->klen + 1);
                if (out.keys[out.count]) {
                    memcpy(out.keys[out.count], e->key, e->klen + 1);
                    out.count++;
                }
            }
            e = e->next;
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
    sdb_lock(db); s = db->log_size; sdb_unlock(db);
    return s;
}

int superdb_compact(superdb_t *db) {
    int r;
    if (!db) return -1;
    sdb_lock(db);
    r = sdb_compact_locked(db);
    sdb_unlock(db);
    return r;
}

int superdb_flushall(superdb_t *db) {
    size_t i;
    FILE *f;
    if (!db) return -1;
    sdb_lock(db);
    for (i = 0; i < db->nbuckets; i++) {
        sdb_entry_t *e = db->buckets[i];
        while (e) { sdb_entry_t *n = e->next; sdb_free_entry(e); e = n; }
        db->buckets[i] = NULL;
    }
    db->count = 0;
    if (db->log) fclose(db->log);
    db->log = NULL;
    f = fopen(db->path, "w+b");
    if (!f) { sdb_set_err(db, "flushall failed"); sdb_unlock(db); return -1; }
    sdb_buf_log(f);
    fwrite(SDB_MAGIC, 1, SDB_MAGIC_LEN, f);
    fflush(f); sdb_fsync(f); fclose(f);
    db->log = fopen(db->path, "a+b");
    if (!db->log) { sdb_set_err(db, "flushall reopen failed"); sdb_unlock(db); return -1; }
    sdb_buf_log(db->log);
    db->log_size = SDB_MAGIC_LEN;
    db->dead_bytes = 0;
    db->pending_sync = 0;
    sdb_unlock(db);
    return 0;
}

/* ---------------- group commit ---------------- */

void superdb_batch_begin(superdb_t *db) {
    if (!db) return;
    sdb_lock(db);
    db->batch_depth++;
    sdb_unlock(db);
}

int superdb_batch_end(superdb_t *db) {
    int rc = 0;
    if (!db) return -1;
    sdb_lock(db);
    if (db->batch_depth > 0) db->batch_depth--;
    if (db->batch_depth == 0 && db->pending_sync) {
        if (db->log) {
            fflush(db->log);
            if (db->sync_writes) {
#ifdef _WIN32
                rc = _commit(_fileno(db->log));
#else
                rc = fsync(fileno(db->log));
#endif
            }
        }
        db->pending_sync = 0;
    }
    sdb_unlock(db);
    return rc;
}

int superdb_sync(superdb_t *db) {
    if (!db) return -1;
    sdb_lock(db);
    if (db->log) sdb_fsync(db->log);
    db->pending_sync = 0;
    sdb_unlock(db);
    return 0;
}
