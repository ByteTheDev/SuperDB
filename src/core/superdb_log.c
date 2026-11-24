/* superdb_log — implementation. See superdb_log.h. */
#include "superdb_log.h"
#include "superdb_crc.h"
#include "superdb_store.h"

#include <stdlib.h>
#include <string.h>

int sdb_write_rec(FILE *w, unsigned op, int64_t expire_at,
                  const char *key, uint32_t klen,
                  const void *val, uint32_t vlen) {
    unsigned char f[17];
    unsigned char hdr[4];
    uint32_t s;
    size_t recsz = 4 + 17 + klen + vlen;
    sdb_crc_init();
    f[0] = (unsigned char)op;
    sdb_put64le(f + 1, (uint64_t)expire_at);
    sdb_put32le(f + 9, klen);
    sdb_put32le(f + 13, vlen);
    s = 0xFFFFFFFFu;
    s = sdb_crc_up(s, f, 17);
    s = sdb_crc_up(s, (const unsigned char*)key, klen);
    if (vlen && val) s = sdb_crc_up(s, (const unsigned char*)val, vlen);
    s ^= 0xFFFFFFFFu;
    sdb_put32le(hdr, s);
    if (recsz <= SDB_STACK_REC) {
        unsigned char sb[SDB_STACK_REC];
        memcpy(sb, hdr, 4);
        memcpy(sb + 4, f, 17);
        memcpy(sb + 21, key, klen);
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

static void sdb_durability_locked(superdb_t *db) {
    if (db->batch_depth > 0) { db->pending_sync = 1; return; }
    if (db->sync_writes) sdb_fsync(db->log);
    /* else: leave buffered in the 1MB stdio buffer; visible via memtable,
     * flushed on batch_end/sync/compact/flushall/close. */
}

int sdb_append_locked(superdb_t *db, unsigned op, int64_t expire_at,
                      const char *key, uint32_t klen,
                      const void *val, uint32_t vlen) {
    if (sdb_write_rec(db->log, op, expire_at, key, klen, val, vlen) != 0) {
        sdb_set_err(db, "log write failed");
        return -1;
    }
    sdb_durability_locked(db);
    db->log_size += 4 + 17 + klen + vlen;
    return 0;
}

void sdb_maybe_compact_locked(superdb_t *db) {
    if (db->compact_threshold && db->dead_bytes >= db->compact_threshold)
        sdb_compact_locked(db);
}

int sdb_compact_locked(superdb_t *db) {
    char tmp[1152];
    FILE *w;
    size_t i;
    size_t new_size = SDB_MAGIC_LEN;
    int64_t now = sdb_now_ms();
    snprintf(tmp, sizeof(tmp), "%s.tmp", db->path);
    w = fopen(tmp, "w+b");
    if (!w) { sdb_set_err(db, "compact: cannot create tmp"); return -1; }
    sdb_buf_log(w);
    if (fwrite(SDB_MAGIC, 1, SDB_MAGIC_LEN, w) != SDB_MAGIC_LEN) {
        fclose(w); return -1;
    }
    for (i = 0; i < db->nbuckets; i++) {
        sdb_entry_t *e = db->buckets[i];
        while (e) {
            if (!(e->expire_at && now >= e->expire_at)) {
                if (sdb_write_rec(w, SDB_OP_SET, e->expire_at, e->key,
                                  (uint32_t)e->klen, e->val,
                                  (uint32_t)e->vlen) != 0) {
                    fclose(w); return -1;
                }
                new_size += 4 + 17 + e->klen + e->vlen;
            }
            e = e->next;
        }
    }
    fflush(w); sdb_fsync(w); fclose(w);
    /* sweep expired from memory */
    for (i = 0; i < db->nbuckets; i++) {
        sdb_entry_t *e = db->buckets[i], *prev = NULL;
        while (e) {
            sdb_entry_t *nx = e->next;
            if (e->expire_at && now >= e->expire_at) {
                if (prev) prev->next = nx; else db->buckets[i] = nx;
                sdb_free_entry(e);
                db->count--;
            } else prev = e;
            e = nx;
        }
    }
    if (db->log) fclose(db->log);
    db->log = NULL;
    if (sdb_replace(tmp, db->path) != 0) {
        /* try to reopen old state */
        db->log = fopen(db->path, "a+b");
        if (db->log) sdb_buf_log(db->log);
        sdb_set_err(db, "compact: replace failed");
        return -1;
    }
    db->log = fopen(db->path, "a+b");
    if (!db->log) { sdb_set_err(db, "compact: reopen failed"); return -1; }
    sdb_buf_log(db->log);
    fseek(db->log, 0, SEEK_END);
    db->log_size = new_size;
    db->dead_bytes = 0;
    db->pending_sync = 0;
    return 0;
}
