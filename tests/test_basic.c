#include <stdio.h>
#include <string.h>
#include <assert.h>
#include <stdlib.h>
#include "superdb.h"

#define CHECK(c, msg) do { \
    if (!(c)) { printf("FAIL: %s\n", msg); return 1; } \
    else printf("ok: %s\n", msg); } while (0)

int main(void) {
    const char *path = "test_basic.sdb";
    superdb_options_t opts;
    superdb_t *db;
    char *v;
    size_t n = 0;
    int ok = 0;
    int64_t x;

    remove(path); remove("test_basic.sdb.tmp");

    memset(&opts, 0, sizeof opts);
    opts.sync_writes = 1;
    opts.compact_threshold = 1024 * 1024;

    db = superdb_open(path, &opts);
    CHECK(db != NULL, "open");

    CHECK(superdb_set_str(db, "hello", "world") == 0, "set hello=world");
    v = superdb_get_str(db, "hello");
    CHECK(v && strcmp(v, "world") == 0, "get hello");
    superdb_free(v);

    CHECK(superdb_set(db, "bin", "a\0b", 3) == 0, "set binary value");
    v = (char*)superdb_get(db, "bin", &n);
    CHECK(v && n == 3 && memcmp(v, "a\0b", 3) == 0, "get binary value");
    superdb_free(v);

    CHECK(superdb_exists(db, "hello") == 1, "exists");
    CHECK(superdb_exists(db, "nope") == 0, "not exists");

    CHECK(superdb_del(db, "hello") == 1, "del");
    CHECK(superdb_get_str(db, "hello") == NULL, "get deleted -> nil");

    /* incr */
    superdb_del(db, "counter");
    x = superdb_incr(db, "counter", 1, &ok);
    CHECK(ok && x == 1, "incr new -> 1");
    x = superdb_incr(db, "counter", 4, &ok);
    CHECK(ok && x == 5, "incr by 4 -> 5");

    /* expire */
    CHECK(superdb_set_str(db, "temp", "1") == 0, "set temp");
    CHECK(superdb_expire(db, "temp", 100000) == 1, "expire set");
    CHECK(superdb_ttl(db, "temp") > 0, "ttl positive");
    CHECK(superdb_set_ex_str(db, "temp2", "x", 100000) == 0, "set_ex");
    CHECK(superdb_ttl(db, "temp2") > 0, "ttl temp2");

    /* keys */
    superdb_set_str(db, "user:1", "a");
    superdb_set_str(db, "user:2", "b");
    superdb_set_str(db, "other", "c");
    {
        superdb_keys_t k = superdb_keys(db, "user:");
        CHECK(k.count == 2, "keys prefix user:");
        superdb_free_keys(&k);
    }

    /* persistence: close + reopen */
    superdb_close(db);
    db = superdb_open(path, &opts);
    CHECK(db != NULL, "reopen");
    v = superdb_get_str(db, "user:1");
    CHECK(v && strcmp(v, "a") == 0, "persist user:1");
    superdb_free(v);
    CHECK(superdb_get_str(db, "hello") == NULL, "persist deleted stays deleted");
    v = superdb_get_str(db, "counter");
    CHECK(v && strcmp(v, "5") == 0, "persist counter=5");
    superdb_free(v);

    CHECK(superdb_compact(db) == 0, "compact");
    v = superdb_get_str(db, "user:2");
    CHECK(v && strcmp(v, "b") == 0, "post-compact read");
    superdb_free(v);

    CHECK(superdb_flushall(db) == 0, "flushall");
    CHECK(superdb_count(db) == 0, "count 0 after flush");

    superdb_close(db);
    remove(path); remove("test_basic.sdb.tmp");
    printf("\nALL TESTS PASSED\n");
    return 0;
}
