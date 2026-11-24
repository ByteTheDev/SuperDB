/* sdb_store — implementation. See sdb_store.h. */
#include "sdb_store.h"

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

void sdb_free_entry(sdb_entry_t *e) {
    if (e) { free(e->key); free(e->val); free(e); }
}
