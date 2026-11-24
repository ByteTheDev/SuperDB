/* sdb_cmd — command dispatch: argv -> SuperDB core -> RESP reply.
 * Stateless apart from the db handle passed in. Private to SuperDB tools.
 */
#ifndef SDB_CMD_H
#define SDB_CMD_H

#include "superdb.h"
#include "sdb_resp.h"

/* argv: array of (ptr,len); values may be binary, keys must be NUL-free.
 * Appends exactly one reply to out. *quit: 0 = none, 1 = close connection,
 * 2 = shut the whole server down. */
void sdb_execute(superdb_t *db, char **argv, size_t *arglen,
                 int argc, sbuf_t *out, int *quit);

#endif
