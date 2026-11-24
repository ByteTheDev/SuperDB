/* sdb_coll — in-memory collection types used by typed values.
 *   sdb_list_t : deque of binary blobs (ring buffer, O(1) push/pop at both ends)
 *   sdb_htab_t : chained hash table of (field, value) pairs. Hashes use both
 *                fields; sets use member as field with a NULL value.
 * Pure data structures: no db, no locks. Private to SuperDB core. */
#ifndef SDB_COLL_H
#define SDB_COLL_H

#include <stddef.h>
#include <stdint.h>

typedef struct { char *p; size_t n; } sdb_blob_t;

typedef struct {
    sdb_blob_t *a;      /* ring buffer, cap is a power of two */
    size_t cap, head, len;
    size_t bytes;       /* sum of element lengths */
} sdb_list_t;

sdb_list_t *sdb_list_new(void);
void        sdb_list_free(sdb_list_t *l);
int         sdb_list_push(sdb_list_t *l, int front, const void *p, size_t n); /* 0 ok */
/* Removes one end. Returns 1 and moves ownership of out->p to the caller. */
int         sdb_list_pop(sdb_list_t *l, int front, sdb_blob_t *out);
/* i must be < len. Borrowed pointer, valid until the next mutation. */
const sdb_blob_t *sdb_list_at(const sdb_list_t *l, size_t i);

typedef struct sdb_fent {
    char *f;  size_t fl;
    char *v;  size_t vl;     /* NULL/0 for set members */
    uint64_t h;
    struct sdb_fent *next;
} sdb_fent_t;

typedef struct {
    sdb_fent_t **b;
    size_t nb, count, bytes;  /* bytes = sum of field + value lengths */
} sdb_htab_t;

sdb_htab_t *sdb_ht_new(void);
void        sdb_ht_free(sdb_htab_t *t);
sdb_fent_t *sdb_ht_find(const sdb_htab_t *t, const void *f, size_t fl);
/* Insert or replace. Returns 1 = new entry, 0 = value replaced, -1 = oom. */
int         sdb_ht_put(sdb_htab_t *t, const void *f, size_t fl, const void *v, size_t vl);
/* 1 = removed, 0 = absent. */
int         sdb_ht_del(sdb_htab_t *t, const void *f, size_t fl);

#endif
