/* superdb_log — append-only log: record encode/decode, replay, compaction.
 * On-disk format (little-endian, stable since 1.0):
 *   header: "SDB1" (4 bytes)
 *   record: u32 crc32(rest) | u8 op | u64 expire_at_ms | u32 klen | u32 vlen
 *           | key[klen] | val[vlen]
 *   op: 1 = SET, 2 = DEL (vlen == 0)
 * Private to SuperDB core. */
#ifndef SUPERDB_LOG_H
#define SUPERDB_LOG_H

#include "superdb_int.h"

#include <stdio.h>

#define SDB_MAGIC     "SDB1"
#define SDB_MAGIC_LEN 4
#define SDB_OP_SET    1
#define SDB_OP_DEL    2
#define SDB_MAX_KEY   65535
#define SDB_MAX_VAL   (64u*1024u*1024u)

/* crc + op + expire + klen + vlen */
#define SDB_REC_HDR_LEN 21
/* records this size or smaller go out in a single fwrite */
#define SDB_STACK_REC   512

/* Write one record. Byte-identical to the 1.0 format. 0 = ok. */
int sdb_write_rec(FILE *w, unsigned op, int64_t expire_at,
                  const char *key, uint32_t klen,
                  const void *val, uint32_t vlen);

/* Append + durability (fsync unless batching / sync=0) + log_size update. */
int sdb_append_locked(superdb_t *db, unsigned op, int64_t expire_at,
                      const char *key, uint32_t klen,
                      const void *val, uint32_t vlen);

/* Rewrite the log with live keys only. Lock must be held. */
int  sdb_compact_locked(superdb_t *db);
void sdb_maybe_compact_locked(superdb_t *db);

#endif
