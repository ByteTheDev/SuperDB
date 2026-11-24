/* sdb_store — in-memory key table (separate chaining, cached hash+len).
 * Functions suffixed _locked assume the db lock is held.
 * Private to SuperDB core. */
#ifndef SDB_STORE_H
#define SDB_STORE_H

#include "sdb_int.h"

void         sdb_rehash(superdb_t *db, size_t want);
sdb_entry_t *sdb_lookup_h_locked(superdb_t *db, const char *key,
                                 size_t klen, uint64_t h);
int          sdb_remove_h_locked(superdb_t *db, const char *key, size_t klen,
                                 uint64_t h, sdb_entry_t **old);
/* Allocates an empty entry of the given type (key copied). Not linked. */
sdb_entry_t *sdb_entry_new(unsigned char type, const char *key, size_t klen,
                           uint64_t h, int64_t expire_at);
/* Links an entry (from sdb_entry_new or a moved entry) into its bucket. */
void         sdb_link_locked(superdb_t *db, sdb_entry_t *e);
/* Drops the value (string bytes or collection), keeps key and links. */
void         sdb_entry_reset_value(sdb_entry_t *e);
void         sdb_free_entry(sdb_entry_t *e);
/* Approximate bytes of live payload held by an entry (for dead accounting). */
size_t       sdb_entry_bytes(const sdb_entry_t *e);

#endif
