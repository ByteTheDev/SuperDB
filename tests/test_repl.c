/* test_repl — replication primitives, in process (no sockets).
 * A replica pulls the primary's log with superdb_log_read and applies it with
 * superdb_replica_feed, exactly as the server's replica thread does. Pulls use
 * tiny chunks so records are split across feeds. The replica's log must end up
 * byte-identical to the primary's, including after a primary compaction (which
 * changes the generation and forces a resync). */
#include <stdio.h>
#include <string.h>
#include <stdlib.h>
#include "superdb.h"

#define CHECK(c, msg) do { \
    if (!(c)) { printf("FAIL: %s (line %d)\n", msg, __LINE__); return 1; } \
    else printf("ok: %s\n", msg); } while (0)

/* One pull. Returns 1 = applied bytes, 0 = caught up, 2 = resynced, -1 = error. */
static int pull(superdb_t *P, superdb_t *R, uint64_t *rgen, size_t chunk) {
    void *data = NULL;
    size_t n = 0;
    uint64_t total = 0, gen = 0;
    size_t off = superdb_log_size(R);
    int rc = superdb_log_read(P, off, chunk, &data, &n, &total, &gen);
    int result;
    if (rc != 0) { superdb_free(data); return -1; }
    if (gen != *rgen) {
        if (superdb_replica_reset(R, gen) != 0) { superdb_free(data); return -1; }
        *rgen = gen;
        superdb_free(data);
        return 2;
    }
    if (n) {
        result = superdb_replica_feed(R, data, n) == 0 ? 1 : -1;
    } else {
        result = 0;
    }
    superdb_free(data);
    return result;
}

static int sync_all(superdb_t *P, superdb_t *R, uint64_t *rgen, size_t chunk) {
    int steps = 0;
    for (;;) {
        int r = pull(P, R, rgen, chunk);
        if (r < 0) return -1;
        if (r == 0) return steps;
        steps++;
    }
}

static int same_bytes(const char *a, const char *b) {
    FILE *fa = fopen(a, "rb"), *fb = fopen(b, "rb");
    int same = 1;
    if (!fa || !fb) same = 0;
    while (same && fa && fb) {
        unsigned char x[512], y[512];
        size_t na = fread(x, 1, sizeof x, fa), nb = fread(y, 1, sizeof y, fb);
        if (na != nb || memcmp(x, y, na) != 0) same = 0;
        if (na == 0) break;
    }
    if (fa) fclose(fa);
    if (fb) fclose(fb);
    return same;
}

int main(void) {
    const char *pp = "test_repl_primary.sdb";
    const char *rp = "test_repl_replica.sdb";
    superdb_options_t o;
    superdb_t *P, *R;
    uint64_t rgen = 0;
    char key[32];
    int i;
    superdb_items_t it;
    void *v;
    size_t n;

    memset(&o, 0, sizeof o);
    o.sync_writes = 0;
    o.compact_threshold = 0;
    remove(pp); remove(rp);
    remove("test_repl_primary.sdb.tmp"); remove("test_repl_replica.sdb.tmp");

    P = superdb_open(pp, &o);
    R = superdb_open(rp, &o);
    CHECK(P && R, "open primary and replica");
    superdb_set_replica(R, 1);
    CHECK(superdb_compact(R) != 0, "a replica refuses its own compaction");

    /* mixed writes on the primary */
    superdb_set_str(P, "name", "superdb");
    superdb_set_ex_str(P, "session", "abc", 3600000);
    superdb_rpush(P, "queue", "job1", 4);
    superdb_rpush(P, "queue", "job2", 4);
    superdb_hset(P, "user:1", "email", 5, "a@b.c", 5);
    superdb_sadd(P, "tags", "red", 3);
    superdb_sadd(P, "tags", "blue", 4);
    superdb_expire(P, "name", 3600000);
    superdb_rename(P, "session", "sess2");
    superdb_lpop(P, "queue", NULL, NULL);

    CHECK(sync_all(P, R, &rgen, 13) > 0, "replica catches up in 13-byte pulls");
    CHECK(same_bytes(pp, rp), "replica log is byte-identical to the primary");
    CHECK(superdb_llen(R, "queue") == 1, "list state matches");
    CHECK(superdb_hget(R, "user:1", "email", 5, &v, &n) == 1 && n == 5, "hash state matches");
    superdb_free(v);
    CHECK(superdb_scard(R, "tags") == 2, "set state matches");
    CHECK(superdb_exists(R, "sess2") && !superdb_exists(R, "session"), "rename replicated");
    CHECK(superdb_ttl(R, "sess2") > 0 && superdb_ttl(R, "name") > 0, "TTLs replicated");

    /* open transaction: replica must not show it until it commits */
    CHECK(superdb_txn_begin(P) == 0, "primary opens a transaction");
    superdb_set_str(P, "tx:a", "1");
    superdb_set_str(P, "tx:b", "2");
    sync_all(P, R, &rgen, 64);
    CHECK(!superdb_exists(R, "tx:a") && !superdb_exists(R, "tx:b"),
          "replica hides an uncommitted transaction");
    CHECK(superdb_txn_commit(P) == 0, "primary commits");
    sync_all(P, R, &rgen, 64);
    CHECK(superdb_exists(R, "tx:a") && superdb_exists(R, "tx:b"),
          "replica applies the transaction once committed");
    CHECK(same_bytes(pp, rp), "log still identical after transaction");

    /* steady stream of writes, pulled in small chunks */
    for (i = 0; i < 300; i++) {
        snprintf(key, sizeof key, "bulk:%d", i);
        superdb_set_str(P, key, "payload-payload");
        if (i % 50 == 0) superdb_rpush(P, "bulk:list", key, strlen(key));
    }
    CHECK(sync_all(P, R, &rgen, 97) > 0, "bulk writes replicate in 97-byte pulls");
    CHECK(same_bytes(pp, rp), "log identical after bulk stream");
    CHECK(superdb_count(R) == superdb_count(P), "key counts match");

    /* primary compacts: generation changes, replica must resync */
    {
        uint64_t before = superdb_log_gen(P);
        CHECK(superdb_compact(P) == 0, "primary compacts");
        CHECK(superdb_log_gen(P) != before, "compaction changed the generation");
    }
    CHECK(sync_all(P, R, &rgen, 4096) > 0, "replica resyncs after primary compaction");
    CHECK(same_bytes(pp, rp), "replica matches the compacted primary");
    CHECK(superdb_count(R) == superdb_count(P), "counts match after resync");

    /* writes after the resync keep flowing */
    superdb_set_str(P, "after", "resync");
    sync_all(P, R, &rgen, 4096);
    CHECK(superdb_exists(R, "after"), "writes after resync replicate");
    CHECK(same_bytes(pp, rp), "log identical after resync");

    /* range reads on the replica agree with the primary */
    CHECK(superdb_lrange(P, "bulk:list", 0, -1, &it) == 0, "primary list read");
    {
        superdb_items_t jt;
        CHECK(superdb_lrange(R, "bulk:list", 0, -1, &jt) == 0 && jt.count == it.count &&
              jt.count == 6, "replica list read matches");
        superdb_free_items(&jt);
    }
    superdb_free_items(&it);

    superdb_close(R);
    superdb_close(P);
    remove(pp); remove(rp);
    remove("test_repl_primary.sdb.tmp"); remove("test_repl_replica.sdb.tmp");
    printf("\nALL REPLICATION TESTS PASSED\n");
    return 0;
}
