/* sdb_repl — read-only replica: a thread that pulls the primary's log with
 * SYNCLOG and applies it. The replica's own log stays byte-identical to the
 * primary's, so its offset is simply its file size. Private to SuperDB tools. */
#ifndef SDB_REPL_H
#define SDB_REPL_H

#include "superdb_cmd.h"

typedef struct sdb_repl sdb_repl_t;

/* Starts pulling from host:port. password: primary's AUTH password or NULL.
 * Returns NULL on failure. The thread exits once ctx reports shutdown. */
sdb_repl_t *sdb_repl_start(sdb_ctx_t *ctx, const char *host, int port, const char *password);
void        sdb_repl_stop(sdb_repl_t *r);   /* joins the thread */

#endif
