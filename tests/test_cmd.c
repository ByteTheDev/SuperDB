/* test_cmd — command layer through sdb_dispatch (no sockets). Checks exact RESP
 * replies for every command family, plus auth, read-only, MULTI/EXEC, slowlog,
 * INFO, SYNCLOG and BACKUP. */
#include <stdio.h>
#include <string.h>
#include <stdlib.h>
#include "superdb.h"
#include "superdb_cmd.h"
#include "superdb_resp.h"

#define CHECK(c, msg) do { \
    if (!(c)) { printf("FAIL: %s (line %d)\n", msg, __LINE__); return 1; } \
    else printf("ok: %s\n", msg); } while (0)

/* Runs one inline command line; returns the reply as a malloc'd string. */
static char *R(sdb_ctx_t *ctx, sdb_session_t *s, const char *line, int *queued) {
    char buf[2048];
    char **argv = NULL;
    size_t *al = NULL;
    int argc = 0, quit = 0;
    size_t consumed = 0;
    sbuf_t out;
    int q;
    snprintf(buf, sizeof buf, "%s\n", line);
    if (sdb_parse_inline(buf, strlen(buf), &argv, &al, &argc, &consumed) != 1) return NULL;
    sb_init(&out);
    q = sdb_dispatch(ctx, s, argv, al, argc, &out, &quit);
    if (!q) sdb_free_argv(argv, al, argc);
    if (queued) *queued = q;
    {
        char *r = (char*)malloc(out.len + 1);
        if (r) { memcpy(r, out.data ? out.data : "", out.len); r[out.len] = '\0'; }
        sb_free(&out);
        return r;
    }
}

/* Same, but asserts the reply equals want exactly. */
static int expect(sdb_ctx_t *ctx, sdb_session_t *s, const char *line, const char *want) {
    char *r = R(ctx, s, line, NULL);
    int ok = r && strcmp(r, want) == 0;
    if (!ok) printf("  [%s] got: %s\n  want: %s\n", line, r ? r : "(null)", want);
    free(r);
    return ok;
}

static int contains(sdb_ctx_t *ctx, sdb_session_t *s, const char *line, const char *needle) {
    char *r = R(ctx, s, line, NULL);
    int ok = r && strstr(r, needle) != NULL;
    if (!ok) printf("  [%s] reply lacks '%s': %s\n", line, needle, r ? r : "(null)");
    free(r);
    return ok;
}

int main(void) {
    const char *path = "test_cmd.sdb";
    superdb_t *db;
    sdb_ctx_t *ctx;
    sdb_session_t s;
    char *r;
    int queued = 0;

    remove(path); remove("test_cmd.sdb.tmp");
    db = superdb_open(path, NULL);
    CHECK(db != NULL, "open");
    ctx = sdb_ctx_new(db, NULL, -1);
    CHECK(ctx != NULL, "context");
    sdb_session_init(&s, ctx);

    /* ---- connection & basics ---- */
    CHECK(expect(ctx, &s, "PING", "+PONG\r\n"), "PING");
    CHECK(expect(ctx, &s, "ECHO hi", "$2\r\nhi\r\n"), "ECHO");
    CHECK(expect(ctx, &s, "FOO bar", "-ERR unknown command\r\n"), "unknown command is an error");
    CHECK(expect(ctx, &s, "GET", "-ERR wrong number of arguments for 'get' command\r\n"),
          "arity is checked before the handler");

    /* ---- strings ---- */
    CHECK(expect(ctx, &s, "SET k v", "+OK\r\n"), "SET");
    CHECK(expect(ctx, &s, "GET k", "$1\r\nv\r\n"), "GET");
    CHECK(expect(ctx, &s, "GET missing", "$-1\r\n"), "GET missing -> nil");
    CHECK(expect(ctx, &s, "SETNX k other", ":0\r\n"), "SETNX existing -> 0");
    CHECK(expect(ctx, &s, "SETNX fresh 1", ":1\r\n"), "SETNX new -> 1");
    CHECK(expect(ctx, &s, "INCR c", ":1\r\n"), "INCR");
    CHECK(expect(ctx, &s, "INCRBY c 4", ":5\r\n"), "INCRBY");
    CHECK(expect(ctx, &s, "INCR k", "-ERR value is not an integer\r\n"), "INCR on text fails");
    CHECK(expect(ctx, &s, "GETSET c 99", "$1\r\n5\r\n"), "GETSET returns old");
    CHECK(expect(ctx, &s, "MSETNX a 1 b 2", ":1\r\n"), "MSETNX all new");
    CHECK(expect(ctx, &s, "MSETNX a 9 zz 1", ":0\r\n"), "MSETNX refuses if any key exists");
    CHECK(expect(ctx, &s, "GET zz", "$-1\r\n"), "MSETNX wrote nothing on refusal");
    CHECK(expect(ctx, &s, "MSETNX a 1 b", "-ERR wrong number of arguments for 'msetnx' command\r\n"),
          "MSETNX needs key/value pairs");

    /* binary values over RESP */
    {
        const char *req = "*3\r\n$3\r\nSET\r\n$1\r\nb\r\n$4\r\na\r\nb\r\n";
        char **argv = NULL; size_t *al = NULL; int argc = 0; size_t consumed = 0; sbuf_t out;
        int quit = 0;
        CHECK(sdb_parse_resp(req, strlen(req), &argv, &al, &argc, &consumed) == 1, "parse binary SET");
        sb_init(&out);
        sdb_dispatch(ctx, &s, argv, al, argc, &out, &quit);
        sdb_free_argv(argv, al, argc);
        CHECK(out.len == 5 && memcmp(out.data, "+OK\r\n", 5) == 0, "binary SET accepted");
        sb_free(&out);
        CHECK(expect(ctx, &s, "GET b", "$4\r\na\r\nb\r\n"), "binary value with CRLF round-trips");
    }

    /* ---- lists ---- */
    CHECK(expect(ctx, &s, "LPUSH l a b", ":2\r\n"), "LPUSH returns length");
    CHECK(expect(ctx, &s, "LRANGE l 0 -1", "*2\r\n$1\r\nb\r\n$1\r\na\r\n"), "LRANGE");
    CHECK(expect(ctx, &s, "RPOP l", "$1\r\na\r\n"), "RPOP");
    CHECK(expect(ctx, &s, "LLEN l", ":1\r\n"), "LLEN");
    CHECK(expect(ctx, &s, "GET l", "-WRONGTYPE Operation against a key holding the wrong kind of value\r\n"),
          "GET on a list is WRONGTYPE");

    /* ---- hashes ---- */
    CHECK(expect(ctx, &s, "HSET h f v g w", ":2\r\n"), "HSET multiple pairs");
    CHECK(expect(ctx, &s, "HGET h f", "$1\r\nv\r\n"), "HGET");
    CHECK(expect(ctx, &s, "HGET h nope", "$-1\r\n"), "HGET missing -> nil");
    CHECK(contains(ctx, &s, "HGETALL h", "*4\r\n"), "HGETALL returns 4 elements");
    CHECK(expect(ctx, &s, "HDEL h f nope", ":1\r\n"), "HDEL counts removed fields");
    CHECK(expect(ctx, &s, "HLEN h", ":1\r\n"), "HLEN");

    /* ---- sets ---- */
    CHECK(expect(ctx, &s, "SADD s a b a", ":2\r\n"), "SADD counts new members");
    CHECK(expect(ctx, &s, "SCARD s", ":2\r\n"), "SCARD");
    CHECK(expect(ctx, &s, "SISMEMBER s a", ":1\r\n"), "SISMEMBER yes");
    CHECK(expect(ctx, &s, "SISMEMBER s z", ":0\r\n"), "SISMEMBER no");
    CHECK(contains(ctx, &s, "SMEMBERS s", "*2\r\n"), "SMEMBERS");
    CHECK(expect(ctx, &s, "SREM s a", ":1\r\n"), "SREM");

    /* ---- keyspace & expiry ---- */
    CHECK(expect(ctx, &s, "TYPE l", "+list\r\n"), "TYPE list");
    CHECK(expect(ctx, &s, "TYPE k", "+string\r\n"), "TYPE string");
    CHECK(expect(ctx, &s, "TYPE nothing", "+none\r\n"), "TYPE none");
    CHECK(expect(ctx, &s, "EXPIRE k 100", ":1\r\n"), "EXPIRE");
    CHECK(contains(ctx, &s, "TTL k", ":"), "TTL");
    CHECK(expect(ctx, &s, "PERSIST k", ":1\r\n"), "PERSIST");
    CHECK(expect(ctx, &s, "TTL k", ":-1\r\n"), "TTL after PERSIST is -1");
    CHECK(expect(ctx, &s, "EXPIREAT k 1", ":1\r\n"), "EXPIREAT in the past");
    CHECK(expect(ctx, &s, "GET k", "$-1\r\n"), "key removed by EXPIREAT");
    CHECK(expect(ctx, &s, "RENAME l l2", "+OK\r\n"), "RENAME");
    CHECK(expect(ctx, &s, "RENAME nosuch x", "-ERR no such key\r\n"), "RENAME missing");
    CHECK(expect(ctx, &s, "LLEN l2", ":1\r\n"), "renamed list kept its contents");
    CHECK(expect(ctx, &s, "DEL a b", ":2\r\n"), "DEL counts");
    CHECK(expect(ctx, &s, "EXISTS c fresh", ":2\r\n"), "EXISTS counts");
    CHECK(contains(ctx, &s, "KEYS *", "$"), "KEYS");
    CHECK(expect(ctx, &s, "SCAN 0 COUNT 100000 MATCH f*", "*2\r\n$1\r\n0\r\n*1\r\n$5\r\nfresh\r\n"),
          "SCAN with MATCH walks to completion in one call");
    CHECK(expect(ctx, &s, "SCAN xyz", "-ERR invalid cursor\r\n"), "SCAN rejects a bad cursor");

    /* ---- admin ---- */
    CHECK(contains(ctx, &s, "INFO", "superdb_version:2.0.0"), "INFO has version");
    CHECK(contains(ctx, &s, "INFO", "keys:"), "INFO has keys");
    CHECK(contains(ctx, &s, "INFO", "cmdstat_get:calls="), "INFO has command stats");
    CHECK(contains(ctx, &s, "COMMAND", "*"), "COMMAND lists commands");

    /* ---- MULTI / EXEC / DISCARD ---- */
    CHECK(expect(ctx, &s, "EXEC", "-ERR EXEC without MULTI\r\n"), "EXEC without MULTI");
    CHECK(expect(ctx, &s, "DISCARD", "-ERR DISCARD without MULTI\r\n"), "DISCARD without MULTI");
    CHECK(expect(ctx, &s, "MULTI", "+OK\r\n"), "MULTI");
    CHECK(expect(ctx, &s, "MULTI", "-ERR MULTI calls can not be nested\r\n"), "nested MULTI rejected");
    r = R(ctx, &s, "SET m1 1", &queued);
    CHECK(queued == 1 && strcmp(r, "+QUEUED\r\n") == 0, "commands are queued inside MULTI");
    free(r);
    r = R(ctx, &s, "INCR m1", &queued);
    CHECK(queued == 1, "second command queued");
    free(r);
    CHECK(expect(ctx, &s, "EXEC", "*2\r\n+OK\r\n:2\r\n"), "EXEC runs the queue in order");
    CHECK(expect(ctx, &s, "GET m1", "$1\r\n2\r\n"), "EXEC effects visible");
    CHECK(expect(ctx, &s, "MULTI", "+OK\r\n"), "MULTI again");
    R(ctx, &s, "SET z 1", NULL);
    CHECK(expect(ctx, &s, "DISCARD", "+OK\r\n"), "DISCARD");
    CHECK(expect(ctx, &s, "GET z", "$-1\r\n"), "discarded commands never ran");

    /* ---- SYNCLOG (replica protocol) ---- */
    {
        uint64_t gen = superdb_log_gen(db);
        char req[128];
        snprintf(req, sizeof req, "SYNCLOG %llu 4 1000", (unsigned long long)gen);
        char stale[128];
        char *sr;
        snprintf(stale, sizeof stale, "SYNCLOG %llu 4 1000", (unsigned long long)(gen + 1));
        CHECK(contains(ctx, &s, req, "*3\r\n"), "SYNCLOG with a matching generation returns data");
        sr = R(ctx, &s, stale, NULL);
        CHECK(sr && strstr(sr, "$-1\r\n") != NULL && strstr(sr, "*3\r\n") == sr,
              "SYNCLOG with a stale generation sends nil");
        free(sr);
        snprintf(stale, sizeof stale, "SYNCLOG %llu 999999999 10", (unsigned long long)gen);
        sr = R(ctx, &s, stale, NULL);
        CHECK(sr && strstr(sr, "-ERR") != NULL, "SYNCLOG past the end is refused");
        free(sr);
    }

    /* ---- BACKUP ---- */
    CHECK(expect(ctx, &s, "BACKUP test_cmd_backup.sdb", "+OK\r\n"), "BACKUP writes a snapshot");
    {
        superdb_t *bk = superdb_open("test_cmd_backup.sdb", NULL);
        char *v;
        CHECK(bk != NULL, "backup opens as a database");
        v = superdb_get_str(bk, "fresh");
        CHECK(v && strcmp(v, "1") == 0, "backup holds the data");
        superdb_free(v);
        superdb_close(bk);
        remove("test_cmd_backup.sdb");
    }

    /* ---- BGREWRITEAOF / COMPACT ---- */
    r = R(ctx, &s, "BGREWRITEAOF", NULL);
    CHECK(r && strncmp(r, "+Background", 11) == 0, "BGREWRITEAOF starts a background rewrite");
    free(r);
    CHECK(expect(ctx, &s, "COMPACT", "+OK\r\n"), "COMPACT waits for it and rewrites");

    /* ---- slowlog (threshold 0 logs everything) ---- */
    {
        sdb_ctx_t *slow = sdb_ctx_new(db, NULL, 0);
        sdb_session_t ss;
        CHECK(slow != NULL, "slowlog context");
        sdb_session_init(&ss, slow);
        R(slow, &ss, "PING", NULL);
        R(slow, &ss, "GET k2", NULL);
        /* PING and GET were logged; the LEN reply is computed before its own entry */
        CHECK(expect(slow, &ss, "SLOWLOG LEN", ":2\r\n"), "SLOWLOG records slow commands");
        CHECK(contains(slow, &ss, "SLOWLOG GET 2", "*2\r\n"), "SLOWLOG GET returns entries");
        CHECK(expect(slow, &ss, "SLOWLOG RESET", "+OK\r\n"), "SLOWLOG RESET");
        sdb_session_free(&ss);
        sdb_ctx_free(slow);
    }

    /* ---- authentication ---- */
    {
        sdb_ctx_t *authctx = sdb_ctx_new(db, "s3cret", -1);
        sdb_session_t as;
        CHECK(authctx != NULL, "auth context");
        sdb_session_init(&as, authctx);
        CHECK(expect(authctx, &as, "GET fresh", "-NOAUTH Authentication required.\r\n"),
              "commands need AUTH first");
        CHECK(expect(authctx, &as, "PING", "-NOAUTH Authentication required.\r\n"), "PING needs AUTH too");
        CHECK(expect(authctx, &as, "AUTH nope", "-WRONGPASS invalid password\r\n"), "wrong password");
        CHECK(expect(authctx, &as, "AUTH s3cret", "+OK\r\n"), "right password");
        CHECK(expect(authctx, &as, "GET fresh", "$1\r\n1\r\n"), "authenticated session works");
        sdb_session_free(&as);
        sdb_ctx_free(authctx);
    }

    /* ---- read-only replica ---- */
    {
        sdb_ctx_t *ro = sdb_ctx_new(db, NULL, -1);
        sdb_session_t rs;
        sdb_ctx_set_readonly(ro, 1);
        sdb_session_init(&rs, ro);
        CHECK(expect(ro, &rs, "SET x y", "-READONLY You can't write against a read only replica.\r\n"),
              "writes rejected on a read-only replica");
        CHECK(expect(ro, &rs, "GET fresh", "$1\r\n1\r\n"), "reads allowed on a read-only replica");
        sdb_session_free(&rs);
        sdb_ctx_free(ro);
    }

    /* ---- QUIT / SHUTDOWN signalling ---- */
    {
        sbuf_t out;
        char **argv = NULL; size_t *al = NULL; int argc = 0, quit = -1; size_t consumed = 0;
        char buf[16] = "QUIT\n";
        sdb_parse_inline(buf, strlen(buf), &argv, &al, &argc, &consumed);
        sb_init(&out);
        sdb_dispatch(ctx, &s, argv, al, argc, &out, &quit);
        sdb_free_argv(argv, al, argc);
        sb_free(&out);
        CHECK(quit == 1, "QUIT asks to close the connection");
        strcpy(buf, "SHUTDOWN\n");
        sdb_parse_inline(buf, strlen(buf), &argv, &al, &argc, &consumed);
        sb_init(&out);
        sdb_dispatch(ctx, &s, argv, al, argc, &out, &quit);
        sdb_free_argv(argv, al, argc);
        sb_free(&out);
        CHECK(quit == 2 && sdb_ctx_shutdown_requested(ctx), "SHUTDOWN requests server stop");
    }

    sdb_session_free(&s);
    sdb_ctx_free(ctx);
    superdb_close(db);
    remove(path); remove("test_cmd.sdb.tmp");
    printf("\nALL COMMAND TESTS PASSED\n");
    return 0;
}
