#ifndef SUPERDB_H
#define SUPERDB_H

/* SuperDB — super ultra lightweight local-first key-value database.
 * Single file log + in-memory hash index. Zero dependencies, C99.
 * Version 1.0.0
 */

#include <stddef.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

#define SUPERDB_VERSION "1.1.0"

/* Opaque database handle */
typedef struct superdb superdb_t;

/* Open options. Pass NULL for sane defaults. */
typedef struct {
    int     sync_writes;        /* fsync every write (1=crash-safe, 0=fast). Default 1 */
    size_t  compact_threshold;  /* dead bytes before auto-compact. Default 4MB, 0=disable */
    size_t  initial_buckets;    /* hash buckets, power of two. Default 1024 */
} superdb_options_t;

/* A key listing returned by superdb_keys(). Free with superdb_free_keys(). */
typedef struct {
    char  **keys;   /* malloc'd array of malloc'd strings */
    size_t  count;
} superdb_keys_t;

/* ---- lifecycle ---- */
superdb_t *superdb_open(const char *path, const superdb_options_t *opts);
void       superdb_close(superdb_t *db);
const char *superdb_err(const superdb_t *db);

/* ---- core ops (keys are NUL-terminated C strings, max 65535 bytes;
 *        values are binary-safe byte buffers) ---- */
int superdb_set(superdb_t *db, const char *key,
                const void *val, size_t vlen);
int superdb_set_str(superdb_t *db, const char *key, const char *val);
int superdb_set_ex(superdb_t *db, const char *key,
                    const void *val, size_t vlen, int64_t ttl_ms);
int superdb_set_ex_str(superdb_t *db, const char *key,
                        const char *val, int64_t ttl_ms);

/* Returns malloc'd buffer (*out_len = length). NULL = not found.
 * Caller frees with superdb_free(). */
void *superdb_get(superdb_t *db, const char *key, size_t *out_len);
/* Convenience: returns malloc'd NUL-terminated string, NULL if missing. */
char *superdb_get_str(superdb_t *db, const char *key);

int superdb_del(superdb_t *db, const char *key);      /* 1=deleted 0=missing */
int superdb_exists(superdb_t *db, const char *key);  /* 1=yes 0=no */

/* ---- extras ---- */
int64_t superdb_incr(superdb_t *db, const char *key, int64_t by, int *ok);
int superdb_expire(superdb_t *db, const char *key, int64_t ttl_ms); /*1=set 0=missing*/
int64_t superdb_ttl(superdb_t *db, const char *key); /* ms left, -1=no ttl, -2=missing */

superdb_keys_t superdb_keys(superdb_t *db, const char *prefix); /* prefix=NULL/"" = all */
void           superdb_free_keys(superdb_keys_t *k);

size_t superdb_count(superdb_t *db);
size_t superdb_log_size(superdb_t *db);

int superdb_compact(superdb_t *db);   /* rewrite log with live keys only */
int superdb_flushall(superdb_t *db);  /* delete everything */

/* Group commit: defer fsync until batch_end (one fsync per batch).
 * Crash-safe per batch boundary; same log format. May nest. */
void superdb_batch_begin(superdb_t *db);
int  superdb_batch_end(superdb_t *db);  /* 0=ok, fsyncs once if needed */
int  superdb_sync(superdb_t *db);       /* flush + fsync now */

void superdb_free(void *p);

#ifdef __cplusplus
}
#endif

#endif
