/* sdb_cmd — command layer: request argv -> SuperDB core -> RESP reply.
 * Owns per-server state (ctx: password, read-only flag, stats, slowlog) and
 * per-connection state (session: auth, MULTI queue). Private to SuperDB tools.
 *
 * Argument invariant: every argv[i] comes from the RESP/inline parser, which
 * NUL-terminates it. Key arguments are used as C strings only after checking
 * that strlen() matches arglen (binary keys are rejected).
 */
#ifndef SDB_CMD_H
#define SDB_CMD_H

#include "superdb.h"
#include "sdb_resp.h"

typedef struct sdb_ctx sdb_ctx_t;

/* A request queued by MULTI. Owns its argv arrays. */
typedef struct { char **argv; size_t *arglen; int argc; } sdb_queued_t;

typedef struct sdb_session {
    int authed;                 /* passed AUTH (or no password is configured) */
    int in_multi;               /* between MULTI and EXEC/DISCARD */
    sdb_queued_t *q;
    size_t nq, qcap;
} sdb_session_t;

/* password: NULL = no authentication. slowlog_us: log commands slower than
 * this many microseconds (0 = all, <0 = off). Returns NULL on oom. */
sdb_ctx_t *sdb_ctx_new(superdb_t *db, const char *password, long long slowlog_us);
void        sdb_ctx_free(sdb_ctx_t *ctx);
superdb_t  *sdb_ctx_db(sdb_ctx_t *ctx);
void        sdb_ctx_set_readonly(sdb_ctx_t *ctx, int ro);
void        sdb_ctx_set_primary(sdb_ctx_t *ctx, const char *host, int port); /* INFO only */
void        sdb_ctx_request_shutdown(sdb_ctx_t *ctx);
int         sdb_ctx_shutdown_requested(sdb_ctx_t *ctx);
void        sdb_ctx_conn_opened(sdb_ctx_t *ctx);
void        sdb_ctx_conn_closed(sdb_ctx_t *ctx);

void sdb_session_init(sdb_session_t *s, sdb_ctx_t *ctx);
void sdb_session_free(sdb_session_t *s);

/* Executes one request and appends exactly one reply to out (EXEC appends an
 * array). *quit: 0 = keep going, 1 = close this connection, 2 = shut down.
 * Returns 1 if the request was queued by MULTI (the caller must NOT free argv,
 * the session owns it now), 0 otherwise (the caller frees argv). */
int sdb_dispatch(sdb_ctx_t *ctx, sdb_session_t *s, char **argv, size_t *arglen,
                 int argc, sbuf_t *out, int *quit);

#endif
