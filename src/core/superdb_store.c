/* superdb_store - implementation. See superdb_store.h. */
#include "superdb_store.h"
#include "superdb_coll.h"

void sdb_rehash(superdb_t *db, size_t want) {
    size_t nb = sdb_pow2(want < 16 ? 16 : want);
    sdb_entry_t **nbk;
    size_t i;
    if (nb <= db->nbuckets) return;
    nbk = (sdb_entry_t**)calloc(nb, sizeof(*nbk));
    if (!nbk) return;
    for (i = 0; i < db->nbuckets; i++) {
        sdb_entry_t *e = db->buckets[i];
        while (e) {
            sdb_entry_t *nx = e->next;
            size_t b = (size_t)(e->hash & (uint64_t)(nb - 1));
            e->next = nbk[b]; nbk[b] = e;
            e = nx;
        }
    }
    free(db->buckets);
    db->buckets = nbk;
    db->nbuckets = nb;
}

sdb_entry_t *sdb_lookup_h_locked(superdb_t *db, const char *key,
                                 size_t klen, uint64_t h) {
    sdb_entry_t *e = db->buckets[(size_t)(h & (uint64_t)(db->nbuckets - 1))];
    while (e) {
        if (e->hash == h && e->klen == klen && memcmp(e->key, key, klen) == 0)
            return e;
        e = e->next;
    }
    return NULL;
}

int sdb_remove_h_locked(superdb_t *db, const char *key, size_t klen,
                        uint64_t h, sdb_entry_t **old) {
    size_t b = (size_t)(h & (uint64_t)(db->nbuckets - 1));
    sdb_entry_t *e = db->buckets[b], *prev = NULL;
    while (e) {
        if (e->hash == h && e->klen == klen && memcmp(e->key, key, klen) == 0) {
            if (prev) prev->next = e->next; else db->buckets[b] = e->next;
            if (old) *old = e; else sdb_free_entry(e);
            db->count--;
            return 1;
        }
        prev = e; e = e->next;
    }
    return 0;
}

sdb_entry_t *sdb_entry_new(unsigned char type, const char *key, size_t klen,
                           uint64_t h, int64_t expire_at) {
    sdb_entry_t *e = (sdb_entry_t*)calloc(1, sizeof *e);
    if (!e) return NULL;
    e->key = (char*)malloc(klen + 1);
    if (!e->key) { free(e); return NULL; }
    memcpy(e->key, key, klen);
    e->key[klen] = '\0';
    e->klen = klen;
    e->hash = h;
    e->expire_at = expire_at;
    e->type = type;
    if (type == SDB_T_LIST) {
        e->coll = sdb_list_new();
    } else if (type == SDB_T_HASH || type == SDB_T_SET) {
        e->coll = sdb_ht_new();
    }
    if (type != SDB_T_STRING && !e->coll) { free(e->key); free(e); return NULL; }
    return e;
}

void sdb_link_locked(superdb_t *db, sdb_entry_t *e) {
    size_t b = (size_t)(e->hash & (uint64_t)(db->nbuckets - 1));
    e->next = db->buckets[b];
    db->buckets[b] = e;
    db->count++;
    if (db->count > db->nbuckets * 3 / 4)
        sdb_rehash(db, db->nbuckets * 2);
}

void sdb_entry_reset_value(sdb_entry_t *e) {
    free(e->val);
    e->val = NULL;
    e->vlen = 0;
    if (e->type == SDB_T_LIST) sdb_list_free((sdb_list_t*)e->coll);
    else if (e->type == SDB_T_HASH || e->type == SDB_T_SET) sdb_ht_free((sdb_htab_t*)e->coll);
    e->coll = NULL;
}

void sdb_free_entry(sdb_entry_t *e) {
    if (!e) return;
    sdb_entry_reset_value(e);
    free(e->key);
    free(e);
}

size_t sdb_entry_bytes(const sdb_entry_t *e) {
    size_t n = e->klen + 32;
    if (e->type == SDB_T_STRING) n += e->vlen;
    else if (e->type == SDB_T_LIST && e->coll) n += ((const sdb_list_t*)e->coll)->bytes;
    else if (e->coll) n += ((const sdb_htab_t*)e->coll)->bytes;
    return n;
}
