/* superdb-cli — command-line client for superdb-server.
 * One-shot: superdb-cli [opts] GET foo
 * REPL:     superdb-cli [opts]          (or -i)
 * Transport lives in sdb_client; this file is arg parsing + REPL.
 */
#include "sdb_client.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#ifdef _WIN32
#  ifndef WIN32_LEAN_AND_MEAN
#  define WIN32_LEAN_AND_MEAN
#  endif
#  include <winsock2.h>
#endif

static void usage(const char *p) {
    fprintf(stderr,
        "Usage: %s [-h HOST] [-p PORT] [-a PASSWORD] [-i] [command args...]\n"
        "  No command (or -i) enters an interactive REPL.\n"
        "  The password may also come from the SUPERDB_PASSWORD environment variable.\n"
        "  Examples:\n"
        "    %s SET foo bar\n"
        "    %s GET foo\n"
        "    %s KEYS 'prefix*'\n", p, p, p, p);
}

int main(int argc, char **argv) {
    const char *host = "127.0.0.1";
    const char *password = getenv("SUPERDB_PASSWORD");
    int port = 7381;
    int interactive = 0;
    int i, cmd_start = 1;

    for (i = 1; i < argc; i++) {
        if (strcmp(argv[i], "-h") == 0 && i + 1 < argc) { host = argv[++i]; cmd_start = i + 1; }
        else if (strcmp(argv[i], "-p") == 0 && i + 1 < argc) { port = atoi(argv[++i]); cmd_start = i + 1; }
        else if (strcmp(argv[i], "-a") == 0 && i + 1 < argc) { password = argv[++i]; cmd_start = i + 1; }
        else if (strcmp(argv[i], "-i") == 0 || strcmp(argv[i], "--interactive") == 0) { interactive = 1; cmd_start = i + 1; }
        else if (strcmp(argv[i], "--help") == 0) { usage(argv[0]); return 0; }
        else break;
    }
    cmd_start = i;

#ifdef _WIN32
    {
        WSADATA wd;
        if (WSAStartup(MAKEWORD(2,2), &wd) != 0) { fprintf(stderr, "WSAStartup failed\n"); return 1; }
    }
#endif

    if (sdb_client_connect(host, port) != 0) return 1;

    if (password && *password) {
        char *auth[2];
        auth[0] = "AUTH";
        auth[1] = (char*)password;
        if (sdb_client_command(2, auth) != 0) { sdb_client_close(); return 1; }
    }

    if (cmd_start < argc && !interactive) {
        int rc = sdb_client_command(argc - cmd_start, argv + cmd_start);
        sdb_client_close();
        return rc == 0 ? 0 : 1;
    }

    /* REPL */
    printf("SuperDB cli — %s:%d  (type QUIT to exit)\n", host, port);
    for (;;) {
        char line[65536];
        char *toks[256];
        char *tok;
        int n = 0;
        printf("superdb> ");
        fflush(stdout);
        if (!fgets(line, sizeof line, stdin)) break;
        /* strip newline */
        line[strcspn(line, "\r\n")] = 0;
        if (!line[0]) continue;
        /* naive space split (REPL; use one-shot for binary values) */
        tok = strtok(line, " \t");
        while (tok && n < 256) { toks[n++] = tok; tok = strtok(NULL, " \t"); }
        if (n == 0) continue;
        if ((strcmp(toks[0], "QUIT") == 0 || strcmp(toks[0], "quit") == 0 ||
             strcmp(toks[0], "EXIT") == 0 || strcmp(toks[0], "exit") == 0)) {
            sdb_client_command(n, toks);
            break;
        }
        if (sdb_client_command(n, toks) != 0) break;
    }
    sdb_client_close();
#ifdef _WIN32
    WSACleanup();
#endif
    return 0;
}
