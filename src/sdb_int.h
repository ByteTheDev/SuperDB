/* sdb_int — private SuperDB internals shared by core modules.
 * NOT part of the public API (see include/superdb.h). */
#ifndef SDB_INT_H
#define SDB_INT_H

#include "superdb.h"
#include "sdb_util.h"
#include "sdb_thread.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

/* Value types. Numeric values match superdb_type_t. */
#define SDB_T_STRING 1
#define SDB_T_LIST   2
#define SDB_T_HASH   3
#define SDB_T_SET    4

typedef struct sdb_entry {
    char *key;
    size_t klen;        /* cached strlen(key) */
    uint64_t hash;      /* cached FNV-1a(key) */
    int64_t expire_at;  /* 0 = none */
    struct sdb_entry *next;
    unsigned char type; /* SDB_T_* */
    char *val;          /* SDB_T_STRING: malloc'd, may be NULL if vlen==0 */
    size_t vlen;
    void *coll;         /* SDB_T_LIST: sdb_list_t*, SDB_T_HASH/SET: sdb_htab_t* */
} sdb_entry_t;

struct superdb {
    char path[1024];
    FILE *log;
    sdb_entry_t **buckets;
    size_t nbuckets;
    size_t count;
    size_t log_size;        /* bytes in the log file (valid end offset) */
    size_t dead_bytes;      /* approximate garbage, drives auto-compaction */
    size_t compact_threshold;
    int sync_writes;
    int batch_depth;        /* >0: defer fsync until batch_end */
    int pending_sync;       /* buffered writes needing fsync */
    int in_txn;             /* a TXN_BEGIN has been written and not committed */
    int replica;            /* 1: read-only mirror, no own compaction */
    uint64_t gen;           /* log layout generation (see superdb_log_gen) */
    uint64_t rng;           /* splitmix state for gen ids */
    size_t replay_off;      /* replica: offset up to which records are applied */
    /* stats */
    uint64_t hits, misses, expired_keys, compactions;
    /* background compaction: delta captures writes made during a rewrite */
    int compacting;
    int cthread_valid;
    sdb_thread_t cthread;
    sdb_buf_t delta;
    int delta_on;
    size_t dead_at_snap;
    sdb_mutex_t lock;
    char errmsg[256];
};

static inline void sdb_lock(superdb_t *db)   { sdb_mutex_lock(&db->lock); }
static inline void sdb_unlock(superdb_t *db) { sdb_mutex_unlock(&db->lock); }

void sdb_set_err(superdb_t *db, const char *m);

#endif
