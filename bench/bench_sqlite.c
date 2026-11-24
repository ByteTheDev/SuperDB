/* bench_sqlite — same workload as bench_sdb, for fair comparison.
 * Usage: bench_sqlite [--n N] [--vlen L] [--mode autocommit|batch|wal]
 *   autocommit: 1 txn per op, synchronous=FULL (durable, ~ SuperDB --sync)
 *   batch:      1 txn per 1000 ops, synchronous=FULL (durable batched)
 *   wal:        journal_mode=WAL, synchronous=NORMAL, batched (fast)
 */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sqlite3.h>

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

static void exec(sqlite3 *db, const char *sql) {
    char *err = 0;
    if (sqlite3_exec(db, sql, 0, 0, &err) != SQLITE_OK) {
        printf("sql err: %s [%s]\n", err ? err : "?", sql);
        sqlite3_free(err);
        exit(1);
    }
}

int main(int argc, char **argv) {
    long N = 100000;
    int vlen = 100;
    const char *mode = "autocommit";
    const char *path = "./bench_sqlite.db";
    int i;
    char key[64], *val;
    double t0, t1;
    sqlite3 *db;
    sqlite3_stmt *ins = 0, *sel = 0;

    for (i = 1; i < argc; i++) {
        if (strcmp(argv[i], "--n") == 0 && i + 1 < argc) N = atol(argv[++i]);
        else if (strcmp(argv[i], "--vlen") == 0 && i + 1 < argc) vlen = atoi(argv[++i]);
        else if (strcmp(argv[i], "--mode") == 0 && i + 1 < argc) mode = argv[++i];
    }

    val = (char*)malloc((size_t)vlen + 1);
    if (!val) return 1;
    memset(val, 'x', (size_t)vlen); val[vlen] = 0;

    remove(path);
    if (sqlite3_open(path, &db) != SQLITE_OK) { printf("open failed\n"); return 1; }

    if (strcmp(mode, "wal") == 0) {
        exec(db, "PRAGMA journal_mode=WAL;");
        exec(db, "PRAGMA synchronous=NORMAL;");
    } else {
        exec(db, "PRAGMA journal_mode=DELETE;");
        exec(db, "PRAGMA synchronous=FULL;");
    }
    exec(db, "CREATE TABLE kv(k TEXT PRIMARY KEY, v BLOB);");
    if (sqlite3_prepare_v2(db, "INSERT OR REPLACE INTO kv(k,v) VALUES(?,?);", -1, &ins, 0) != SQLITE_OK) return 1;
    if (sqlite3_prepare_v2(db, "SELECT v FROM kv WHERE k=?;", -1, &sel, 0) != SQLITE_OK) return 1;

    /* SET */
    t0 = now_s();
    if (strcmp(mode, "autocommit") == 0) {
        for (i = 0; i < N; i++) {
            snprintf(key, sizeof key, "key:%08d", i);
            sqlite3_bind_text(ins, 1, key, -1, SQLITE_TRANSIENT);
            sqlite3_bind_blob(ins, 2, val, vlen, SQLITE_TRANSIENT);
            if (sqlite3_step(ins) != SQLITE_DONE) { printf("insert failed\n"); return 1; }
            sqlite3_reset(ins);
        }
    } else {
        exec(db, "BEGIN;");
        for (i = 0; i < N; i++) {
            snprintf(key, sizeof key, "key:%08d", i);
            sqlite3_bind_text(ins, 1, key, -1, SQLITE_TRANSIENT);
            sqlite3_bind_blob(ins, 2, val, vlen, SQLITE_TRANSIENT);
            if (sqlite3_step(ins) != SQLITE_DONE) { printf("insert failed\n"); return 1; }
            sqlite3_reset(ins);
            if ((i + 1) % 1000 == 0) { exec(db, "COMMIT;"); exec(db, "BEGIN;"); }
        }
        exec(db, "COMMIT;");
    }
    t1 = now_s();
    printf("SQL SET   : %ld ops in %.3fs = %.0f ops/sec (vlen=%d mode=%s)\n",
           N, t1 - t0, N / (t1 - t0 + 1e-9), vlen, mode);

    /* GET */
    t0 = now_s();
    {
        long hits = 0;
        for (i = 0; i < N; i++) {
            snprintf(key, sizeof key, "key:%08d", i);
            sqlite3_bind_text(sel, 1, key, -1, SQLITE_TRANSIENT);
            if (sqlite3_step(sel) == SQLITE_ROW) {
                if (sqlite3_column_bytes(sel, 0) == vlen) hits++;
            }
            sqlite3_reset(sel);
        }
        t1 = now_s();
        printf("SQL GET   : %ld ops in %.3fs = %.0f ops/sec (hits=%ld)\n",
               N, t1 - t0, N / (t1 - t0 + 1e-9), hits);
    }

    sqlite3_finalize(ins);
    sqlite3_finalize(sel);
    sqlite3_close(db);
    free(val);
    remove(path);
    remove("./bench_sqlite.db-wal");
    remove("./bench_sqlite.db-shm");
    remove("./bench_sqlite.db-journal");
    return 0;
}
