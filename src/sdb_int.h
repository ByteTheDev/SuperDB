/* sdb_int — private SuperDB internals shared by core modules.
 * NOT part of the public API (see include/superdb.h). */
#ifndef SDB_INT_H
#define SDB_INT_H

#include "superdb.h"
#include "sdb_util.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#ifdef _WIN32
#  ifndef WIN32_LEAN_AND_MEAN
#  define WIN32_LEAN_AND_MEAN
#  endif
#  include <windows.h>
#else
#  include <pthread.h>
#endif

typedef struct sdb_entry {
    char *key;
    char *val;          /* malloc'd, may be NULL if vlen==0 */
    size_t vlen;
    size_t klen;        /* cached strlen(key) */
    uint64_t hash;      /* cached FNV-1a(key) */
    int64_t expire_at;  /* 0 = none */
    struct sdb_entry *next;
} sdb_entry_t;

struct superdb {
    char path[1024];
    FILE *log;
    sdb_entry_t **buckets;
    size_t nbuckets;
    size_t count;
    size_t log_size;
    size_t dead_bytes;
    size_t compact_threshold;
    int sync_writes;
    int batch_depth;    /* >0: defer fsync until batch_end */
    int pending_sync;   /* buffered writes needing fsync */
#ifdef _WIN32
    CRITICAL_SECTION lock;
#else
    pthread_mutex_t lock;
#endif
    char errmsg[256];
};

static inline void sdb_lock(superdb_t *db) {
#ifdef _WIN32
    EnterCriticalSection(&db->lock);
#else
    pthread_mutex_lock(&db->lock);
#endif
}

static inline void sdb_unlock(superdb_t *db) {
#ifdef _WIN32
    LeaveCriticalSection(&db->lock);
#else
    pthread_mutex_unlock(&db->lock);
#endif
}

void sdb_set_err(superdb_t *db, const char *m);

#endif
