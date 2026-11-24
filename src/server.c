/* superdb-server — TCP server for SuperDB.
 * One thread per connection (bounded by --max-clients). The database is
 * mutex-guarded, so connections run concurrently; a slow client no longer
 * blocks the others. Owns sockets, connection registry and the accept loop;
 * the wire format lives in sdb_resp, commands in sdb_cmd, replication in
 * sdb_repl. Portable: POSIX sockets/pthreads and Winsock/Win32 threads.
 */
#include "superdb.h"
#include "sdb_resp.h"
#include "sdb_cmd.h"
#include "sdb_net.h"
#include "sdb_thread.h"
#include "sdb_repl.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#ifndef _WIN32
#  include <sys/socket.h>
#endif

#define CLIENT_BUF_MAX (8u * 1024u * 1024u)
#define DEFAULT_MAX_CLIENTS 256

typedef struct conn {
    sdb_sock_t fd;
    sdb_ctx_t *ctx;
    sdb_session_t sess;
    sdb_thread_t th;
    int done;                   /* set under reg.mu when the thread has finished */
    struct conn *next;
} conn_t;

typedef struct {
    sdb_mutex_t mu;
    conn_t *head;
    size_t count;
} registry_t;

static registry_t reg;
static sdb_ctx_t *g_ctx;

static int send_text(sdb_sock_t fd, const char *s) {
    return sdb_net_send_all(fd, s, strlen(s));
}

/* Executes every complete request in the buffer. Returns 1 to close the
 * connection (QUIT, SHUTDOWN, protocol error or failed send). */
static int drain(conn_t *c, char **bufp, size_t *lenp, size_t *capp) {
    for (;;) {
        char **argv = NULL; size_t *arglen = NULL;
        int argc = 0, r, quit = 0, queued;
        size_t consumed = 0;
        sbuf_t out;
        if (*lenp == 0) return 0;
        if ((*bufp)[0] == '*')
            r = sdb_parse_resp(*bufp, *lenp, &argv, &arglen, &argc, &consumed);
        else if (memchr(*bufp, '\n', *lenp))
            r = sdb_parse_inline(*bufp, *lenp, &argv, &arglen, &argc, &consumed);
        else
            return 0;                       /* need more bytes */
        if (r == 0) { sdb_free_argv(argv, arglen, argc); return 0; }
        if (r < 0) {
            send_text(c->fd, "-ERR protocol error\r\n");
            return 1;
        }
        memmove(*bufp, *bufp + consumed, *lenp - consumed);
        *lenp -= consumed;
        (void)capp;
        if (argc == 0) { sdb_free_argv(argv, arglen, argc); continue; }

        sb_init(&out);
        queued = sdb_dispatch(c->ctx, &c->sess, argv, arglen, argc, &out, &quit);
        if (!queued) sdb_free_argv(argv, arglen, argc);
        if (out.len && sdb_net_send_all(c->fd, out.data, out.len) != 0) {
            sb_free(&out);
            return 1;
        }
        sb_free(&out);
        if (quit) return 1;
    }
}

static void conn_main(void *arg) {
    conn_t *c = (conn_t*)arg;
    char *buf = NULL;
    size_t len = 0, cap = 0;
    for (;;) {
        char tmp[16384];
        long n = sdb_net_recv(c->fd, tmp, sizeof tmp);
        int close_now = 0;
        if (n <= 0) break;
        if (len + (size_t)n > cap) {
            size_t nc = cap ? cap : 4096;
            char *nb;
            while (nc < len + (size_t)n) nc *= 2;
            if (nc > CLIENT_BUF_MAX) break;
            nb = (char*)realloc(buf, nc);
            if (!nb) break;
            buf = nb;
            cap = nc;
        }
        memcpy(buf + len, tmp, (size_t)n);
        len += (size_t)n;
        close_now = drain(c, &buf, &len, &cap);
        if (close_now) break;
    }
    free(buf);
    sdb_ctx_conn_closed(c->ctx);

    sdb_mutex_lock(&reg.mu);
    sdb_net_close(c->fd);
    c->fd = SDB_SOCK_INVALID;
    c->done = 1;
    sdb_mutex_unlock(&reg.mu);
}

/* Joins and frees connections whose threads have finished. */
static void reap_finished(void) {
    conn_t *done = NULL, **pp;
    sdb_mutex_lock(&reg.mu);
    pp = &reg.head;
    while (*pp) {
        conn_t *c = *pp;
        if (c->done) {
            *pp = c->next;
            c->next = done;
            done = c;
            reg.count--;
        } else {
            pp = &c->next;
        }
    }
    sdb_mutex_unlock(&reg.mu);
    while (done) {
        conn_t *c = done;
        done = c->next;
        sdb_thread_join(&c->th);
        sdb_session_free(&c->sess);
        free(c);
    }
}

static void usage(const char *prog) {
    fprintf(stderr,
        "Usage: %s [--db FILE] [--host ADDR] [--port N] [--max-clients N]\n"
        "          [--requirepass PWD] [--slowlog-us N] [--replicaof HOST:PORT]\n"
        "          [--primary-auth PWD] [-v]\n"
        "Defaults: --db ./superdb.sdb --host 127.0.0.1 --port 7381 --max-clients 256\n"
        "          --slowlog-us 10000 (log commands slower than this; -1 = off)\n"
        "Password can also come from the SUPERDB_PASSWORD environment variable.\n", prog);
}

int main(int argc, char **argv) {
    const char *dbpath = "./superdb.sdb";
    const char *host = "127.0.0.1";
    const char *password = getenv("SUPERDB_PASSWORD");
    const char *primary_auth = getenv("SUPERDB_PRIMARY_PASSWORD");
    const char *replicaof = NULL;
    long long slowlog_us = 10000;
    int port = 7381;
    size_t max_clients = DEFAULT_MAX_CLIENTS;
    superdb_t *db;
    sdb_sock_t ls;
    sdb_repl_t *repl = NULL;
    int i;

    for (i = 1; i < argc; i++) {
        if (strcmp(argv[i], "--db") == 0 && i + 1 < argc) dbpath = argv[++i];
        else if (strcmp(argv[i], "--host") == 0 && i + 1 < argc) host = argv[++i];
        else if (strcmp(argv[i], "--port") == 0 && i + 1 < argc) port = atoi(argv[++i]);
        else if (strcmp(argv[i], "--max-clients") == 0 && i + 1 < argc) max_clients = (size_t)atol(argv[++i]);
        else if (strcmp(argv[i], "--requirepass") == 0 && i + 1 < argc) password = argv[++i];
        else if (strcmp(argv[i], "--primary-auth") == 0 && i + 1 < argc) primary_auth = argv[++i];
        else if (strcmp(argv[i], "--slowlog-us") == 0 && i + 1 < argc) slowlog_us = atoll(argv[++i]);
        else if (strcmp(argv[i], "--replicaof") == 0 && i + 1 < argc) replicaof = argv[++i];
        else if (strcmp(argv[i], "-v") == 0 || strcmp(argv[i], "--verbose") == 0) { /* accepted for compatibility */ }
        else if (strcmp(argv[i], "-h") == 0 || strcmp(argv[i], "--help") == 0) { usage(argv[0]); return 0; }
        else { usage(argv[0]); return 1; }
    }
    if (max_clients == 0) max_clients = DEFAULT_MAX_CLIENTS;

    if (sdb_net_init() != 0) { fprintf(stderr, "network init failed\n"); return 1; }
    if (sdb_mutex_init(&reg.mu) != 0) { fprintf(stderr, "mutex init failed\n"); return 1; }

    db = superdb_open(dbpath, NULL);
    if (!db) { fprintf(stderr, "cannot open db '%s'\n", dbpath); return 1; }
    if (replicaof) superdb_set_replica(db, 1);

    g_ctx = sdb_ctx_new(db, password, slowlog_us);
    if (!g_ctx) { fprintf(stderr, "out of memory\n"); superdb_close(db); return 1; }
    if (replicaof) {
        char hostbuf[256];
        const char *colon = strrchr(replicaof, ':');
        int pport;
        if (!colon || colon == replicaof || (size_t)(colon - replicaof) >= sizeof hostbuf) {
            fprintf(stderr, "--replicaof expects HOST:PORT\n");
            return 1;
        }
        memcpy(hostbuf, replicaof, (size_t)(colon - replicaof));
        hostbuf[colon - replicaof] = '\0';
        pport = atoi(colon + 1);
        sdb_ctx_set_readonly(g_ctx, 1);
        sdb_ctx_set_primary(g_ctx, hostbuf, pport);
        repl = sdb_repl_start(g_ctx, hostbuf, pport, primary_auth);
        if (!repl) { fprintf(stderr, "cannot start replication\n"); return 1; }
    }

    ls = sdb_net_listen(host, port);
    if (ls == SDB_SOCK_INVALID) {
        fprintf(stderr, "cannot listen on %s:%d\n", host, port);
        if (repl) sdb_repl_stop(repl);
        sdb_ctx_free(g_ctx);
        superdb_close(db);
        return 1;
    }

    printf("SuperDB %s listening on %s:%d (db: %s)%s\n", SUPERDB_VERSION, host, port, dbpath,
           replicaof ? " [read-only replica]" : "");
    fflush(stdout);

    for (;;) {
        int ready;
        if (sdb_ctx_shutdown_requested(g_ctx)) break;
        ready = sdb_net_poll_readable(ls, 200);
        reap_finished();
        if (ready <= 0) continue;

        {
            sdb_sock_t cfd = accept(ls, NULL, NULL);
            conn_t *c;
            int full;
            if (cfd == SDB_SOCK_INVALID) continue;
            sdb_mutex_lock(&reg.mu);
            full = reg.count >= max_clients;
            sdb_mutex_unlock(&reg.mu);
            if (full) {
                send_text(cfd, "-ERR max number of clients reached\r\n");
                sdb_net_close(cfd);
                continue;
            }
            c = (conn_t*)calloc(1, sizeof *c);
            if (!c) { sdb_net_close(cfd); continue; }
            c->fd = cfd;
            c->ctx = g_ctx;
            sdb_session_init(&c->sess, g_ctx);
            sdb_ctx_conn_opened(g_ctx);
            sdb_mutex_lock(&reg.mu);
            c->next = reg.head;
            reg.head = c;
            reg.count++;
            sdb_mutex_unlock(&reg.mu);
            if (sdb_thread_start(&c->th, conn_main, c) != 0) {
                /* No thread will ever run for c: unregister and free it here. */
                conn_t **pp;
                sdb_mutex_lock(&reg.mu);
                for (pp = &reg.head; *pp; pp = &(*pp)->next) {
                    if (*pp == c) { *pp = c->next; reg.count--; break; }
                }
                sdb_mutex_unlock(&reg.mu);
                sdb_net_close(cfd);
                sdb_ctx_conn_closed(g_ctx);
                sdb_session_free(&c->sess);
                free(c);
            }
        }
    }

    /* Shutdown: wake every connection thread, wait for them, then tear down. */
    sdb_mutex_lock(&reg.mu);
    {
        conn_t *c;
        for (c = reg.head; c; c = c->next) sdb_net_shutdown(c->fd);
    }
    sdb_mutex_unlock(&reg.mu);
    {
        conn_t *c = reg.head;
        while (c) {
            conn_t *nx = c->next;
            sdb_thread_join(&c->th);
            sdb_session_free(&c->sess);
            free(c);
            c = nx;
        }
        reg.head = NULL;
        reg.count = 0;
    }

    if (repl) sdb_repl_stop(repl);
    sdb_net_close(ls);
    sdb_ctx_free(g_ctx);
    g_ctx = NULL;
    superdb_close(db);
    sdb_mutex_destroy(&reg.mu);
    sdb_net_cleanup();
    printf("bye.\n");
    return 0;
}
