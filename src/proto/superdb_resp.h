/* superdb_resp — RESP-subset framing: reply builders + request parsers.
 * Shared by the server and unit-tested in tests/test_resp.c.
 * No SuperDB core dependency (pure bytes in, structured args out).
 */
#ifndef SUPERDB_RESP_H
#define SUPERDB_RESP_H

#include <stddef.h>
#include <stdint.h>

/* Growable reply buffer. */
typedef struct { char *data; size_t len, cap; } sbuf_t;

void sb_init(sbuf_t *s);
void sb_free(sbuf_t *s);
void sb_append(sbuf_t *s, const void *p, size_t n);
void sb_str(sbuf_t *s, const char *t);
void sb_ok(sbuf_t *s);                 /* +OK */
void sb_err(sbuf_t *s, const char *m); /* -ERR msg */
void sb_int(sbuf_t *s, int64_t v);     /* :n */
void sb_bulk(sbuf_t *s, const void *p, size_t n); /* $<n> bytes (NULL = $-1) */
void sb_bulk_str(sbuf_t *s, const char *t);

/* ASCII case-insensitive compare (used for command dispatch). */
int sdb_cmd_eq(const char *a, const char *b);

/* Parse one request from buf[0..len).
 * Returns 1 = got command (argv/arglen malloc'd, *consumed set),
 *         0 = need more data, -1 = protocol error.
 * sdb_parse_resp:  RESP arrays  (*<n> $<len> bytes ...)
 * sdb_parse_inline: telnet lines (SET key joins the tail so values
 *                   may contain spaces; binary values need RESP).
 * Free results with sdb_free_argv. Empty inline line yields argc=0.
 */
int sdb_parse_resp(const char *buf, size_t len,
                   char ***argv, size_t **arglen,
                   int *argc, size_t *consumed);
int sdb_parse_inline(const char *buf, size_t len,
                     char ***argv, size_t **arglen,
                     int *argc, size_t *consumed);
void sdb_free_argv(char **argv, size_t *arglen, int argc);

#endif
