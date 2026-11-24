/* test_types — typed values (strings, lists, hashes, sets), the new string
 * commands (SETNX, GETSET, MSETNX, PERSIST, EXPIREAT, RENAME), cursor SCAN,
 * persistence across reopen, and migration of legacy SDB1 files. */
#include <stdio.h>
#include <string.h>
#include <stdlib.h>
#include <time.h>
#include "superdb.h"
#include "superdb_crc.h"

#define CHECK(c, msg) do { \
    if (!(c)) { printf("FAIL: %s (line %d)\n", msg, __LINE__); return 1; } \
    else printf("ok: %s\n", msg); } while (0)

static int cmp_str(const void *a, const void *b) {
    return strcmp(*(char *const *)a, *(char *const *)b);
}

/* Appends one SDB1 SET record (the legacy on-disk format) to f. */
static void write_v1_set(FILE *f, const char *key, const char *val) {
    size_t klen = strlen(key), vlen = strlen(val);
    unsigned char *body = (unsigned char*)malloc(17 + klen + vlen);
    unsigned char crc[4];
    uint32_t c;
    body[0] = 1;                       /* SET */
    memset(body + 1, 0, 8);            /* no expiry */
    body[9] = (unsigned char)klen; body[10] = (unsigned char)(klen >> 8);
    body[11] = (unsigned char)(klen >> 16); body[12] = (unsigned char)(klen >> 24);
    body[13] = (unsigned char)vlen; body[14] = (unsigned char)(vlen >> 8);
    body[15] = (unsigned char)(vlen >> 16); body[16] = (unsigned char)(vlen >> 24);
    memcpy(body + 17, key, klen);
    memcpy(body + 17 + klen, val, vlen);
    c = sdb_crc32(body, 17 + klen + vlen);
    crc[0] = (unsigned char)c; crc[1] = (unsigned char)(c >> 8);
    crc[2] = (unsigned char)(c >> 16); crc[3] = (unsigned char)(c >> 24);
    fwrite(crc, 1, 4, f);
    fwrite(body, 1, 17 + klen + vlen, f);
    free(body);
}

int main(void) {
    const char *path = "test_types.sdb";
    superdb_t *db;
    superdb_items_t it;
    superdb_keys_t ks;
    void *v;
    size_t n;
    int64_t now_s = (int64_t)time(NULL);

    remove(path); remove("test_types.sdb.tmp");
    db = superdb_open(path, NULL);
    CHECK(db != NULL, "open");

    /* ---- SETNX / GETSET ---- */
    CHECK(superdb_setnx(db, "a", "1", 1) == 1, "setnx on new key");
    CHECK(superdb_setnx(db, "a", "2", 1) == 0, "setnx on existing key");
    CHECK(superdb_get_ex(db, "a", &v, &n) == 1 && n == 1 && ((char*)v)[0] == '1', "setnx kept old value");
    superdb_free(v);
    CHECK(superdb_getset(db, "a", "9", 1, &v, &n) == 1 && n == 1 && ((char*)v)[0] == '1',
          "getset returns old value");
    superdb_free(v);
    CHECK(superdb_getset(db, "nokey", "x", 1, &v, &n) == 1 && v == NULL, "getset on missing key -> NULL");

    /* ---- MSETNX is all-or-nothing ---- */
    {
        const char *ks1[2] = {"m1", "m2"};
        const char *ks2[2] = {"m3", "m1"};
        const void *vs[2] = {"x", "y"};
        size_t vl[2] = {1, 1};
        CHECK(superdb_msetnx(db, 2, ks1, vs, vl) == 1, "msetnx on fresh keys");
        CHECK(superdb_exists(db, "m1") && superdb_exists(db, "m2"), "msetnx wrote both keys");
        CHECK(superdb_msetnx(db, 2, ks2, vs, vl) == 0, "msetnx refuses when a key exists");
        CHECK(!superdb_exists(db, "m3"), "msetnx wrote nothing on refusal");
    }

    /* ---- PERSIST / EXPIREAT ---- */
    superdb_set_str(db, "e", "v");
    CHECK(superdb_expireat(db, "e", now_s * 1000 + 100000000) == 1, "expireat in the future");
    CHECK(superdb_ttl(db, "e") > 0, "expireat sets a TTL");
    CHECK(superdb_persist(db, "e") == 1, "persist removes TTL");
    CHECK(superdb_ttl(db, "e") == -1, "no TTL after persist");
    CHECK(superdb_persist(db, "e") == 0, "persist without TTL -> 0");
    CHECK(superdb_expireat(db, "e", 1000) == 1 && !superdb_exists(db, "e"), "expireat in the past deletes");

    /* ---- RENAME ---- */
    superdb_set_str(db, "r1", "val");
    superdb_expire(db, "r1", 100000);
    superdb_set_str(db, "r2", "old");
    CHECK(superdb_rename(db, "r1", "r2") == 1, "rename overwrites destination");
    CHECK(!superdb_exists(db, "r1"), "rename removes source");
    v = superdb_get_str(db, "r2");
    CHECK(v && strcmp((char*)v, "val") == 0, "rename moved the value");
    superdb_free(v);
    CHECK(superdb_ttl(db, "r2") > 0, "rename keeps the TTL");
    CHECK(superdb_rename(db, "nosuch", "z") == 0, "rename of a missing key -> 0");
    CHECK(superdb_rename(db, "r2", "r2") == 1, "rename to itself is a no-op success");

    /* ---- TYPE and WRONGTYPE ---- */
    superdb_lpush(db, "L", "a", 1);
    CHECK(superdb_type(db, "L") == SUPERDB_T_LIST, "type of a list");
    CHECK(superdb_type(db, "r2") == SUPERDB_T_STRING, "type of a string");
    CHECK(superdb_type(db, "nothing") == SUPERDB_T_NONE, "type of a missing key");
    CHECK(superdb_get_ex(db, "L", &v, &n) == SUPERDB_WRONGTYPE, "GET on a list is WRONGTYPE");
    CHECK(superdb_hset(db, "L", "f", 1, "v", 1) == SUPERDB_WRONGTYPE, "HSET on a list is WRONGTYPE");
    CHECK(superdb_sadd(db, "L", "m", 1) == SUPERDB_WRONGTYPE, "SADD on a list is WRONGTYPE");
    {
        int ok = 1;
        superdb_incr(db, "L", 1, &ok);
        CHECK(!ok, "INCR on a list fails");
    }

    /* ---- LISTS ---- */
    CHECK(superdb_lpush(db, "pl", "2", 1) == 1, "lpush creates list");
    CHECK(superdb_lpush(db, "pl", "1", 1) == 2, "lpush returns length");
    CHECK(superdb_rpush(db, "pl", "3", 1) == 3, "rpush appends");
    CHECK(superdb_llen(db, "pl") == 3, "llen");
    CHECK(superdb_lrange(db, "pl", 0, -1, &it) == 0 && it.count == 3 &&
          memcmp(it.items[0], "1", 1) == 0 && memcmp(it.items[2], "3", 1) == 0,
          "lrange 0 -1 keeps order");
    superdb_free_items(&it);
    CHECK(superdb_lrange(db, "pl", -2, -1, &it) == 0 && it.count == 2 && memcmp(it.items[0], "2", 1) == 0,
          "lrange with negative indices");
    superdb_free_items(&it);
    CHECK(superdb_lrange(db, "pl", 5, 10, &it) == 0 && it.count == 0, "lrange past the end is empty");
    CHECK(superdb_lrange(db, "pl", 2, 1, &it) == 0 && it.count == 0, "lrange inverted is empty");
    superdb_free_items(&it);
    CHECK(superdb_lpop(db, "pl", &v, &n) == 1 && n == 1 && ((char*)v)[0] == '1', "lpop from head");
    superdb_free(v);
    CHECK(superdb_rpop(db, "pl", &v, &n) == 1 && n == 1 && ((char*)v)[0] == '3', "rpop from tail");
    superdb_free(v);
    CHECK(superdb_llen(db, "pl") == 1, "one element left");
    CHECK(superdb_rpop(db, "pl", &v, &n) == 1, "rpop last element");
    superdb_free(v);
    CHECK(!superdb_exists(db, "pl"), "empty list removes its key");
    CHECK(superdb_lpop(db, "pl", &v, &n) == 0, "pop from missing key -> 0");

    /* ---- HASHES ---- */
    CHECK(superdb_hset(db, "ph", "f1", 2, "v1", 2) == 1, "hset new field");
    CHECK(superdb_hset(db, "ph", "f1", 2, "v2", 2) == 0, "hset existing field updates");
    CHECK(superdb_hget(db, "ph", "f1", 2, &v, &n) == 1 && n == 2 && memcmp(v, "v2", 2) == 0, "hget");
    superdb_free(v);
    CHECK(superdb_hset(db, "ph", "f2", 2, "x", 1) == 1, "second field");
    CHECK(superdb_hset(db, "ph", "\0k", 2, "\xff", 1) == 1, "binary field name");
    CHECK(superdb_hget(db, "ph", "\0k", 2, &v, &n) == 1 && n == 1 && ((unsigned char*)v)[0] == 0xff,
          "binary field round-trips");
    superdb_free(v);
    CHECK(superdb_hlen(db, "ph") == 3, "hlen");
    CHECK(superdb_hgetall(db, "ph", &it) == 0 && it.count == 6, "hgetall returns field/value pairs");
    superdb_free_items(&it);
    CHECK(superdb_hdel(db, "ph", "f1", 2) == 1, "hdel existing");
    CHECK(superdb_hdel(db, "ph", "f1", 2) == 0, "hdel missing -> 0");
    CHECK(superdb_hget(db, "ph", "f1", 2, &v, &n) == 0, "hget deleted field -> missing");

    /* ---- SETS ---- */
    CHECK(superdb_sadd(db, "ps", "x", 1) == 1, "sadd new member");
    CHECK(superdb_sadd(db, "ps", "x", 1) == 0, "sadd duplicate -> 0");
    CHECK(superdb_sadd(db, "ps", "y", 1) == 1, "sadd second member");
    CHECK(superdb_scard(db, "ps") == 2, "scard");
    CHECK(superdb_sismember(db, "ps", "x", 1) == 1 && superdb_sismember(db, "ps", "z", 1) == 0,
          "sismember");
    CHECK(superdb_smembers(db, "ps", &it) == 0 && it.count == 2, "smembers");
    superdb_free_items(&it);
    CHECK(superdb_srem(db, "ps", "x", 1) == 1, "srem member");
    CHECK(superdb_srem(db, "ps", "x", 1) == 0, "srem missing -> 0");
    CHECK(superdb_srem(db, "ps", "y", 1) == 1 && !superdb_exists(db, "ps"), "empty set removes key");

    /* ---- SCAN: cursor walk returns every key exactly once ---- */
    {
        char key[32];
        int i;
        for (i = 0; i < 1000; i++) {
            snprintf(key, sizeof key, "k%04d", i);
            superdb_set_str(db, key, "v");
        }
        {
            size_t total = 0, cap = 4096, j;
            char **seen = (char**)malloc(cap * sizeof *seen);
            uint64_t cur = 0;
            do {
                superdb_keys_t part = {NULL, 0};
                uint64_t next = superdb_scan(db, cur, "k*", 7, &part);
                for (j = 0; j < part.count; j++) {
                    if (total == cap) {
                        cap *= 2;
                        seen = (char**)realloc(seen, cap * sizeof *seen);
                    }
                    seen[total++] = part.keys[j];   /* keep ownership */
                    part.keys[j] = NULL;
                }
                free(part.keys);
                cur = next;
            } while (cur != 0);
            qsort(seen, total, sizeof *seen, cmp_str);
            {
                int dupes = 0;
                for (j = 1; j < total; j++) if (strcmp(seen[j - 1], seen[j]) == 0) dupes++;
                CHECK(total == 1000 && dupes == 0, "scan with MATCH returns each key once");
            }
            for (j = 0; j < total; j++) free(seen[j]);
            free(seen);
        }
        {
            superdb_keys_t all = {NULL, 0};
            uint64_t cur = 0;
            size_t count_all = 0;
            do {
                superdb_keys_t part = {NULL, 0};
                cur = superdb_scan(db, cur, NULL, 16, &part);
                count_all += part.count;
                superdb_free_keys(&part);
            } while (cur != 0);
            superdb_free_keys(&all);
            CHECK(count_all == superdb_count(db), "unfiltered scan visits every key");
        }
    }

    /* ---- durable objects for the reopen check ---- */
    superdb_rpush(db, "keep:list", "one", 3);
    superdb_rpush(db, "keep:list", "two", 3);
    superdb_hset(db, "keep:hash", "color", 5, "green", 5);
    superdb_sadd(db, "keep:set", "m1", 2);
    superdb_sadd(db, "keep:set", "m2", 2);
    superdb_set_ex_str(db, "keep:ttl", "t", 100000);
    superdb_close(db);

    db = superdb_open(path, NULL);
    CHECK(db != NULL, "reopen");
    CHECK(superdb_llen(db, "keep:list") == 2, "list survives reopen");
    CHECK(superdb_lrange(db, "keep:list", 0, -1, &it) == 0 && it.count == 2 &&
          memcmp(it.items[0], "one", 3) == 0 && memcmp(it.items[1], "two", 3) == 0,
          "list order survives reopen");
    superdb_free_items(&it);
    CHECK(superdb_hget(db, "keep:hash", "color", 5, &v, &n) == 1 && n == 5 && memcmp(v, "green", 5) == 0,
          "hash survives reopen");
    superdb_free(v);
    CHECK(superdb_scard(db, "keep:set") == 2, "set survives reopen");
    CHECK(superdb_ttl(db, "keep:ttl") > 0, "TTL survives reopen");
    CHECK(superdb_get_ex(db, "r2", &v, &n) == 1, "string survives reopen");
    superdb_free(v);
    superdb_close(db);

    /* ---- legacy SDB1 file is read and migrated to SDB2 ---- */
    {
        const char *lp = "test_types_legacy.sdb";
        FILE *f;
        unsigned char magic[4];
        remove(lp);
        f = fopen(lp, "wb");
        CHECK(f != NULL, "create legacy file");
        fwrite("SDB1", 1, 4, f);
        write_v1_set(f, "lk", "legacy-value");
        write_v1_set(f, "lk2", "second");
        fclose(f);

        db = superdb_open(lp, NULL);
        CHECK(db != NULL, "open legacy SDB1 file");
        v = superdb_get_str(db, "lk2");
        CHECK(v && strcmp((char*)v, "second") == 0, "legacy values readable");
        superdb_free(v);
        superdb_close(db);

        f = fopen(lp, "rb");
        CHECK(f && fread(magic, 1, 4, f) == 4 && memcmp(magic, "SDB2", 4) == 0,
              "legacy file rewritten as SDB2");
        if (f) fclose(f);
        remove(lp);
    }

    /* ---- keys listing ---- */
    db = superdb_open(path, NULL);
    CHECK(db != NULL, "reopen for key listing");
    ks = superdb_keys(db, "keep:");
    CHECK(ks.count == 4, "KEYS prefix finds the durable objects");
    superdb_free_keys(&ks);
    CHECK(superdb_flushall(db) == 0 && superdb_count(db) == 0, "flushall empties the db");
    superdb_close(db);
    db = superdb_open(path, NULL);
    CHECK(db != NULL && superdb_count(db) == 0, "empty after flushall and reopen");
    superdb_close(db);

    remove(path); remove("test_types.sdb.tmp");
    printf("\nALL TYPE TESTS PASSED\n");
    return 0;
}
