/* test_resp — unit tests for superdb_resp (no db needed).
 * Checks reply builders and both request parsers, incl. binary safety.
 */
#include <stdio.h>
#include <string.h>
#include <stdlib.h>

#include "superdb_resp.h"

#define CHECK(c, msg) do { \
    if (!(c)) { printf("FAIL: %s (line %d)\n", msg, __LINE__); return 1; } \
    else printf("ok: %s\n", msg); } while (0)

static int expect_buf(const sbuf_t *s, const char *want, size_t wantlen, const char *msg) {
    if (s->len != wantlen || memcmp(s->data, want, wantlen) != 0) {
        printf("FAIL: %s (line %d): got len=%lu want len=%lu\n",
               msg, __LINE__, (unsigned long)s->len, (unsigned long)wantlen);
        return 1;
    }
    printf("ok: %s\n", msg);
    return 0;
}

int main(void) {
    setvbuf(stdout, NULL, _IONBF, 0);
    sbuf_t s;
    char **argv = NULL; size_t *arglen = NULL;
    int argc = -1; size_t consumed = 0;
    int r;

    /* --- builders --- */
    sb_init(&s); sb_ok(&s);
    if (expect_buf(&s, "+OK\r\n", 5, "sb_ok")) return 1;
    sb_free(&s);

    sb_init(&s); sb_err(&s, "nope");
    if (expect_buf(&s, "-ERR nope\r\n", 11, "sb_err")) return 1;
    sb_free(&s);

    sb_init(&s); sb_int(&s, -42);
    if (expect_buf(&s, ":-42\r\n", 6, "sb_int neg")) return 1;
    sb_free(&s);

    sb_init(&s); sb_int(&s, 0);
    if (expect_buf(&s, ":0\r\n", 4, "sb_int zero")) return 1;
    sb_free(&s);

    sb_init(&s); sb_bulk(&s, "hi", 2);
    if (expect_buf(&s, "$2\r\nhi\r\n", 8, "sb_bulk")) return 1;
    sb_free(&s);

    sb_init(&s); sb_bulk(&s, NULL, 0);
    if (expect_buf(&s, "$-1\r\n", 5, "sb_bulk nil")) return 1;
    sb_free(&s);

    sb_init(&s); sb_bulk(&s, "a\r\nb", 4);
    if (expect_buf(&s, "$4\r\na\r\nb\r\n", 10, "sb_bulk binary")) return 1;
    sb_free(&s);

    sb_init(&s); sb_bulk_str(&s, NULL);
    if (expect_buf(&s, "$-1\r\n", 5, "sb_bulk_str nil")) return 1;
    sb_free(&s);

    /* --- cmd compare --- */
    CHECK(sdb_cmd_eq("SET", "set") == 1, "cmd_eq case-insensitive");
    CHECK(sdb_cmd_eq("Get", "GET") == 1, "cmd_eq mixed case");
    CHECK(sdb_cmd_eq("SET", "SETS") == 0, "cmd_eq prefix differs");
    CHECK(sdb_cmd_eq("", "") == 1, "cmd_eq empty");

    /* --- RESP parse: full message --- */
    {
        const char *m = "*3\r\n$3\r\nSET\r\n$3\r\nfoo\r\n$3\r\nbar\r\n";
        r = sdb_parse_resp(m, strlen(m), &argv, &arglen, &argc, &consumed);
        CHECK(r == 1, "resp full returns 1");
        CHECK(argc == 3, "resp argc==3");
        CHECK(consumed == strlen(m), "resp consumes all");
        CHECK(arglen[0] == 3 && memcmp(argv[0], "SET", 3) == 0, "resp argv[0]");
        CHECK(arglen[1] == 3 && memcmp(argv[1], "foo", 3) == 0, "resp argv[1]");
        CHECK(arglen[2] == 3 && memcmp(argv[2], "bar", 3) == 0, "resp argv[2]");
        sdb_free_argv(argv, arglen, argc); argv = NULL; arglen = NULL;
    }

    /* --- RESP parse: binary-safe bulk with embedded CRLF --- */
    {
        const char *m = "*2\r\n$3\r\nGET\r\n$4\r\na\r\nb\r\n";
        r = sdb_parse_resp(m, strlen(m), &argv, &arglen, &argc, &consumed);
        CHECK(r == 1, "resp binary returns 1");
        CHECK(argc == 2, "resp binary argc==2");
        CHECK(arglen[1] == 4 && memcmp(argv[1], "a\r\nb", 4) == 0, "resp binary value intact");
        CHECK(argv[1][4] == '\0', "resp value NUL-terminated");
        sdb_free_argv(argv, arglen, argc); argv = NULL; arglen = NULL;
    }

    /* --- RESP parse: incomplete --- */
    {
        const char *m = "*3\r\n$3\r\nSET\r\n";
        r = sdb_parse_resp(m, strlen(m), &argv, &arglen, &argc, &consumed);
        CHECK(r == 0, "resp partial returns 0");
        sdb_free_argv(argv, arglen, argc); argv = NULL; arglen = NULL;
    }
    {
        const char *m = "*2\r\n$3\r\nGET\r\n$10\r\nshort";
        r = sdb_parse_resp(m, strlen(m), &argv, &arglen, &argc, &consumed);
        CHECK(r == 0, "resp short bulk returns 0");
        sdb_free_argv(argv, arglen, argc); argv = NULL; arglen = NULL;
    }

    /* --- RESP parse: errors --- */
    {
        const char *m = "*x\r\n";
        r = sdb_parse_resp(m, strlen(m), &argv, &arglen, &argc, &consumed);
        CHECK(r == -1, "resp bad count returns -1");
        sdb_free_argv(argv, arglen, argc); argv = NULL; arglen = NULL;
    }
    {
        const char *m = "*1\r\nX\r\n";
        r = sdb_parse_resp(m, strlen(m), &argv, &arglen, &argc, &consumed);
        CHECK(r == -1, "resp missing $ returns -1");
        sdb_free_argv(argv, arglen, argc); argv = NULL; arglen = NULL;
    }
    {
        const char *m = "*1\r\n$999999999\r\n";
        r = sdb_parse_resp(m, strlen(m), &argv, &arglen, &argc, &consumed);
        CHECK(r == -1, "resp huge bulk rejected");
        sdb_free_argv(argv, arglen, argc); argv = NULL; arglen = NULL;
    }

    /* --- inline parse --- */
    {
        const char *m = "PING\n";
        r = sdb_parse_inline(m, strlen(m), &argv, &arglen, &argc, &consumed);
        CHECK(r == 1 && argc == 1, "inline PING");
        CHECK(strcmp(argv[0], "PING") == 0, "inline argv[0]");
        sdb_free_argv(argv, arglen, argc); argv = NULL; arglen = NULL;
    }
    {
        const char *m = "SET foo bar baz qux\r\n";
        r = sdb_parse_inline(m, strlen(m), &argv, &arglen, &argc, &consumed);
        CHECK(r == 1 && argc == 3, "inline SET joins tail");
        CHECK(strcmp(argv[0], "SET") == 0, "inline SET cmd");
        CHECK(strcmp(argv[1], "foo") == 0, "inline SET key");
        CHECK(strcmp(argv[2], "bar baz qux") == 0, "inline SET value with spaces");
        sdb_free_argv(argv, arglen, argc); argv = NULL; arglen = NULL;
    }
    {
        const char *m = "GET foo";
        r = sdb_parse_inline(m, strlen(m), &argv, &arglen, &argc, &consumed);
        CHECK(r == 0, "inline partial returns 0");
        sdb_free_argv(argv, arglen, argc); argv = NULL; arglen = NULL;
    }
    {
        const char *m = "\n";
        r = sdb_parse_inline(m, strlen(m), &argv, &arglen, &argc, &consumed);
        CHECK(r == 1 && argc == 0, "inline empty line ignored");
        sdb_free_argv(argv, arglen, argc); argv = NULL; arglen = NULL;
    }

    printf("\nALL RESP TESTS PASSED\n");
    return 0;
}
