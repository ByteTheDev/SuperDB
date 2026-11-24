/* bench_sdb — SuperDB throughput benchmark.
 * Usage: bench_sdb [--n N] [--vlen L] [--sync|--nosync] [--db PATH]
 * Workload: N SETs (key 16B, value L bytes), N GETs, N mixed 80/20.
 */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include "superdb.h"

#ifdef _WIN32
#  ifndef WIN32_LEAN_AND_MEAN
#  define WIN32_LEAN_AND_MEAN
#  endif
#  include <windows.h>
static double now_s(void) {
    LARGE_INTEGER f, c;
    QueryPerformanceFrequency(&f);
    QueryPerformanceCounter(&c);
    return (double)c.QuadPart / (double)f.QuadPart;
}
#else
#  include <sys/time.h>
static double now_s(void) {
    struct timeval tv; gettimeofday(&tv, NULL);
    return tv.tv_sec + tv.tv_usec / 1e6;
}
#endif

int main(int argc, char **argv) {
    long N = 100000;
    int vlen = 100;
    int sync = 0; /* default nosync = fast; pass --sync for durable */
    long batch = 0; /* group-commit batch size (0 = off) */
    int skip_mixed = 0;
    const char *path = "./bench_sdb.sdb";
    int i;
    char key[64], *val;
    double t0, t1;
    superdb_options_t opts;
    superdb_t *db;

    for (i = 1; i < argc; i++) {
        if (strcmp(argv[i], "--n") == 0 && i + 1 < argc) N = atol(argv[++i]);
        else if (strcmp(argv[i], "--vlen") == 0 && i + 1 < argc) vlen = atoi(argv[++i]);
        else if (strcmp(argv[i], "--sync") == 0) sync = 1;
        else if (strcmp(argv[i], "--nosync") == 0) sync = 0;
        else if (strcmp(argv[i], "--batch") == 0 && i + 1 < argc) batch = atol(argv[++i]);
        else if (strcmp(argv[i], "--skip-mixed") == 0) skip_mixed = 1;
        else if (strcmp(argv[i], "--db") == 0 && i + 1 < argc) path = argv[++i];
    }

    val = (char*)malloc((size_t)vlen + 1);
    if (!val) return 1;
    memset(val, 'x', (size_t)vlen); val[vlen] = 0;

    remove(path);
    memset(&opts, 0, sizeof opts);
    opts.sync_writes = sync;
    opts.compact_threshold = 0; /* disable auto-compact during bench */
    db = superdb_open(path, &opts);
    if (!db) { printf("open failed\n"); return 1; }

    /* SET */
    t0 = now_s();
    if (batch > 0) superdb_batch_begin(db);
    for (i = 0; i < N; i++) {
        int k = snprintf(key, sizeof key, "key:%08d", i);
        (void)k;
        if (superdb_set(db, key, val, (size_t)vlen) != 0) { printf("set failed\n"); return 1; }
        if (batch > 0 && (i + 1) % batch == 0) { superdb_batch_end(db); superdb_batch_begin(db); }
    }
    if (batch > 0) superdb_batch_end(db);
    t1 = now_s();
    printf("SDB SET   : %ld ops in %.3fs = %.0f ops/sec (vlen=%d sync=%d batch=%ld)\n",
           N, t1 - t0, N / (t1 - t0 + 1e-9), vlen, sync, batch);

    /* GET hit */
    t0 = now_s();
    {
        long hits = 0;
        for (i = 0; i < N; i++) {
            size_t n = 0;
            void *v;
            snprintf(key, sizeof key, "key:%08d", i);
            v = superdb_get(db, key, &n);
            if (v && n == (size_t)vlen) hits++;
            superdb_free(v);
        }
        t1 = now_s();
        printf("SDB GET   : %ld ops in %.3fs = %.0f ops/sec (hits=%ld)\n",
               N, t1 - t0, N / (t1 - t0 + 1e-9), hits);
    }

    /* mixed 50/50 set+get on existing keys */
    if (!skip_mixed) {
    t0 = now_s();
    for (i = 0; i < N; i++) {
        size_t n = 0; void *v;
        snprintf(key, sizeof key, "key:%08ld", i % (N / 10 + 1));
        if ((i & 1) == 0) superdb_set(db, key, val, (size_t)vlen);
        else { v = superdb_get(db, key, &n); superdb_free(v); }
    }
    t1 = now_s();
    printf("SDB MIXED : %ld ops in %.3fs = %.0f ops/sec\n", N, t1 - t0, N / (t1 - t0 + 1e-9));
    } /* skip_mixed */

    printf("SDB log size: %lu bytes\n", (unsigned long)superdb_log_size(db));
    superdb_close(db);
    free(val);
    remove(path);
    return 0;
}
