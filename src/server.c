/* superdb-server — tiny TCP server for SuperDB.
 * select() loop, one process, up to MAX_CLIENTS connections.
 * Wire format + command dispatch live in sdb_resp / sdb_cmd; this file
 * only owns sockets, client buffers and the event loop.
 * Portable: POSIX sockets + Winsock.
 */
#ifdef _WIN32
#  ifndef WIN32_LEAN_AND_MEAN
#  define WIN32_LEAN_AND_MEAN
#  endif
#  ifndef FD_SETSIZE
#  define FD_SETSIZE 1024
#  endif
#  include <winsock2.h>
#  include <ws2tcpip.h>
#  pragma comment(lib, "ws2_32.lib")
typedef SOCKET sock_t;
#  define SOCK_INVALID INVALID_SOCKET
#  define sock_close closesocket
#else
#  include <sys/types.h>
#  include <sys/socket.h>
#  include <netinet/in.h>
#  include <arpa/inet.h>
#  include <netdb.h>
#  include <unistd.h>
#  include <fcntl.h>
#  include <errno.h>
typedef int sock_t;
#  define SOCK_INVALID (-1)
#  define sock_close close
#endif

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <stdint.h>
#include <inttypes.h>

#include "superdb.h"
#include "sdb_resp.h"
#include "sdb_cmd.h"

#ifndef MAX_CLIENTS
#define MAX_CLIENTS 256
#endif
#define CLIENT_BUF_INIT 4096
#define CLIENT_BUF_MAX (8u*1024u*1024u)

typedef struct {
    sock_t fd;
    char *buf;
    size_t len, cap;
    int dead;
} client_t;

static int g_verbose = 0;

static void logmsg(const char *m) { if (g_verbose) { fprintf(stderr, "[superdb] %s\n", m); fflush(stderr); } }

static int send_all(sock_t fd, const char *p, size_t n) {
    size_t sent = 0;
    while (sent < n) {
#ifdef _WIN32
        int r = send(fd, p + sent, (int)(n - sent), 0);
#else
        ssize_t r = send(fd, p + sent, n - sent, 0);
#endif
        if (r <= 0) return -1;
        sent += (size_t)r;
    }
    return 0;
}

/* process buffered data; returns 1 if server shutdown requested */
static int drain_client(superdb_t *db, client_t *c) {
    int shutdown = 0;
    for (;;) {
        char **argv = NULL; size_t *arglen = NULL;
        int argc = 0; size_t consumed = 0;
        int r;
        sbuf_t out; int quit = 0;
        if (c->len == 0) break;
        if (c->buf[0] == '*') r = sdb_parse_resp(c->buf, c->len, &argv, &arglen, &argc, &consumed);
        else if (memchr(c->buf, '\n', c->len)) r = sdb_parse_inline(c->buf, c->len, &argv, &arglen, &argc, &consumed);
        else break; /* need more */
        if (r == 0) { sdb_free_argv(argv, arglen, argc); break; }
        if (r < 0) {
            sbuf_t e; sb_init(&e); sb_err(&e, "protocol error");
            if (e.data) send_all(c->fd, e.data, e.len);
            sb_free(&e);
            c->dead = 1;
            break;
        }
        /* consume */
        memmove(c->buf, c->buf + consumed, c->len - consumed);
        c->len -= consumed;
        if (argc == 0) { sdb_free_argv(argv, arglen, argc); continue; }
        sb_init(&out);
        sdb_execute(db, argv, arglen, argc, &out, &quit);
        sdb_free_argv(argv, arglen, argc);
        if (out.data && send_all(c->fd, out.data, out.len) != 0) {
            sb_free(&out);
            c->dead = 1;
            break;
        }
        sb_free(&out);
        if (quit == 1) { c->dead = 1; break; }
        if (quit == 2) { c->dead = 1; shutdown = 1; break; }
    }
    return shutdown;
}

/* ---------- main ---------- */

static void usage(const char *prog) {
    fprintf(stderr,
        "Usage: %s [--db FILE] [--host ADDR] [--port N] [-v]\n"
        "Defaults: --db ./superdb.sdb --host 127.0.0.1 --port 7381\n", prog);
}

int main(int argc, char **argv) {
    const char *dbpath = "./superdb.sdb";
    const char *host = "127.0.0.1";
    int port = 7381;
    int i;
    superdb_t *db = NULL;
    sock_t ls = SOCK_INVALID;
    client_t clients[MAX_CLIENTS];
    int nclients = 0;

    for (i = 1; i < argc; i++) {
        if ((strcmp(argv[i], "--db") == 0) && i + 1 < argc) dbpath = argv[++i];
        else if ((strcmp(argv[i], "--host") == 0) && i + 1 < argc) host = argv[++i];
        else if ((strcmp(argv[i], "--port") == 0) && i + 1 < argc) port = atoi(argv[++i]);
        else if (strcmp(argv[i], "-v") == 0 || strcmp(argv[i], "--verbose") == 0) g_verbose = 1;
        else if (strcmp(argv[i], "-h") == 0 || strcmp(argv[i], "--help") == 0) { usage(argv[0]); return 0; }
        else { usage(argv[0]); return 1; }
    }

    db = superdb_open(dbpath, NULL);
    if (!db) { fprintf(stderr, "cannot open db '%s'\n", dbpath); return 1; }

#ifdef _WIN32
    {
        WSADATA wd;
        if (WSAStartup(MAKEWORD(2,2), &wd) != 0) {
            fprintf(stderr, "WSAStartup failed\n"); return 1;
        }
    }
#endif

    ls = socket(AF_INET, SOCK_STREAM, 0);
    if (ls == SOCK_INVALID) { perror("socket"); return 1; }
    {
        int one = 1;
        setsockopt(ls, SOL_SOCKET, SO_REUSEADDR, (const char*)&one, sizeof one);
    }
    {
        struct sockaddr_in a;
        memset(&a, 0, sizeof a);
        a.sin_family = AF_INET;
        a.sin_port = htons((unsigned short)port);
        if (strcmp(host, "0.0.0.0") == 0) a.sin_addr.s_addr = htonl(INADDR_ANY);
        else a.sin_addr.s_addr = inet_addr(host);
        if (bind(ls, (struct sockaddr*)&a, sizeof a) != 0) {
            perror("bind"); return 1;
        }
    }
    if (listen(ls, 64) != 0) { perror("listen"); return 1; }

    for (i = 0; i < MAX_CLIENTS; i++) {
        clients[i].fd = SOCK_INVALID;
        clients[i].buf = NULL; clients[i].len = 0; clients[i].cap = 0;
        clients[i].dead = 0;
    }

    printf("SuperDB %s listening on %s:%d (db: %s)\n", SUPERDB_VERSION, host, port, dbpath);
    fflush(stdout);

    for (;;) {
        fd_set rfds;
        sock_t mx = ls;
        struct timeval tv;
        int r, ci;
        FD_ZERO(&rfds);
        FD_SET(ls, &rfds);
        for (ci = 0; ci < MAX_CLIENTS; ci++) {
            if (clients[ci].fd != SOCK_INVALID) {
                FD_SET(clients[ci].fd, &rfds);
#ifdef _WIN32
                (void)mx;
#else
                if (clients[ci].fd > mx) mx = clients[ci].fd;
#endif
            }
        }
        tv.tv_sec = 1; tv.tv_usec = 0;
#ifdef _WIN32
        r = select(0, &rfds, NULL, NULL, &tv);
#else
        r = select(mx + 1, &rfds, NULL, NULL, &tv);
#endif
        if (r < 0) {
#ifdef _WIN32
            if (WSAGetLastError() == WSAEINTR) continue;
#else
            if (errno == EINTR) continue;
#endif
            perror("select"); break;
        }
        if (FD_ISSET(ls, &rfds)) {
            sock_t cfd = accept(ls, NULL, NULL);
            int slot = -1;
            if (cfd != SOCK_INVALID) {
                for (ci = 0; ci < MAX_CLIENTS; ci++)
                    if (clients[ci].fd == SOCK_INVALID) { slot = ci; break; }
                if (slot < 0) {
                    const char *full = "-ERR too many clients\r\n";
                    send_all(cfd, full, strlen(full));
                    sock_close(cfd);
                } else {
                    clients[slot].fd = cfd;
                    clients[slot].len = 0;
                    clients[slot].dead = 0;
                    if (!clients[slot].buf) {
                        clients[slot].cap = CLIENT_BUF_INIT;
                        clients[slot].buf = (char*)malloc(clients[slot].cap);
                    }
                    if (g_verbose) fprintf(stderr, "client #%d connected\n", slot);
                    (void)nclients;
                }
            }
        }
        for (ci = 0; ci < MAX_CLIENTS; ci++) {
            if (clients[ci].fd != SOCK_INVALID && FD_ISSET(clients[ci].fd, &rfds)) {
                char tmp[65536];
#ifdef _WIN32
                int n = recv(clients[ci].fd, tmp, sizeof tmp, 0);
#else
                ssize_t n = recv(clients[ci].fd, tmp, sizeof tmp, 0);
#endif
                if (n <= 0) {
                    clients[ci].dead = 1;
                } else {
                    if (clients[ci].len + (size_t)n + 1 > clients[ci].cap) {
                        size_t nc = clients[ci].cap ? clients[ci].cap * 2 : CLIENT_BUF_INIT;
                        while (nc < clients[ci].len + (size_t)n + 1) nc *= 2;
                        if (nc > CLIENT_BUF_MAX) { clients[ci].dead = 1; }
                        else {
                            char *nb = (char*)realloc(clients[ci].buf, nc);
                            if (!nb) clients[ci].dead = 1;
                            else { clients[ci].buf = nb; clients[ci].cap = nc; }
                        }
                    }
                    if (!clients[ci].dead) {
                        memcpy(clients[ci].buf + clients[ci].len, tmp, (size_t)n);
                        clients[ci].len += (size_t)n;
                        if (drain_client(db, &clients[ci])) {
                            /* SHUTDOWN: close everything */
                            int cj;
                            for (cj = 0; cj < MAX_CLIENTS; cj++) {
                                if (clients[cj].fd != SOCK_INVALID) {
                                    sock_close(clients[cj].fd);
                                    clients[cj].fd = SOCK_INVALID;
                                    free(clients[cj].buf);
                                    clients[cj].buf = NULL;
                                    clients[cj].cap = 0;
                                    clients[cj].len = 0;
                                }
                            }
                            superdb_close(db);
                            sock_close(ls);
#ifdef _WIN32
                            WSACleanup();
#endif
                            printf("bye.\n");
                            return 0;
                        }
                    }
                }
            }
        }
        /* reap dead */
        for (ci = 0; ci < MAX_CLIENTS; ci++) {
            if (clients[ci].fd != SOCK_INVALID && clients[ci].dead) {
                sock_close(clients[ci].fd);
                clients[ci].fd = SOCK_INVALID;
                clients[ci].len = 0;
                clients[ci].dead = 0;
                if (g_verbose) fprintf(stderr, "client #%d disconnected\n", ci);
            }
        }
    }

    superdb_close(db);
    sock_close(ls);
#ifdef _WIN32
    WSACleanup();
#endif
    {
        /* keep compiler quiet about unused */
        (void)logmsg; (void)nclients;
    }
    return 0;
}
