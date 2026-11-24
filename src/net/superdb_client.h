/* superdb_client — blocking RESP client transport for superdb-cli.
 * Connect, send a command, read one reply, pretty-print it.
 * Private to SuperDB tools (not the embedded library API).
 */
#ifndef SUPERDB_CLIENT_H
#define SUPERDB_CLIENT_H

/* 0 = connected, -1 = failed (message already printed). */
int sdb_client_connect(const char *host, int port);
void sdb_client_close(void);

/* Build a RESP request from argv, send it, read and print the reply. */
int sdb_client_command(int argc, char **argv);

#endif
