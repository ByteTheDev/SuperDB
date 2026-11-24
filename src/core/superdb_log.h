/* superdb_log - append-only log: record encode, replay, apply, compaction.
 * On-disk format SDB2 (little-endian). Stable layout since 2.0:
 *   header: "SDB2" (4 bytes). Legacy "SDB1" files are migrated on open.
 *   record: u32 crc32(rest) | u8 op | i64 expire_at_ms | u32 klen | u32 vlen
 *           | key[klen] | val[vlen]
 *   op 1 SET key val        | op 2 DEL key            | op 5 EXPIRE key (expire only)
 *   op 3 TXN_BEGIN          | op 4 TXN_COMMIT         (klen = vlen = 0)
 *   op 6/7 LPUSH/RPUSH val  | op 8/9 LPOP/RPOP        (no val)
 *   op 10 HSET val=[u32 flen][field][value]  | op 11 HDEL val=field
 *   op 12 SADD val=member   | op 13 SREM val=member
 * Every record carries the key's current expire_at, so replay is order-exact.
 * Records between TXN_BEGIN and TXN_COMMIT apply only when the commit is present.
 * Private to SuperDB core. */
#ifndef SUPERDB_LOG_H
#define SUPERDB_LOG_H

#include "superdb_int.h"

#include <stdio.h>

#define SDB_MAGIC       "SDB2"
#define SDB_MAGIC_V1    "SDB1"
#define SDB_MAGIC_LEN   4
#define SDB_OP_SET      1
#define SDB_OP_DEL      2
#define SDB_OP_TXN_BEGIN  3
#define SDB_OP_TXN_COMMIT 4
#define SDB_OP_EXPIRE   5
#define SDB_OP_LPUSH    6
#define SDB_OP_RPUSH    7
#define SDB_OP_LPOP    8
#define SDB_OP_RPOP    9
#define SDB_OP_HSET    10
#define SDB_OP_HDEL    11
#define SDB_OP_SADD    12
#define SDB_OP_SREM    13
#define SDB_MAX_KEY    65535
#define SDB_MAX_VAL    (64u*1024u*1024u)

/* crc + op + expire + klen + vlen */
#define SDB_REC_HDR_LEN 21
/* records this size or smaller go out in a single fwrite */
#define SDB_STACK_REC   512

/* Write one record to a stdio stream. 0 = ok. */
int sdb_write_rec(FILE *w, unsigned op, int64_t expire_at,
                  const char *key, uint32_t klen,
                  const void *val, uint32_t vlen);
/* Encode one record into a memory buffer (same bytes as sdb_write_rec). */
int sdb_encode_rec(sdb_buf_t *b, unsigned op, int64_t expire_at,
                   const char *key, size_t klen, const void *val, size_t vlen);
/* HSET payload: [u32 flen][field][value]. b must be empty on entry. */
int sdb_pack_hset(sdb_buf_t *b, const void *f, size_t fl, const void *v, size_t vl);

/* Log a mutation: write record, mirror into a background rewrite if one runs,
 * then fsync unless batching / sync=0. Updates log_size. 0 = ok. */
int sdb_append_locked(superdb_t *db, unsigned op, int64_t expire_at,
                      const char *key, size_t klen, const void *val, size_t vlen);
/* Append already-encoded records (used for multi-record ops). */
int sdb_append_raw_locked(superdb_t *db, const void *p, size_t n);
/* Flush stdio and fsync if writes were deferred by batching. */
int sdb_flush_pending_locked(superdb_t *db);

/* Transactions. tx_begin returns 1 if it opened one (caller must end it),
 * 0 if one was already open (no-op), -1 on error. */
int  sdb_tx_begin_locked(superdb_t *db);
void sdb_tx_end_locked(superdb_t *db, int started);

/* Applies one mutation to memory. Used by live writes and by replay, so both
 * produce identical state. 0 = applied, -1 = invalid for current state/oom. */
int sdb_apply_locked(superdb_t *db, unsigned op, int64_t expire_at,
                     const char *key, size_t klen, const void *val, size_t vlen);

/* Encodes all of an entry's state as records under the given key name. */
int sdb_emit_entry(sdb_buf_t *b, const char *key, size_t klen, const sdb_entry_t *e);
/* Snapshot of all unexpired entries as a complete log (magic included). */
int sdb_snapshot_locked(superdb_t *db, sdb_buf_t *b);
/* Drops expired entries from memory (no log writes). */
void sdb_sweep_expired_locked(superdb_t *db);

/* Replays records from f starting at byte offset start. Applies committed
 * records only. *good = offset after the last committed record.
 * legacy: file is SDB1 (only ops 1-2 allowed). Returns 0, or -1 on I/O error. */
int sdb_replay_locked(superdb_t *db, FILE *f, size_t start, size_t *good, int legacy);

/* Rewrite the log with live keys only. Lock must be held; no rewrite running. */
int  sdb_compact_sync_locked(superdb_t *db);
/* Starts a background rewrite. Lock held. 0 started, 1 already running, -1 error. */
int  sdb_compact_async_locked(superdb_t *db);
void sdb_maybe_compact_locked(superdb_t *db);
/* Blocks until no background rewrite runs. Call WITHOUT the lock held. */
void sdb_wait_compact(superdb_t *db);
/* Acquires the lock with no background rewrite running. Caller unlocks. */
void sdb_lock_idle(superdb_t *db);

#endif
