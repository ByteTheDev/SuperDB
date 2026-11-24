/* sdb_store — in-memory hash index (separate chaining, cached hash+len).
 * All functions assume the db lock is held (hence _locked suffix).
 * Private to SuperDB core. */
#ifndef SDB_STORE_H
#define SDB_STORE_H

#include "sdb_int.h"

void         sdb_rehash(superdb_t *db, size_t want);
sdb_entry_t *sdb_lookup_h_locked(superdb_t *db, const char *key,
                                 size_t klen, uint64_t h);
int          sdb_remove_h_locked(superdb_t *db, const char *key, size_t klen,
                                 uint64_t h, sdb_entry_t **old);
void         sdb_free_entry(sdb_entry_t *e);

#endif
