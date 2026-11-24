/* test_batch — group-commit API: batch_begin/end, sync, durability. */
#include <stdio.h>
#include <string.h>
#include <stdlib.h>
#include "superdb.h"

#define CHECK(c, msg) do { \
    if (!(c)) { printf("FAIL: %s\n", msg); return 1; } \
    else printf("ok: %s\n", msg); } while (0)

int main(void) {
    const char *path = "test_batch.sdb";
    superdb_t *db;
    int i;
    char key[64], *v;

    remove(path);

    db = superdb_open(path, &(superdb_options_t){ 1, 0, 1024 });
    CHECK(db != NULL, "open sync=1");

    /* nested begin/end: only the outermost end fsyncs */
    superdb_batch_begin(db);
    superdb_batch_begin(db);
    for (i = 0; i < 5000; i++) {
        snprintf(key, sizeof key, "b:%05d", i);
        if (superdb_set_str(db, key, "v") != 0) { printf("FAIL: set %d\n", i); return 1; }
    }
    /* visible mid-batch (memtable), before any fsync */
    v = superdb_get_str(db, "b:00123");
    CHECK(v && strcmp(v, "v") == 0, "read-your-write inside batch");
    superdb_free(v);
    CHECK(superdb_batch_end(db) == 0, "inner batch_end");
    CHECK(superdb_count(db) == 5000, "count 5000 before outer end");
    CHECK(superdb_batch_end(db) == 0, "outer batch_end fsyncs");
    CHECK(superdb_sync(db) == 0, "explicit sync ok");

    /* TTL inside batch is preserved */
    superdb_batch_begin(db);
    CHECK(superdb_set_ex_str(db, "exp", "1", 60000) == 0, "set_ex in batch");
    CHECK(superdb_batch_end(db) == 0, "batch_end with ttl");
    CHECK(superdb_ttl(db, "exp") > 0, "ttl survives batch");
    superdb_close(db);

    /* durable across reopen */
    db = superdb_open(path, NULL);
    CHECK(db != NULL, "reopen");
    CHECK(superdb_count(db) == 5001, "count persists (5000 + exp)");
    v = superdb_get_str(db, "b:04999");
    CHECK(v && strcmp(v, "v") == 0, "value persists");
    superdb_free(v);
    superdb_close(db);

    remove(path);
    printf("\nALL BATCH TESTS PASSED\n");
    return 0;
}
