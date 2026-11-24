/* test_compact — background compaction. Writes keep flowing while the log is
 * rewritten on a worker thread; the result must match the in-memory state
 * exactly, both before and after reopen. Also covers automatic compaction. */
#include <stdio.h>
#include <string.h>
#include <stdlib.h>
#include "superdb.h"
#include "sdb_thread.h"

#define CHECK(c, msg) do { \
    if (!(c)) { printf("FAIL: %s (line %d)\n", msg, __LINE__); return 1; } \
    else printf("ok: %s\n", msg); } while (0)

static void wait_idle(superdb_t *db) {
    superdb_stats_t st;
    do {
        superdb_get_stats(db, &st);
        if (st.compacting) sdb_sleep_ms(1);
    } while (st.compacting);
}

static int check_value(superdb_t *db, const char *key, const char *want) {
    char *v = superdb_get_str(db, key);
    int ok = (want == NULL) ? (v == NULL) : (v != NULL && strcmp(v, want) == 0);
    superdb_free(v);
    return ok;
}

int main(void) {
    const char *path = "test_compact.sdb";
    superdb_options_t o;
    superdb_t *db;
    superdb_stats_t st;
    char key[32], val[128];
    int i, round;
    uint64_t gen_before, gen_after;
    size_t log_before;

    memset(&o, 0, sizeof o);
    o.sync_writes = 0;
    o.compact_threshold = 0;          /* manual compaction only, for this part */
    remove(path); remove("test_compact.sdb.tmp");

    db = superdb_open(path, &o);
    CHECK(db != NULL, "open");

    /* build a lot of garbage: every key overwritten several times */
    memset(val, 'x', sizeof val - 1);
    val[sizeof val - 1] = '\0';
    for (round = 0; round < 4; round++) {
        for (i = 0; i < 3000; i++) {
            snprintf(key, sizeof key, "c%05d", i);
            superdb_set_str(db, key, val);
        }
    }
    superdb_get_stats(db, &st);
    log_before = st.log_bytes;
    CHECK(st.dead_bytes > 0, "overwrites produced dead bytes");
    gen_before = superdb_log_gen(db);

    /* start a rewrite, then keep writing while the worker runs */
    CHECK(superdb_compact_async(db) == 0, "background compaction started");
    for (i = 0; i < 500; i++) {
        snprintf(key, sizeof key, "during:%d", i);
        superdb_set_str(db, key, "live");
    }
    for (i = 0; i < 100; i++) {
        snprintf(key, sizeof key, "c%05d", i);
        superdb_del(db, key);
    }
    superdb_rpush(db, "during:list", "a", 1);
    superdb_rpush(db, "during:list", "b", 1);
    superdb_expire(db, "during:0", 3600000);
    {
        int r = superdb_compact_async(db);
        CHECK(r == 1 || r == 0, "second request while running is refused or queued");
    }
    wait_idle(db);
    superdb_get_stats(db, &st);
    CHECK(st.compactions >= 1, "compaction completed");
    CHECK(st.log_bytes < log_before, "rewritten log is smaller");
    gen_after = superdb_log_gen(db);
    CHECK(gen_after != gen_before, "log generation changed");

    CHECK(superdb_count(db) == 3000 - 100 + 500 + 1, "in-memory count after concurrent writes");
    CHECK(check_value(db, "during:499", "live"), "write made during rewrite is visible");
    CHECK(check_value(db, "c00100", val), "untouched key survives rewrite");
    CHECK(check_value(db, "c00005", NULL), "delete made during rewrite is honoured");
    CHECK(superdb_llen(db, "during:list") == 2, "list pushed during rewrite is intact");
    CHECK(superdb_ttl(db, "during:0") > 0, "TTL set during rewrite is kept");

    /* reopen must reproduce exactly that state from the rewritten log */
    superdb_close(db);
    db = superdb_open(path, &o);
    CHECK(db != NULL, "reopen after rewrite");
    CHECK(superdb_count(db) == 3000 - 100 + 500 + 1, "count survives reopen");
    CHECK(check_value(db, "during:499", "live"), "concurrent write survives reopen");
    CHECK(check_value(db, "c00005", NULL), "concurrent delete survives reopen");
    CHECK(superdb_llen(db, "during:list") == 2, "concurrent list survives reopen");
    CHECK(superdb_ttl(db, "during:0") > 0, "TTL survives reopen");

    /* synchronous COMPACT still works and waits for any rewrite */
    CHECK(superdb_compact(db) == 0, "synchronous compact");
    CHECK(superdb_count(db) == 3000 - 100 + 500 + 1, "count unchanged by compact");
    superdb_close(db);

    /* ---- automatic background compaction on overwrite pressure ---- */
    remove(path); remove("test_compact.sdb.tmp");
    o.compact_threshold = 64 * 1024;
    db = superdb_open(path, &o);
    CHECK(db != NULL, "reopen with auto-compaction");
    for (i = 0; i < 4000; i++) {
        snprintf(val, sizeof val, "value-%d", i);
        superdb_set_str(db, "hot", val);
    }
    wait_idle(db);
    superdb_get_stats(db, &st);
    CHECK(st.compactions >= 1, "auto-compaction ran in the background");
    CHECK(st.log_bytes < 64 * 1024, "auto-compacted log stays small");
    CHECK(check_value(db, "hot", "value-3999"), "hot key holds the last value");
    superdb_close(db);

    db = superdb_open(path, &o);
    CHECK(check_value(db, "hot", "value-3999"), "hot key correct after reopen");
    superdb_close(db);

    remove(path); remove("test_compact.sdb.tmp");
    printf("\nALL COMPACTION TESTS PASSED\n");
    return 0;
}
