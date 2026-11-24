/* sdb_repl — implementation. See sdb_repl.h.
 *
 * Protocol (primary side is sdb_cmd's SYNCLOG):
 *   request : SYNCLOG <gen> <offset> <max>
 *   reply   : *3  $gen  $total  $data      (data = nil when gen mismatches)
 * On a generation mismatch the replica empties itself and adopts the new gen.
 */
#include "superdb_repl.h"
#include "superdb_net.h"
#include "superdb_thread.h"
#include "superdb.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <inttypes.h>

#define REPL_CHUNK (1u << 20)

struct sdb_repl {
    sdb_ctx_t *ctx;
    char host[256];
    int port;
    char password[256];
    int has_password;
    sdb_thread_t th;
    sdb_sock_t fd;
};

/* Buffered reader that stops waiting when the server is shutting down. */
typedef struct {
    sdb_repl_t *r;
    unsigned char *buf;
    size_t len, pos, cap;
} rd_t;

static int rd_fill(rd_t *rd) {
    unsigned char tmp[65536];
    long n;
    for (;;) {
        if (sdb_ctx_shutdown_requested(rd->r->ctx)) return -1;
        {
            int ready = sdb_net_poll_readable(rd->r->fd, 200);
            if (ready < 0) return -1;
            if (ready == 0) continue;
        }
        n = sdb_net_recv(rd->r->fd, tmp, sizeof tmp);
        if (n <= 0) return -1;
        if (rd->pos > 0) {
            memmove(rd->buf, rd->buf + rd->pos, rd->len - rd->pos);
            rd->len -= rd->pos;
            rd->pos = 0;
        }
        if (rd->len + (size_t)n > rd->cap) {
            size_t nc = rd->cap ? rd->cap : 65536;
            unsigned char *nb;
            while (nc < rd->len + (size_t)n) nc *= 2;
            nb = (unsigned char*)realloc(rd->buf, nc);
            if (!nb) return -1;
            rd->buf = nb;
            rd->cap = nc;
        }
        memcpy(rd->buf + rd->len, tmp, (size_t)n);
        rd->len += (size_t)n;
        return 0;
    }
}

/* Reads one CRLF-terminated line (without the CRLF). */
static int rd_line(rd_t *rd, char *out, size_t cap) {
    for (;;) {
        size_t i;
        for (i = rd->pos; i + 1 < rd->len; i++) {
            if (rd->buf[i] == '\r' && rd->buf[i + 1] == '\n') {
                size_t n = i - rd->pos;
                if (n + 1 > cap) return -1;
                memcpy(out, rd->buf + rd->pos, n);
                out[n] = '\0';
                rd->pos = i + 2;
                return 0;
            }
        }
        if (rd_fill(rd) != 0) return -1;
    }
}

/* Reads exactly n bytes into a fresh buffer. */
static int rd_bytes(rd_t *rd, size_t n, unsigned char **out) {
    unsigned char *p = (unsigned char*)malloc(n + 1);   /* +1: callers may treat it as a C string */
    size_t got = 0;
    if (!p) return -1;
    p[n] = 0;
    while (got < n) {
        size_t avail = rd->len - rd->pos;
        size_t take = avail < n - got ? avail : n - got;
        if (take) {
            memcpy(p + got, rd->buf + rd->pos, take);
            rd->pos += take;
            got += take;
        } else if (rd_fill(rd) != 0) {
            free(p);
            return -1;
        }
    }
    /* consume the trailing CRLF of the bulk string */
    while (rd->len - rd->pos < 2) {
        if (rd_fill(rd) != 0) { free(p); return -1; }
    }
    rd->pos += 2;
    *out = p;
    return 0;
}

/* Reads a bulk string. *nil = 1 for $-1 (then *out is NULL). */
static int rd_bulk(rd_t *rd, unsigned char **out, size_t *n, int *nil) {
    char line[64];
    long len;
    *nil = 0; *out = NULL; *n = 0;
    if (rd_line(rd, line, sizeof line) != 0 || line[0] != '$') return -1;
    len = strtol(line + 1, NULL, 10);
    if (len < 0) { *nil = 1; return 0; }
    *n = (size_t)len;
    return rd_bytes(rd, (size_t)len, out);
}

static int send_cmd(sdb_repl_t *r, const char *s) {
    return sdb_net_send_all(r->fd, s, strlen(s));
}

/* Authenticates if a password is configured. 0 = ok. */
static int do_auth(sdb_repl_t *r, rd_t *rd) {
    char line[256];
    char req[320];
    if (!r->has_password) return 0;
    snprintf(req, sizeof req, "AUTH %s\r\n", r->password);
    if (send_cmd(r, req) != 0) return -1;
    if (rd_line(rd, line, sizeof line) != 0) return -1;
    return line[0] == '+' ? 0 : -1;
}

/* One SYNCLOG round trip. Returns 0 ok, -1 connection/protocol error. */
static int sync_once(sdb_repl_t *r, rd_t *rd, uint64_t *gen, int *mismatch) {
    char line[128];
    char req[160];
    unsigned char *gb = NULL, *tb = NULL, *data = NULL;
    size_t gn = 0, tn = 0, dn = 0;
    int nil_g, nil_t, nil_d;
    uint64_t total = 0;
    long cnt;
    superdb_t *db = sdb_ctx_db(r->ctx);
    int rc = -1;

    snprintf(req, sizeof req, "SYNCLOG %" PRIu64 " %" PRIu64 " %u\r\n",
             *gen, (uint64_t)superdb_log_size(db), REPL_CHUNK);
    if (send_cmd(r, req) != 0) return -1;
    if (rd_line(rd, line, sizeof line) != 0 || line[0] != '*') goto out;
    cnt = strtol(line + 1, NULL, 10);
    if (cnt != 3) goto out;
    if (rd_bulk(rd, &gb, &gn, &nil_g) != 0 || nil_g) goto out;
    if (rd_bulk(rd, &tb, &tn, &nil_t) != 0 || nil_t) goto out;
    if (rd_bulk(rd, &data, &dn, &nil_d) != 0) goto out;
    *gen = strtoull((const char*)gb, NULL, 10);
    total = strtoull((const char*)tb, NULL, 10);
    (void)total;
    if (nil_d) {
        *mismatch = 1;
        rc = 0;
        goto out;
    }
    *mismatch = 0;
    if (dn && superdb_replica_feed(db, data, dn) != 0) goto out;
    rc = 0;
out:
    free(gb); free(tb); free(data);
    return rc;
}

static void repl_main(void *arg) {
    sdb_repl_t *r = (sdb_repl_t*)arg;
    superdb_t *db = sdb_ctx_db(r->ctx);
    uint64_t gen = 0;             /* 0 never matches: forces an initial reset */
    while (!sdb_ctx_shutdown_requested(r->ctx)) {
        rd_t rd;
        memset(&rd, 0, sizeof rd);
        rd.r = r;
        r->fd = sdb_net_connect(r->host, r->port);
        if (r->fd != SDB_SOCK_INVALID && do_auth(r, &rd) == 0) {
            while (!sdb_ctx_shutdown_requested(r->ctx)) {
                int mismatch = 0;
                size_t before = superdb_log_size(db);
                if (sync_once(r, &rd, &gen, &mismatch) != 0) break;
                if (mismatch) {
                    if (superdb_replica_reset(db, gen) != 0) break;
                    continue;               /* re-request from the new start */
                }
                if (superdb_log_size(db) == before) sdb_sleep_ms(100);   /* caught up */
            }
        }
        if (r->fd != SDB_SOCK_INVALID) {
            sdb_net_shutdown(r->fd);
            sdb_net_close(r->fd);
            r->fd = SDB_SOCK_INVALID;
        }
        free(rd.buf);
        /* back off before reconnecting, in steps so shutdown stays responsive */
        {
            int i;
            for (i = 0; i < 10 && !sdb_ctx_shutdown_requested(r->ctx); i++) sdb_sleep_ms(100);
        }
    }
}

sdb_repl_t *sdb_repl_start(sdb_ctx_t *ctx, const char *host, int port, const char *password) {
    sdb_repl_t *r = (sdb_repl_t*)calloc(1, sizeof *r);
    if (!r) return NULL;
    r->ctx = ctx;
    snprintf(r->host, sizeof r->host, "%s", host);
    r->port = port;
    r->fd = SDB_SOCK_INVALID;
    if (password && *password) {
        snprintf(r->password, sizeof r->password, "%s", password);
        r->has_password = 1;
    }
    if (sdb_thread_start(&r->th, repl_main, r) != 0) { free(r); return NULL; }
    return r;
}

void sdb_repl_stop(sdb_repl_t *r) {
    if (!r) return;
    sdb_thread_join(&r->th);
    free(r);
}
