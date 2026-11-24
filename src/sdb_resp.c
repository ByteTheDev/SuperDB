/* sdb_resp — implementation. See sdb_resp.h. */
#include "sdb_resp.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <inttypes.h>

/* ---------- reply builders ---------- */

void sb_init(sbuf_t *s) { s->data = NULL; s->len = 0; s->cap = 0; }

void sb_free(sbuf_t *s) {
    if (s) { free(s->data); s->data = NULL; s->len = 0; s->cap = 0; }
}

void sb_append(sbuf_t *s, const void *p, size_t n) {
    if (s->len + n + 1 > s->cap) {
        size_t nc = s->cap ? s->cap * 2 : 256;
        char *nd;
        while (nc < s->len + n + 1) nc *= 2;
        nd = (char*)realloc(s->data, nc);
        if (!nd) return; /* OOM: reply truncated; caller still sends prefix */
        s->data = nd;
        s->cap = nc;
    }
    memcpy(s->data + s->len, p, n);
    s->len += n;
    s->data[s->len] = '\0';
}

void sb_str(sbuf_t *s, const char *t) { sb_append(s, t, strlen(t)); }
void sb_ok(sbuf_t *s) { sb_str(s, "+OK\r\n"); }

void sb_err(sbuf_t *s, const char *m) {
    sb_str(s, "-ERR "); sb_str(s, m); sb_str(s, "\r\n");
}

void sb_int(sbuf_t *s, int64_t v) {
    char b[64]; int n = snprintf(b, sizeof b, ":%" PRId64 "\r\n", v);
    if (n > 0) sb_append(s, b, (size_t)n);
}

void sb_bulk(sbuf_t *s, const void *p, size_t n) {
    char b[64]; int h;
    if (!p) { sb_str(s, "$-1\r\n"); return; }
    h = snprintf(b, sizeof b, "$%lu\r\n", (unsigned long)n);
    if (h > 0) sb_append(s, b, (size_t)h);
    sb_append(s, p, n);
    sb_str(s, "\r\n");
}

void sb_bulk_str(sbuf_t *s, const char *t) {
    if (!t) sb_str(s, "$-1\r\n");
    else sb_bulk(s, t, strlen(t));
}

int sdb_cmd_eq(const char *a, const char *b) {
    while (*a && *b) {
        char ca = *a++, cb = *b++;
        if (ca >= 'A' && ca <= 'Z') ca += 32;
        if (cb >= 'A' && cb <= 'Z') cb += 32;
        if (ca != cb) return 0;
    }
    return *a == *b;
}

/* ---------- request parsing ---------- */

int sdb_parse_resp(const char *p, size_t len,
                   char ***argv, size_t **arglen,
                   int *argc, size_t *consumed) {
    size_t pos = 0;
    long narg, i;
    char **av = NULL; size_t *al = NULL;

    if (len < 1 || p[0] != '*') return 0;
    /* read *<n>\r\n */
    {
        const char *eol = memchr(p, '\n', len);
        long v;
        if (!eol) return 0;
        v = strtol(p + 1, NULL, 10);
        if (v <= 0 || v > 1024) return -1;
        narg = v;
        pos = (size_t)(eol - p) + 1;
    }
    av = (char**)calloc((size_t)narg, sizeof(*av));
    al = (size_t*)calloc((size_t)narg, sizeof(*al));
    if (!av || !al) { free(av); free(al); return -1; }
    for (i = 0; i < narg; i++) {
        long blen;
        const char *eol;
        size_t line_end;
        if (pos >= len) { /* incomplete */
            long j; for (j = 0; j < i; j++) free(av[j]);
            free(av); free(al); return 0;
        }
        if (p[pos] != '$') {
            long j; for (j = 0; j < i; j++) free(av[j]);
            free(av); free(al); return -1;
        }
        eol = memchr(p + pos, '\n', len - pos);
        if (!eol) {
            long j; for (j = 0; j < i; j++) free(av[j]);
            free(av); free(al); return 0;
        }
        blen = strtol(p + pos + 1, NULL, 10);
        if (blen < 0 || blen > 64*1024*1024) {
            long j; for (j = 0; j < i; j++) free(av[j]);
            free(av); free(al); return -1;
        }
        line_end = (size_t)(eol - p) + 1;
        pos = line_end;
        if (len - pos < (size_t)blen + 2) {
            long j; for (j = 0; j < i; j++) free(av[j]);
            free(av); free(al); return 0;
        }
        av[i] = (char*)malloc((size_t)blen + 1);
        if (!av[i]) {
            long j; for (j = 0; j < i; j++) free(av[j]);
            free(av); free(al); return -1;
        }
        memcpy(av[i], p + pos, (size_t)blen);
        av[i][blen] = '\0';
        al[i] = (size_t)blen;
        pos += (size_t)blen;
        /* expect \r\n */
        if (pos + 1 >= len || p[pos] != '\r' || p[pos+1] != '\n') {
            /* allow bare \n? be strict: need \r\n */
            if (!(pos < len && p[pos] == '\n')) {
                long j; for (j = 0; j <= i; j++) free(av[j]);
                free(av); free(al); return -1;
            }
            pos += 1;
        } else pos += 2;
    }
    *argv = av; *arglen = al; *argc = (int)narg; *consumed = pos;
    return 1;
}

int sdb_parse_inline(const char *buf, size_t len,
                     char ***argv, size_t **arglen,
                     int *argc, size_t *consumed) {
    const char *eol = memchr(buf, '\n', len);
    size_t linelen;
    char *line, *tok;
    char **av = NULL; size_t *al = NULL;
    int cap = 0, n = 0;
    size_t i;

    if (!eol) return 0;
    linelen = (size_t)(eol - buf);
    if (linelen > 0 && buf[linelen-1] == '\r') linelen--;
    line = (char*)malloc(linelen + 1);
    if (!line) return -1;
    memcpy(line, buf, linelen);
    line[linelen] = 0;
    *consumed = (size_t)(eol - buf) + 1;

    /* tokenize: for SET, rejoin tail so values with spaces work */
    tok = line;
    /* skip leading spaces */
    while (*tok == ' ' || *tok == '\t') tok++;
    while (*tok) {
        char *end = tok;
        while (*end && *end != ' ' && *end != '\t') end++;
        if (n >= cap) {
            int nc = cap ? cap * 2 : 8;
            char **na = (char**)realloc(av, (size_t)nc * sizeof(*na));
            if (!na) { free(line);
                for (i = 0; i < (size_t)n; i++) free(av[i]);
                free(av); free(al); return -1; }
            av = na;
            {
                size_t *nl = (size_t*)realloc(al, (size_t)nc * sizeof(*nl));
                if (!nl) { free(line);
                    for (i = 0; i < (size_t)n; i++) free(av[i]);
                    free(av); free(al); return -1; }
                al = nl;
            }
            cap = nc;
        }
        {
            size_t tl = (size_t)(end - tok);
            av[n] = (char*)malloc(tl + 1);
            if (!av[n]) { free(line);
                for (i = 0; i < (size_t)n; i++) free(av[i]);
                free(av); free(al); return -1; }
            memcpy(av[n], tok, tl); av[n][tl] = 0; al[n] = tl; n++;
        }
        if (!*end) break;
        *end = 0;
        tok = end + 1;
        while (*tok == ' ' || *tok == '\t') tok++;
        /* SET key <rest-of-line>: join remaining as value */
        if (n == 2 && av[0] && (sdb_cmd_eq(av[0], "SET"))) {
            if (*tok) {
                size_t rest = strlen(tok);
                char *joined = (char*)malloc(rest + 1);
                if (!joined) { free(line);
                    for (i = 0; i < (size_t)n; i++) free(av[i]);
                    free(av); free(al); return -1; }
                /* collect remaining tokens with single spaces */
                joined[0] = 0;
                {
                    char *r = tok;
                    size_t jl = 0;
                    while (*r) {
                        char *e2 = r;
                        while (*e2 && *e2 != ' ' && *e2 != '\t') e2++;
                        if (jl) joined[jl++] = ' ';
                        memcpy(joined + jl, r, (size_t)(e2 - r));
                        jl += (size_t)(e2 - r);
                        if (!*e2) break;
                        r = e2 + 1;
                        while (*r == ' ' || *r == '\t') r++;
                    }
                    joined[jl] = 0;
                    /* keep av[0]=cmd, av[1]=key; append joined value as av[2] */
                    if (n == 2) {
                        av[n] = joined; al[n] = strlen(joined); n++;
                    } else { free(joined); }
                }
            } else {
                /* SET with no value yet: leave for arity error */
            }
            break;
        }
    }
    free(line);
    if (n == 0) {
        free(av); free(al);
        *argv = NULL; *arglen = NULL; *argc = 0;
        return 1; /* empty line: caller ignores */
    }
    *argv = av; *arglen = al; *argc = n;
    return 1;
}

void sdb_free_argv(char **argv, size_t *arglen, int argc) {
    int i;
    if (!argv) { free(arglen); return; }
    for (i = 0; i < argc; i++) free(argv[i]);
    free(argv); free(arglen);
}
