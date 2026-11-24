#ifndef SUPERDB_H
#define SUPERDB_H

/* SuperDB — super ultra lightweight local-first key-value database.
 * Single append-only log (format SDB2) + in-memory hash index.
 * Values are strings, lists, hashes or sets. Zero dependencies, C99.
 */

#include <stddef.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

#define SUPERDB_VERSION "2.0.0"

/* Opaque database handle */
typedef struct superdb superdb_t;

/* Open options. Pass NULL for sane defaults. */
typedef struct {
    int     sync_writes;        /* fsync every write (1=crash-safe, 0=fast). Default 1 */
    size_t  compact_threshold;  /* dead bytes before auto-compact. Default 4MB, 0=disable */
    size_t  initial_buckets;    /* hash buckets, power of two. Default 1024 */
} superdb_options_t;

/* A key listing returned by superdb_keys()/superdb_scan(). Free with superdb_free_keys(). */
typedef struct {
    char  **keys;   /* malloc'd array of malloc'd strings */
    size_t  count;
} superdb_keys_t;

/* Binary-safe item list (LRANGE, HGETALL, SMEMBERS). Free with superdb_free_items(). */
typedef struct {
    void  **items;  /* malloc'd array of malloc'd buffers */
    size_t *lens;   /* byte length of each item */
    size_t  count;
} superdb_items_t;

typedef enum {
    SUPERDB_T_NONE   = 0,
    SUPERDB_T_STRING = 1,
    SUPERDB_T_LIST   = 2,
    SUPERDB_T_HASH   = 3,
    SUPERDB_T_SET    = 4
} superdb_type_t;

/* Typed operations return these on failure (in addition to their own codes). */
#define SUPERDB_ERR       (-1)  /* generic failure, see superdb_err() */
#define SUPERDB_WRONGTYPE (-2)  /* key holds a different value type */

typedef struct {
    size_t   keys;          /* keys in memory (may include not-yet-swept expired keys) */
    size_t   log_bytes;     /* current log size */
    size_t   dead_bytes;    /* approximate garbage in the log */
    uint64_t hits;          /* read lookups that found a live key */
    uint64_t misses;        /* read lookups that did not */
    uint64_t expired_keys;  /* keys removed because their TTL passed */
    uint64_t compactions;   /* completed rewrites (sync + background) */
    int      compacting;    /* 1 while a background rewrite is running */
} superdb_stats_t;

/* ---- lifecycle ---- */
superdb_t *superdb_open(const char *path, const superdb_options_t *opts);
void       superdb_close(superdb_t *db);
const char *superdb_err(const superdb_t *db);

/* ---- strings ---- (keys are NUL-terminated C strings, max 65535 bytes;
 *      values are binary-safe byte buffers) */
int superdb_set(superdb_t *db, const char *key,
                const void *val, size_t vlen);
int superdb_set_str(superdb_t *db, const char *key, const char *val);
int superdb_set_ex(superdb_t *db, const char *key,
                    const void *val, size_t vlen, int64_t ttl_ms);
int superdb_set_ex_str(superdb_t *db, const char *key,
                        const char *val, int64_t ttl_ms);

/* Returns malloc'd buffer (*out_len = length). NULL = missing or wrong type.
 * Caller frees with superdb_free(). */
void *superdb_get(superdb_t *db, const char *key, size_t *out_len);
/* Convenience: returns malloc'd NUL-terminated string, NULL if missing. */
char *superdb_get_str(superdb_t *db, const char *key);
/* 1 = found (*out malloc'd), 0 = missing, SUPERDB_WRONGTYPE = not a string. */
int superdb_get_ex(superdb_t *db, const char *key, void **out, size_t *out_len);

int superdb_setnx(superdb_t *db, const char *key, const void *val, size_t vlen); /* 1=set 0=exists */
/* Sets the new value (clears TTL). *old = malloc'd previous value, or NULL if
 * the key was missing. Returns 1 ok, SUPERDB_WRONGTYPE or SUPERDB_ERR. */
int superdb_getset(superdb_t *db, const char *key, const void *val, size_t vlen,
                   void **old, size_t *old_len);
/* All-or-nothing: sets every pair only if none of the keys exist.
 * Returns 1 set, 0 some key existed (nothing written), SUPERDB_ERR. */
int superdb_msetnx(superdb_t *db, size_t n, const char *const *keys,
                   const void *const *vals, const size_t *vlens);

int superdb_del(superdb_t *db, const char *key);      /* 1=deleted 0=missing (any type) */
int superdb_exists(superdb_t *db, const char *key);  /* 1=yes 0=no */
/* superdb_type_t of the live key (SUPERDB_T_NONE if missing). */
int superdb_type(superdb_t *db, const char *key);
/* 1 = renamed, 0 = no such source key, SUPERDB_ERR. Overwrites dst (any type),
 * carries the TTL along, and is atomic in the log. */
int superdb_rename(superdb_t *db, const char *src, const char *dst);

/* ---- counters & expiry ---- */
int64_t superdb_incr(superdb_t *db, const char *key, int64_t by, int *ok);
int superdb_expire(superdb_t *db, const char *key, int64_t ttl_ms); /*1=set 0=missing*/
/* Absolute deadline in unix milliseconds. A deadline in the past deletes the key. */
int superdb_expireat(superdb_t *db, const char *key, int64_t unix_ms); /*1=set 0=missing*/
int superdb_persist(superdb_t *db, const char *key);   /* 1=TTL removed 0=none/missing */
int64_t superdb_ttl(superdb_t *db, const char *key); /* ms left, -1=no ttl, -2=missing */

/* ---- key listing ---- */
superdb_keys_t superdb_keys(superdb_t *db, const char *prefix); /* prefix=NULL/"" = all */
void           superdb_free_keys(superdb_keys_t *k);
/* Incremental scan. Start with cursor 0; pass the returned cursor back until it
 * returns 0. Every key present for the whole scan is returned at least once.
 * pattern: glob (* ? [a-z] [^a-z] \x) or NULL for all. count: buckets per call
 * (0 = default 10). Appends matches to *out (free with superdb_free_keys).
 * If memory runs out mid-scan the result is incomplete and UINT64_MAX is returned. */
uint64_t superdb_scan(superdb_t *db, uint64_t cursor, const char *pattern,
                      size_t count, superdb_keys_t *out);

/* ---- lists ---- (push returns the new length, pop returns 1 + malloc'd value) */
int64_t superdb_lpush(superdb_t *db, const char *key, const void *val, size_t vlen);
int64_t superdb_rpush(superdb_t *db, const char *key, const void *val, size_t vlen);
int     superdb_lpop(superdb_t *db, const char *key, void **out, size_t *out_len);
int     superdb_rpop(superdb_t *db, const char *key, void **out, size_t *out_len);
int64_t superdb_llen(superdb_t *db, const char *key);
/* Redis-style inclusive indices, negative counts from the end. */
int superdb_lrange(superdb_t *db, const char *key, int64_t start, int64_t stop,
                   superdb_items_t *out);

/* ---- hashes ---- (fields are binary-safe, max 65535 bytes) */
int superdb_hset(superdb_t *db, const char *key, const void *field, size_t flen,
                 const void *val, size_t vlen);     /* 1=new field 0=updated */
int superdb_hget(superdb_t *db, const char *key, const void *field, size_t flen,
                 void **out, size_t *out_len);      /* 1=found 0=missing */
int superdb_hdel(superdb_t *db, const char *key, const void *field, size_t flen);
int64_t superdb_hlen(superdb_t *db, const char *key);
/* Interleaved field, value pairs in out->items. */
int superdb_hgetall(superdb_t *db, const char *key, superdb_items_t *out);

/* ---- sets ---- */
int superdb_sadd(superdb_t *db, const char *key, const void *member, size_t mlen); /* 1=added */
int superdb_srem(superdb_t *db, const char *key, const void *member, size_t mlen);  /* 1=removed */
int superdb_sismember(superdb_t *db, const char *key, const void *member, size_t mlen);
int64_t superdb_scard(superdb_t *db, const char *key);
int superdb_smembers(superdb_t *db, const char *key, superdb_items_t *out);

void superdb_free_items(superdb_items_t *it);

/* ---- counts & maintenance ---- */
size_t superdb_count(superdb_t *db);
size_t superdb_log_size(superdb_t *db);
void   superdb_get_stats(superdb_t *db, superdb_stats_t *out);

int superdb_compact(superdb_t *db);        /* synchronous rewrite of the log (live keys only) */
/* Starts a background rewrite; writes keep flowing and are merged at the end.
 * Returns 0 started, 1 already running, SUPERDB_ERR. */
int superdb_compact_async(superdb_t *db);
int superdb_flushall(superdb_t *db);       /* delete everything */
/* Consistent snapshot of the live data written as a fresh, compacted file at dst. */
int superdb_backup(superdb_t *db, const char *dst_path);

/* ---- group commit ---- defer fsync until batch_end (one fsync per batch).
 * Crash-safe per batch boundary; same log format. May nest. */
void superdb_batch_begin(superdb_t *db);
int  superdb_batch_end(superdb_t *db);  /* 0=ok, fsyncs once if needed */
int  superdb_sync(superdb_t *db);       /* flush + fsync now */

/* ---- transactions ---- Writes between txn_begin and txn_commit are logged
 * inside a TXN_BEGIN/TXN_COMMIT pair. After a crash, replay applies the group
 * only if its commit record reached disk; otherwise the whole group is dropped.
 * Transactions are single-writer: no other thread may write while one is open.
 * There is no rollback of in-memory changes. */
int superdb_txn_begin(superdb_t *db);   /* 0=ok, SUPERDB_ERR if one is already open */
int superdb_txn_commit(superdb_t *db);  /* 0=ok, SUPERDB_ERR if none is open */

/* ---- replication primitives ---- The log is byte-identical between a primary
 * and a replica that has applied it. gen changes whenever the byte layout does
 * (compaction, flushall), telling replicas to resynchronize. */
uint64_t superdb_log_gen(superdb_t *db);
/* Copies up to maxlen bytes of the log starting at off. *total and *gen are
 * filled atomically with the data, even on error. Returns 0 ok, SUPERDB_ERR if
 * off is past the end. Free *out with superdb_free(). */
int superdb_log_read(superdb_t *db, uint64_t off, size_t maxlen,
                     void **out, size_t *out_len, uint64_t *total, uint64_t *gen);
/* Marks db as a read-only replica: its own compaction is disabled. */
void superdb_set_replica(superdb_t *db, int on);
/* Replica only: empties the db and adopts primary generation gen. */
int superdb_replica_reset(superdb_t *db, uint64_t gen);
/* Replica only: appends primary log bytes at the current end and applies the
 * committed records they complete. */
int superdb_replica_feed(superdb_t *db, const void *data, size_t n);

void superdb_free(void *p);

#ifdef __cplusplus
}
#endif

#endif
