/* sdb_cmd — implementation. See sdb_cmd.h.
 * Moved verbatim out of the old monolithic server.c; the only change is
 * that the db handle is a parameter instead of a global. */
#include "sdb_cmd.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

/* argv: array of (ptr,len), not NUL-terminated necessarily; we copy key */
void sdb_execute(superdb_t *db, char **argv, size_t *arglen,
                 int argc, sbuf_t *out, int *quit) {
    *quit = 0;
    if (argc < 1) { sb_err(out, "empty command"); return; }

    if (sdb_cmd_eq(argv[0], "PING")) {
        if (argc == 1) sb_str(out, "+PONG\r\n");
        else sb_bulk(out, argv[1], arglen[1]);
    } else if (sdb_cmd_eq(argv[0], "ECHO")) {
        if (argc != 2) sb_err(out, "ECHO <msg>");
        else sb_bulk(out, argv[1], arglen[1]);
    } else if (sdb_cmd_eq(argv[0], "SET")) {
        /* SET key value [EX seconds] */
        int64_t ttl_ms = 0;
        const char *key; size_t klen;
        const char *val; size_t vlen;
        if (argc < 3) { sb_err(out, "SET <key> <value> [EX seconds]"); return; }
        key = argv[1]; klen = arglen[1];
        val = argv[2]; vlen = arglen[2];
        if (argc >= 5 && sdb_cmd_eq(argv[3], "EX")) {
            char tmp[32]; long secs;
            if (arglen[4] >= sizeof tmp) { sb_err(out, "bad EX"); return; }
            memcpy(tmp, argv[4], arglen[4]); tmp[arglen[4]] = 0;
            secs = strtol(tmp, NULL, 10);
            if (secs <= 0) { sb_err(out, "bad EX seconds"); return; }
            ttl_ms = (int64_t)secs * 1000;
        } else if (argc != 3 && argc != 5) {
            sb_err(out, "SET <key> <value> [EX seconds]"); return;
        }
        /* keys must be NUL-safe for core: copy */
        {
            char *k = (char*)malloc(klen + 1);
            int rc;
            if (!k) { sb_err(out, "oom"); return; }
            memcpy(k, key, klen); k[klen] = 0;
            if (memchr(k, '\0', klen)) { free(k); sb_err(out, "key has NUL"); return; }
            rc = (ttl_ms > 0)
                ? superdb_set_ex(db, k, val, vlen, ttl_ms)
                : superdb_set(db, k, val, vlen);
            free(k);
            if (rc != 0) sb_err(out, superdb_err(db));
            else sb_ok(out);
        }
    } else if (sdb_cmd_eq(argv[0], "GET")) {
        if (argc != 2) { sb_err(out, "GET <key>"); return; }
        {
            char *k = (char*)malloc(arglen[1] + 1);
            void *v; size_t n = 0;
            if (!k) { sb_err(out, "oom"); return; }
            memcpy(k, argv[1], arglen[1]); k[arglen[1]] = 0;
            v = superdb_get(db, k, &n);
            free(k);
            if (!v) sb_str(out, "$-1\r\n");
            else { sb_bulk(out, v, n); superdb_free(v); }
        }
    } else if (sdb_cmd_eq(argv[0], "DEL")) {
        int64_t n = 0;
        int i;
        if (argc < 2) { sb_err(out, "DEL <key> [key ...]"); return; }
        for (i = 1; i < argc; i++) {
            char *k = (char*)malloc(arglen[i] + 1);
            if (!k) continue;
            memcpy(k, argv[i], arglen[i]); k[arglen[i]] = 0;
            n += superdb_del(db, k);
            free(k);
        }
        sb_int(out, n);
    } else if (sdb_cmd_eq(argv[0], "EXISTS")) {
        int64_t n = 0;
        int i;
        if (argc < 2) { sb_err(out, "EXISTS <key> [key ...]"); return; }
        for (i = 1; i < argc; i++) {
            char *k = (char*)malloc(arglen[i] + 1);
            if (!k) continue;
            memcpy(k, argv[i], arglen[i]); k[arglen[i]] = 0;
            n += superdb_exists(db, k);
            free(k);
        }
        sb_int(out, n);
    } else if (sdb_cmd_eq(argv[0], "KEYS")) {
        superdb_keys_t ks;
        char *pat = NULL;
        size_t pn = 0;
        if (argc > 2) { sb_err(out, "KEYS [prefix]"); return; }
        if (argc == 2) {
            /* support "prefix*" glob: strip trailing * */
            pat = argv[1]; pn = arglen[1];
            if (pn > 0 && pat[pn-1] == '*') pn--;
            if (pn == 1 && pat[0] == '*') pn = 0;
        }
        {
            char *pfx = (char*)malloc(pn + 1);
            size_t i;
            if (!pfx) { sb_err(out, "oom"); return; }
            if (pn) memcpy(pfx, pat ? pat : "", pn);
            pfx[pn] = 0;
            ks = superdb_keys(db, pfx);
            free(pfx);
            {
                char hb[64]; int h = snprintf(hb, sizeof hb, "*%lu\r\n", (unsigned long)ks.count);
                sb_append(out, hb, (size_t)h);
                for (i = 0; i < ks.count; i++) sb_bulk_str(out, ks.keys[i]);
            }
            superdb_free_keys(&ks);
        }
    } else if (sdb_cmd_eq(argv[0], "EXPIRE")) {
        if (argc != 3) { sb_err(out, "EXPIRE <key> <seconds>"); return; }
        {
            char *k = (char*)malloc(arglen[1] + 1);
            char tb[32]; long secs; int r;
            if (!k) { sb_err(out, "oom"); return; }
            memcpy(k, argv[1], arglen[1]); k[arglen[1]] = 0;
            if (arglen[2] >= sizeof tb) { free(k); sb_err(out, "bad seconds"); return; }
            memcpy(tb, argv[2], arglen[2]); tb[arglen[2]] = 0;
            secs = strtol(tb, NULL, 10);
            if (secs <= 0) { free(k); sb_err(out, "bad seconds"); return; }
            r = superdb_expire(db, k, (int64_t)secs * 1000);
            free(k);
            sb_int(out, r ? 1 : 0);
        }
    } else if (sdb_cmd_eq(argv[0], "TTL")) {
        if (argc != 2) { sb_err(out, "TTL <key>"); return; }
        {
            char *k = (char*)malloc(arglen[1] + 1);
            int64_t t;
            if (!k) { sb_err(out, "oom"); return; }
            memcpy(k, argv[1], arglen[1]); k[arglen[1]] = 0;
            t = superdb_ttl(db, k);
            free(k);
            if (t == -2) sb_int(out, -2);
            else if (t == -1) sb_int(out, -1);
            else sb_int(out, (t + 999) / 1000); /* seconds, like redis */
        }
    } else if (sdb_cmd_eq(argv[0], "PTTL")) {
        if (argc != 2) { sb_err(out, "PTTL <key>"); return; }
        {
            char *k = (char*)malloc(arglen[1] + 1);
            int64_t t;
            if (!k) { sb_err(out, "oom"); return; }
            memcpy(k, argv[1], arglen[1]); k[arglen[1]] = 0;
            t = superdb_ttl(db, k);
            free(k);
            sb_int(out, t);
        }
    } else if (sdb_cmd_eq(argv[0], "INCR") || sdb_cmd_eq(argv[0], "INCRBY")) {
        int is_by = sdb_cmd_eq(argv[0], "INCRBY");
        int64_t by = 1;
        if ((!is_by && argc != 2 && argc != 3) || (is_by && argc != 3)) {
            sb_err(out, is_by ? "INCRBY <key> <delta>" : "INCR <key> [by]");
            return;
        }
        if (is_by || argc == 3) {
            char tb[32];
            const char *src = argv[2];
            size_t sl = arglen[2];
            if (sl >= sizeof tb) { sb_err(out, "bad number"); return; }
            memcpy(tb, src, sl); tb[sl] = 0;
            by = strtoll(tb, NULL, 10);
        }
        {
            char *k = (char*)malloc(arglen[1] + 1);
            int ok = 0; int64_t v;
            if (!k) { sb_err(out, "oom"); return; }
            memcpy(k, argv[1], arglen[1]); k[arglen[1]] = 0;
            v = superdb_incr(db, k, by, &ok);
            free(k);
            if (!ok) sb_err(out, superdb_err(db));
            else sb_int(out, v);
        }
    } else if (sdb_cmd_eq(argv[0], "DBSIZE")) {
        sb_int(out, (int64_t)superdb_count(db));
    } else if (sdb_cmd_eq(argv[0], "FLUSHALL")) {
        if (superdb_flushall(db) != 0) sb_err(out, superdb_err(db));
        else sb_ok(out);
    } else if (sdb_cmd_eq(argv[0], "COMPACT") || sdb_cmd_eq(argv[0], "SAVE") || sdb_cmd_eq(argv[0], "BGREWRITEAOF")) {
        if (superdb_compact(db) != 0) sb_err(out, superdb_err(db));
        else sb_ok(out);
    } else if (sdb_cmd_eq(argv[0], "INFO")) {
        char b[512];
        int n = snprintf(b, sizeof b,
            "superdb_version:%s\r\nkeys:%lu\r\nlog_bytes:%lu\r\n",
            SUPERDB_VERSION,
            (unsigned long)superdb_count(db),
            (unsigned long)superdb_log_size(db));
        if (n > 0) sb_bulk(out, b, (size_t)n);
    } else if (sdb_cmd_eq(argv[0], "QUIT") || sdb_cmd_eq(argv[0], "EXIT")) {
        sb_ok(out);
        *quit = 1;
    } else if (sdb_cmd_eq(argv[0], "SHUTDOWN")) {
        sb_ok(out);
        *quit = 2; /* server-wide */
    } else if (sdb_cmd_eq(argv[0], "COMMAND") || sdb_cmd_eq(argv[0], "HELP")) {
        sb_bulk_str(out, "PING ECHO SET GET DEL EXISTS KEYS EXPIRE TTL PTTL INCR INCRBY DBSIZE FLUSHALL COMPACT INFO QUIT SHUTDOWN");
    } else {
        sb_err(out, "unknown command");
    }
}
