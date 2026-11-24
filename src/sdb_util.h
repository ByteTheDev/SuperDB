/* sdb_util — small shared helpers: time, RNG, byte buffers, codecs, fsync.
 * Portable C99, no dependencies. Part of SuperDB core (private). */
#ifndef SDB_UTIL_H
#define SDB_UTIL_H

#include <stddef.h>
#include <stdint.h>
#include <stdio.h>

#define SDB_LOG_BUF (1u << 20)  /* stdio buffer for the append-only log */

/* Growable byte buffer (used for snapshots, record batches, packing). */
typedef struct { unsigned char *p; size_t n, cap; } sdb_buf_t;
int  sdb_buf_put(sdb_buf_t *b, const void *p, size_t n);  /* 0 = ok, -1 = oom */
void sdb_buf_free(sdb_buf_t *b);

int64_t  sdb_now_ms(void);
int64_t  sdb_now_us(void);                 /* monotonic, for timing */
uint64_t sdb_rand64(uint64_t *state);      /* splitmix64; state is caller-owned */

void     sdb_put32le(unsigned char *b, uint32_t v);
void     sdb_put64le(unsigned char *b, uint64_t v);
uint32_t sdb_get32le(const unsigned char *b);
uint64_t sdb_get64le(const unsigned char *b);

uint64_t sdb_hash_len(const char *s, size_t n);  /* FNV-1a 64 */
size_t   sdb_pow2(size_t n);

void sdb_fsync(FILE *f);              /* fflush + OS fsync */
void sdb_buf_log(FILE *f);            /* 1MB fully-buffered stdio */
int  sdb_truncate(FILE *f, long len); /* shrink open file, 0 = ok */
int  sdb_replace(const char *tmp, const char *dst); /* rename-over, 0 = ok */

#endif
