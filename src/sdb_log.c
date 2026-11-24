/* sdb_log — implementation. See sdb_log.h. */
#include "sdb_log.h"
#include "sdb_crc.h"
#include "sdb_store.h"
#include "sdb_coll.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

/* ---------------- record encoding ---------------- */

static void rec_header(unsigned char f[17], unsigned op, int64_t expire_at,
                       size_t klen, size_t vlen) {
    f[0] = (unsigned char)op;
    sdb_put64le(f + 1, (uint64_t)expire_at);
    sdb_put32le(f + 9, (uint32_t)klen);
    sdb_put32le(f + 13, (uint32_t)vlen);
}

static uint32_t rec_crc(const unsigned char f[17], const void *key, size_t klen,
                        const void *val, size_t vlen) {
    uint32_t s = 0xFFFFFFFFu;
    s = sdb_crc_up(s, f, 17);
    if (klen) s = sdb_crc_up(s, (const unsigned char*)key, klen);
    if (vlen && val) s = sdb_crc_up(s, (const unsigned char*)val, vlen);
    return s ^ 0xFFFFFFFFu;
}

int sdb_write_rec(FILE *w, unsigned op, int64_t expire_at,
                  const char *key, uint32_t klen,
                  const void *val, uint32_t vlen) {
    unsigned char f[17];
    unsigned char hdr[4];
    size_t recsz = SDB_REC_HDR_LEN + (size_t)klen + vlen;
    sdb_crc_init();
    rec_header(f, op, expire_at, klen, vlen);
    sdb_put32le(hdr, rec_crc(f, key, klen, val, vlen));
    if (recsz <= SDB_STACK_REC) {
        unsigned char sb[SDB_STACK_REC];
        memcpy(sb, hdr, 4);
        memcpy(sb + 4, f, 17);
        if (klen) memcpy(sb + 21, key, klen);
        if (vlen && val) memcpy(sb + 21 + klen, val, vlen);
        if (fwrite(sb, 1, recsz, w) != recsz) return -1;
    } else {
        if (fwrite(hdr, 1, 4, w) != 4 ||
            fwrite(f, 1, 17, w) != 17 ||
            (klen && fwrite(key, 1, klen, w) != klen) ||
            (vlen && val && fwrite(val, 1, vlen, w) != vlen)) return -1;
    }
    return 0;
}

int sdb_encode_rec(sdb_buf_t *b, unsigned op, int64_t expire_at,
                   const char *key, size_t klen, const void *val, size_t vlen) {
    unsigned char f[17], hdr[4];
    sdb_crc_init();
    rec_header(f, op, expire_at, klen, vlen);
    sdb_put32le(hdr, rec_crc(f, key, klen, val, vlen));
    if (sdb_buf_put(b, hdr, 4) || sdb_buf_put(b, f, 17)) return -1;
    if (klen && sdb_buf_put(b, key, klen)) return -1;
    if (vlen && sdb_buf_put(b, val, vlen)) return -1;
    return 0;
}

int sdb_pack_hset(sdb_buf_t *b, const void *f, size_t fl, const void *v, size_t vl) {
    unsigned char lb[4];
    sdb_put32le(lb, (uint32_t)fl);
    if (sdb_buf_put(b, lb, 4) || sdb_buf_put(b, f, fl) || sdb_buf_put(b, v, vl)) return -1;
    return 0;
}

/* ---------------- durable appends ---------------- */

static void durability_locked(superdb_t *db) {
    if (db->batch_depth > 0) { db->pending_sync = 1; return; }
    if (db->sync_writes) sdb_fsync(db->log);
    /* else: left in the 1MB stdio buffer; flushed on batch_end/sync/compact/close. */
}

int sdb_append_locked(superdb_t *db, unsigned op, int64_t expire_at,
                      const char *key, size_t klen, const void *val, size_t vlen) {
    if (!db->log) { sdb_set_err(db, "log is not open"); return -1; }
    if (sdb_write_rec(db->log, op, expire_at, key, (uint32_t)klen,
                      val, (uint32_t)vlen) != 0) {
        sdb_set_err(db, "log write failed");
        return -1;
    }
    if (db->delta_on == 1 &&
        sdb_encode_rec(&db->delta, op, expire_at, key, klen, val, vlen) != 0)
        db->delta_on = 2;   /* overflow: the running rewrite will be abandoned */
    durability_locked(db);
    db->log_size += SDB_REC_HDR_LEN + klen + vlen;
    return 0;
}

int sdb_append_raw_locked(superdb_t *db, const void *p, size_t n) {
    if (n == 0) return 0;
    if (!db->log) { sdb_set_err(db, "log is not open"); return -1; }
    if (fwrite(p, 1, n, db->log) != n) {
        sdb_set_err(db, "log write failed");
        return -1;
    }
    if (db->delta_on == 1 && sdb_buf_put(&db->delta, p, n) != 0) db->delta_on = 2;
    durability_locked(db);
    db->log_size += n;
    return 0;
}

int sdb_flush_pending_locked(superdb_t *db) {
    if (db->log) {
        fflush(db->log);
        if (db->sync_writes) sdb_fsync(db->log);
    }
    db->pending_sync = 0;
    return 0;
}

int sdb_tx_begin_locked(superdb_t *db) {
    if (db->in_txn) return 0;
    if (sdb_append_locked(db, SDB_OP_TXN_BEGIN, 0, "", 0, NULL, 0) != 0) return -1;
    db->in_txn = 1;
    db->batch_depth++;
    return 1;
}

void sdb_tx_end_locked(superdb_t *db, int started) {
    if (!started) return;
    sdb_append_locked(db, SDB_OP_TXN_COMMIT, 0, "", 0, NULL, 0);
    db->in_txn = 0;
    if (db->batch_depth > 0) db->batch_depth--;
    if (db->batch_depth == 0 && db->pending_sync) sdb_flush_pending_locked(db);
}

/* ---------------- apply (shared by live writes and replay) ---------------- */

static void remove_key_locked(superdb_t *db, const char *key, size_t klen, uint64_t h) {
    sdb_entry_t *o = NULL;
    if (sdb_remove_h_locked(db, key, klen, h, &o) && o) {
        db->dead_bytes += sdb_entry_bytes(o);
        sdb_free_entry(o);
    }
}

/* Creates the entry if missing (linked). Returns NULL on oom. */
static sdb_entry_t *ensure_entry(superdb_t *db, unsigned char type, const char *key,
                                 size_t klen, uint64_t h, int64_t expire_at) {
    sdb_entry_t *e = sdb_lookup_h_locked(db, key, klen, h);
    if (e) return e;
    e = sdb_entry_new(type, key, klen, h, expire_at);
    if (e) sdb_link_locked(db, e);
    return e;
}

int sdb_apply_locked(superdb_t *db, unsigned op, int64_t expire_at,
                     const char *key, size_t klen, const void *val, size_t vlen) {
    uint64_t h = sdb_hash_len(key, klen);
    sdb_entry_t *e = sdb_lookup_h_locked(db, key, klen, h);

    switch (op) {
    case SDB_OP_SET: {
        char *nv = NULL;
        if (vlen) {
            nv = (char*)malloc(vlen);
            if (!nv) return -1;
            memcpy(nv, val, vlen);
        }
        if (e) {
            db->dead_bytes += sdb_entry_bytes(e);
            sdb_entry_reset_value(e);
            e->type = SDB_T_STRING;
            e->expire_at = expire_at;
        } else {
            e = sdb_entry_new(SDB_T_STRING, key, klen, h, expire_at);
            if (!e) { free(nv); return -1; }
            sdb_link_locked(db, e);
        }
        e->val = nv;
        e->vlen = vlen;
        return 0;
    }
    case SDB_OP_DEL:
        if (e) remove_key_locked(db, key, klen, h);
        return 0;
    case SDB_OP_EXPIRE:
        if (e) e->expire_at = expire_at;
        return 0;
    case SDB_OP_LPUSH:
    case SDB_OP_RPUSH:
        if (e && e->type != SDB_T_LIST) return -1;
        e = ensure_entry(db, SDB_T_LIST, key, klen, h, expire_at);
        if (!e) return -1;
        e->expire_at = expire_at;
        return sdb_list_push((sdb_list_t*)e->coll, op == SDB_OP_LPUSH, val, vlen) == 0 ? 0 : -1;
    case SDB_OP_LPOP:
    case SDB_OP_RPOP: {
        sdb_list_t *l;
        sdb_blob_t b;
        if (!e || e->type != SDB_T_LIST) return -1;
        l = (sdb_list_t*)e->coll;
        if (sdb_list_pop(l, op == SDB_OP_LPOP, &b)) {
            db->dead_bytes += 2 * (32 + klen) + b.n;
            free(b.p);
        }
        if (l->len == 0) remove_key_locked(db, key, klen, h);
        else e->expire_at = expire_at;
        return 0;
    }
    case SDB_OP_HSET: {
        uint32_t fl;
        const char *f, *v;
        size_t vl;
        if (vlen < 4) return -1;
        fl = sdb_get32le((const unsigned char*)val);
        if ((size_t)fl > vlen - 4) return -1;
        f = (const char*)val + 4;
        v = f + fl;
        vl = vlen - 4 - fl;
        if (e && e->type != SDB_T_HASH) return -1;
        e = ensure_entry(db, SDB_T_HASH, key, klen, h, expire_at);
        if (!e) return -1;
        e->expire_at = expire_at;
        return sdb_ht_put((sdb_htab_t*)e->coll, f, fl, v, vl) < 0 ? -1 : 0;
    }
    case SDB_OP_HDEL:
        if (!e) return 0;
        if (e->type != SDB_T_HASH) return -1;
        sdb_ht_del((sdb_htab_t*)e->coll, val, vlen);
        if (((sdb_htab_t*)e->coll)->count == 0) remove_key_locked(db, key, klen, h);
        else e->expire_at = expire_at;
        return 0;
    case SDB_OP_SADD:
        if (e && e->type != SDB_T_SET) return -1;
        e = ensure_entry(db, SDB_T_SET, key, klen, h, expire_at);
        if (!e) return -1;
        e->expire_at = expire_at;
        return sdb_ht_put((sdb_htab_t*)e->coll, val, vlen, NULL, 0) < 0 ? -1 : 0;
    case SDB_OP_SREM:
        if (!e) return 0;
        if (e->type != SDB_T_SET) return -1;
        sdb_ht_del((sdb_htab_t*)e->coll, val, vlen);
        if (((sdb_htab_t*)e->coll)->count == 0) remove_key_locked(db, key, klen, h);
        else e->expire_at = expire_at;
        return 0;
    default:
        return -1;
    }
}

/* ---------------- replay ---------------- */

typedef struct {
    unsigned op;
    int64_t expire;
    char *key;  size_t klen;
    char *val;  size_t vlen;
} sdb_rec_t;

static void rec_free(sdb_rec_t *r) {
    free(r->key); free(r->val);
    r->key = NULL; r->val = NULL;
}

static int op_is_txn(unsigned op) { return op == SDB_OP_TXN_BEGIN || op == SDB_OP_TXN_COMMIT; }
static int op_has_val(unsigned op) {
    return op == SDB_OP_SET || op == SDB_OP_LPUSH || op == SDB_OP_RPUSH ||
           op == SDB_OP_HSET || op == SDB_OP_HDEL || op == SDB_OP_SADD || op == SDB_OP_SREM;
}

/* Reads one record at the current position. 1 = valid record, 0 = EOF,
 * torn, or corrupt (the caller stops there). */
static int read_rec(FILE *f, sdb_rec_t *r, size_t *reclen, int legacy) {
    unsigned char hdr[SDB_REC_HDR_LEN];
    uint32_t crc_want, klen, vlen;
    r->key = NULL; r->val = NULL;
    if (fread(hdr, 1, SDB_REC_HDR_LEN, f) != SDB_REC_HDR_LEN) return 0;
    crc_want = sdb_get32le(hdr);
    r->op = hdr[4];
    r->expire = (int64_t)sdb_get64le(hdr + 5);
    klen = sdb_get32le(hdr + 13);
    vlen = sdb_get32le(hdr + 17);
    r->klen = klen; r->vlen = vlen;
    if (legacy && r->op > SDB_OP_DEL) return 0;
    if (r->op < SDB_OP_SET || r->op > SDB_OP_SREM) return 0;
    if (klen > SDB_MAX_KEY || vlen > SDB_MAX_VAL) return 0;
    if (op_is_txn(r->op)) {
        if (klen || vlen) return 0;
    } else {
        if (klen == 0) return 0;
        if (!op_has_val(r->op) && vlen) return 0;
    }
    if (klen) {
        r->key = (char*)malloc(klen + 1);
        if (!r->key) return 0;
        if (fread(r->key, 1, klen, f) != klen) { rec_free(r); return 0; }
        r->key[klen] = '\0';
        if (memchr(r->key, '\0', klen) != NULL) {   /* keys are C strings */
            rec_free(r); return 0;
        }
    }
    if (vlen) {
        r->val = (char*)malloc(vlen);
        if (!r->val) { rec_free(r); return 0; }
        if (fread(r->val, 1, vlen, f) != vlen) { rec_free(r); return 0; }
    }
    if (rec_crc(hdr + 4, r->key, klen, r->val, vlen) != crc_want) { rec_free(r); return 0; }
    *reclen = SDB_REC_HDR_LEN + (size_t)klen + vlen;
    return 1;
}

int sdb_replay_locked(superdb_t *db, FILE *f, size_t start, size_t *good_out, int legacy) {
    size_t pos = start, good = start;
    sdb_rec_t *pend = NULL;
    size_t np = 0, cap = 0, i;
    int in_txn = 0;
    sdb_crc_init();
    if (fseek(f, (long)start, SEEK_SET) != 0) return -1;
    for (;;) {
        sdb_rec_t r;
        size_t rl = 0;
        if (!read_rec(f, &r, &rl, legacy)) break;
        pos += rl;
        if (r.op == SDB_OP_TXN_BEGIN) {
            if (in_txn) break;          /* nested begin: corrupt */
            in_txn = 1;
            np = 0;
            continue;
        }
        if (r.op == SDB_OP_TXN_COMMIT) {
            if (!in_txn) break;         /* commit without begin: corrupt */
            for (i = 0; i < np; i++) {
                sdb_apply_locked(db, pend[i].op, pend[i].expire, pend[i].key,
                                 pend[i].klen, pend[i].val, pend[i].vlen);
                rec_free(&pend[i]);
            }
            np = 0;
            in_txn = 0;
            good = pos;
            continue;
        }
        if (in_txn) {
            if (np == cap) {
                size_t nc = cap ? cap * 2 : 16;
                sdb_rec_t *np2 = (sdb_rec_t*)realloc(pend, nc * sizeof *pend);
                if (!np2) { rec_free(&r); break; }
                pend = np2; cap = nc;
            }
            pend[np++] = r;             /* ownership moves to pend */
            continue;
        }
        sdb_apply_locked(db, r.op, r.expire, r.key, r.klen, r.val, r.vlen);
        rec_free(&r);
        good = pos;
    }
    /* An unfinished transaction is dropped: good stays before its begin. */
    for (i = 0; i < np; i++) rec_free(&pend[i]);
    free(pend);
    *good_out = good;
    return 0;
}

/* ---------------- snapshot & sweep ---------------- */

int sdb_emit_entry(sdb_buf_t *b, const char *key, size_t klen, const sdb_entry_t *e) {
    size_t i;
    if (e->type == SDB_T_STRING)
        return sdb_encode_rec(b, SDB_OP_SET, e->expire_at, key, klen, e->val, e->vlen);
    if (e->type == SDB_T_LIST) {
        const sdb_list_t *l = (const sdb_list_t*)e->coll;
        for (i = 0; i < l->len; i++) {
            const sdb_blob_t *x = sdb_list_at(l, i);
            if (sdb_encode_rec(b, SDB_OP_RPUSH, e->expire_at, key, klen, x->p, x->n)) return -1;
        }
        return 0;
    }
    if (e->type == SDB_T_HASH || e->type == SDB_T_SET) {
        const sdb_htab_t *t = (const sdb_htab_t*)e->coll;
        sdb_buf_t tmp = {NULL, 0, 0};
        int rc = 0;
        size_t j;
        for (j = 0; j < t->nb && rc == 0; j++) {
            const sdb_fent_t *x;
            for (x = t->b[j]; x && rc == 0; x = x->next) {
                if (e->type == SDB_T_SET) {
                    rc = sdb_encode_rec(b, SDB_OP_SADD, e->expire_at, key, klen, x->f, x->fl);
                } else {
                    tmp.n = 0;
                    rc = sdb_pack_hset(&tmp, x->f, x->fl, x->v, x->vl);
                    if (rc == 0)
                        rc = sdb_encode_rec(b, SDB_OP_HSET, e->expire_at, key, klen, tmp.p, tmp.n);
                }
            }
        }
        sdb_buf_free(&tmp);
        return rc;
    }
    return -1;
}

int sdb_snapshot_locked(superdb_t *db, sdb_buf_t *b) {
    size_t i;
    int64_t now = sdb_now_ms();
    if (sdb_buf_put(b, SDB_MAGIC, SDB_MAGIC_LEN) != 0) return -1;
    for (i = 0; i < db->nbuckets; i++) {
        sdb_entry_t *e;
        for (e = db->buckets[i]; e; e = e->next) {
            if (e->expire_at && now >= e->expire_at) continue;
            if (sdb_emit_entry(b, e->key, e->klen, e) != 0) return -1;
        }
    }
    return 0;
}

void sdb_sweep_expired_locked(superdb_t *db) {
    size_t i;
    int64_t now = sdb_now_ms();
    for (i = 0; i < db->nbuckets; i++) {
        sdb_entry_t *e = db->buckets[i], *prev = NULL;
        while (e) {
            sdb_entry_t *nx = e->next;
            if (e->expire_at && now >= e->expire_at) {
                if (prev) prev->next = nx; else db->buckets[i] = nx;
                db->count--;
                db->expired_keys++;
                db->dead_bytes += sdb_entry_bytes(e);
                sdb_free_entry(e);
            } else {
                prev = e;
            }
            e = nx;
        }
    }
}

/* ---------------- compaction ---------------- */

static int write_file(const char *path, const unsigned char *p, size_t n) {
    FILE *w = fopen(path, "wb");
    if (!w) return -1;
    sdb_buf_log(w);
    if (n && fwrite(p, 1, n, w) != n) { fclose(w); return -1; }
    sdb_fsync(w);
    fclose(w);
    return 0;
}

/* Closes the current handle (if any) and opens the file in append mode. */
static void reopen_log_locked(superdb_t *db) {
    if (db->log) { fflush(db->log); fclose(db->log); db->log = NULL; }
    db->log = fopen(db->path, "a+b");
    if (db->log) {
        sdb_buf_log(db->log);
        fseek(db->log, 0, SEEK_END);
    }
}

int sdb_compact_sync_locked(superdb_t *db) {
    sdb_buf_t b = {NULL, 0, 0};
    char tmp[1100];
    if (db->replica || db->in_txn) { sdb_set_err(db, "compact: not allowed now"); return -1; }
    snprintf(tmp, sizeof tmp, "%s.tmp", db->path);
    sdb_sweep_expired_locked(db);
    if (sdb_snapshot_locked(db, &b) != 0) {
        sdb_buf_free(&b);
        sdb_set_err(db, "compact: out of memory");
        return -1;
    }
    if (write_file(tmp, b.p, b.n) != 0) {
        remove(tmp);
        sdb_buf_free(&b);
        sdb_set_err(db, "compact: cannot write tmp");
        return -1;
    }
    if (db->log) { fclose(db->log); db->log = NULL; }
    if (sdb_replace(tmp, db->path) != 0) {
        reopen_log_locked(db);
        sdb_buf_free(&b);
        sdb_set_err(db, "compact: replace failed");
        return -1;
    }
    reopen_log_locked(db);
    if (!db->log) { sdb_buf_free(&b); sdb_set_err(db, "compact: reopen failed"); return -1; }
    db->log_size = b.n;
    db->dead_bytes = 0;
    db->pending_sync = 0;
    db->gen = sdb_rand64(&db->rng);
    db->compactions++;
    sdb_buf_free(&b);
    return 0;
}

typedef struct { superdb_t *db; sdb_buf_t snap; } bg_job_t;

static void bg_compact_main(void *arg) {
    bg_job_t *j = (bg_job_t*)arg;
    superdb_t *db = j->db;
    char tmp[1100];
    int ok;
    snprintf(tmp, sizeof tmp, "%s.tmp", db->path);
    /* The big write happens without the lock: the snapshot is immutable. */
    ok = write_file(tmp, j->snap.p, j->snap.n) == 0;

    sdb_lock(db);
    if (ok && db->delta_on != 1) ok = 0;          /* delta overflowed */
    if (ok && db->delta.n) {
        FILE *w = fopen(tmp, "ab");
        ok = w != NULL && fwrite(db->delta.p, 1, db->delta.n, w) == db->delta.n;
        if (w) { sdb_fsync(w); fclose(w); }
        if (!w) ok = 0;
    }
    if (ok) {
        if (db->log) { fclose(db->log); db->log = NULL; }
        ok = sdb_replace(tmp, db->path) == 0;
    }
    reopen_log_locked(db);
    if (ok && db->log) {
        db->log_size = j->snap.n + db->delta.n;
        db->dead_bytes = db->dead_bytes >= db->dead_at_snap ? db->dead_bytes - db->dead_at_snap : 0;
        db->pending_sync = 0;
        db->gen = sdb_rand64(&db->rng);
        db->compactions++;
    } else {
        remove(tmp);
        sdb_set_err(db, "background compaction failed; old log kept");
    }
    db->delta.n = 0;
    db->delta_on = 0;
    db->compacting = 0;
    sdb_unlock(db);
    sdb_buf_free(&j->snap);
    free(j);
}

int sdb_compact_async_locked(superdb_t *db) {
    bg_job_t *j;
    if (db->replica || db->in_txn) return -1;
    if (db->compacting) return 1;
    if (db->cthread_valid) {
        /* compacting == 0: the previous worker already released the lock. */
        sdb_thread_join(&db->cthread);
        db->cthread_valid = 0;
    }
    j = (bg_job_t*)calloc(1, sizeof *j);
    if (!j) return -1;
    sdb_sweep_expired_locked(db);
    if (sdb_snapshot_locked(db, &j->snap) != 0) {
        sdb_buf_free(&j->snap);
        free(j);
        return -1;
    }
    j->db = db;
    db->dead_at_snap = db->dead_bytes;
    db->delta.n = 0;
    db->delta_on = 1;
    db->compacting = 1;
    if (sdb_thread_start(&db->cthread, bg_compact_main, j) != 0) {
        db->compacting = 0;
        db->delta_on = 0;
        sdb_buf_free(&j->snap);
        free(j);
        return -1;
    }
    db->cthread_valid = 1;
    return 0;
}

void sdb_maybe_compact_locked(superdb_t *db) {
    if (db->replica || db->compacting || db->in_txn || !db->compact_threshold) return;
    if (db->dead_bytes >= db->compact_threshold) sdb_compact_async_locked(db);
}

void sdb_wait_compact(superdb_t *db) {
    for (;;) {
        sdb_thread_t t;
        int have = 0;
        sdb_lock(db);
        if (!db->compacting && !db->cthread_valid) { sdb_unlock(db); return; }
        if (db->cthread_valid) {
            t = db->cthread;
            db->cthread_valid = 0;
            have = 1;
        }
        sdb_unlock(db);
        if (have) sdb_thread_join(&t);
        else sdb_sleep_ms(1);
    }
}

void sdb_lock_idle(superdb_t *db) {
    for (;;) {
        sdb_wait_compact(db);
        sdb_lock(db);
        if (!db->compacting) return;
        sdb_unlock(db);
    }
}
