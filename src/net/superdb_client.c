/* superdb_client — implementation. See superdb_client.h.
 * Moved verbatim out of the old monolithic cli.c. */
#ifdef _WIN32
#  ifndef WIN32_LEAN_AND_MEAN
#  define WIN32_LEAN_AND_MEAN
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
#  include <unistd.h>
#  include <errno.h>
typedef int sock_t;
#  define SOCK_INVALID (-1)
#  define sock_close close
#endif

#include "superdb_client.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <stdint.h>

static sock_t g_fd = SOCK_INVALID;

static int send_all(const char *p, size_t n) {
    size_t s = 0;
    while (s < n) {
#ifdef _WIN32
        int r = send(g_fd, p + s, (int)(n - s), 0);
#else
        ssize_t r = send(g_fd, p + s, n - s, 0);
#endif
        if (r <= 0) return -1;
        s += (size_t)r;
    }
    return 0;
}

/* Read one full RESP reply. Returns malloc'd buffer with raw bytes + len.
 * Parses incrementally so binary-safe bulk strings work. */
static int read_reply(char **out, size_t *outlen) {
    size_t cap = 4096, len = 0;
    char *b = (char*)malloc(cap);
    if (!b) return -1;

    /* helper: ensure bytes */
    for (;;) {
        /* try parse */
        if (len > 0) {
            char t = b[0];
            if (t == '+' || t == '-' || t == ':') {
                char *e = memchr(b, '\n', len);
                if (e) {
                    size_t n = (size_t)(e - b) + 1;
                    *out = b; *outlen = n;
                    return 0;
                }
            } else if (t == '$') {
                char *e = memchr(b, '\n', len);
                if (e) {
                    long bl = strtol(b + 1, NULL, 10);
                    size_t hdr = (size_t)(e - b) + 1;
                    if (bl < 0) { *out = b; *outlen = hdr; return 0; }
                    if (len >= hdr + (size_t)bl + 2) {
                        *out = b; *outlen = hdr + (size_t)bl + 2;
                        return 0;
                    }
                }
            } else if (t == '*') {
                /* parse array of bulk/simple: walk elements */
                long ne = 0;
                char *e = memchr(b, '\n', len);
                size_t pos;
                long k;
                if (!e) goto need_more;
                ne = strtol(b + 1, NULL, 10);
                if (ne < 0) { *out = b; *outlen = (size_t)(e - b) + 1; return 0; }
                pos = (size_t)(e - b) + 1;
                for (k = 0; k < ne; k++) {
                    char *e2;
                    if (pos >= len) goto need_more;
                    if (b[pos] == '$') {
                        long bl;
                        e2 = memchr(b + pos, '\n', len - pos);
                        if (!e2) goto need_more;
                        bl = strtol(b + pos + 1, NULL, 10);
                        pos = (size_t)(e2 - b) + 1;
                        if (bl < 0) continue;
                        if (len < pos + (size_t)bl + 2) goto need_more;
                        pos += (size_t)bl + 2;
                    } else if (b[pos] == '+' || b[pos] == '-' || b[pos] == ':') {
                        e2 = memchr(b + pos, '\n', len - pos);
                        if (!e2) goto need_more;
                        pos = (size_t)(e2 - b) + 1;
                    } else goto need_more;
                }
                *out = b; *outlen = pos;
                return 0;
            } else {
                fprintf(stderr, "protocol error: bad byte 0x%02x\n", (unsigned char)t);
                free(b);
                return -1;
            }
        }
need_more:;
        if (len + 4096 + 1 > cap) {
            cap *= 2;
            if (cap > 32*1024*1024) { free(b); return -1; }
            {
                char *nb = (char*)realloc(b, cap);
                if (!nb) { free(b); return -1; }
                b = nb;
            }
        }
#ifdef _WIN32
        {
            int n = recv(g_fd, b + len, 4096, 0);
            if (n <= 0) { free(b); return -1; }
            len += (size_t)n;
        }
#else
        {
            ssize_t n = recv(g_fd, b + len, 4096, 0);
            if (n <= 0) { free(b); return -1; }
            len += (size_t)n;
        }
#endif
    }
}

/* Pretty-print a raw RESP reply */
static void print_reply(const char *b, size_t n) {
    if (n == 0) return;
    if (b[0] == '+') {
        size_t e = n;
        while (e > 0 && (b[e-1] == '\n' || b[e-1] == '\r')) e--;
        printf("%.*s\n", (int)(e - 1), b + 1);
    } else if (b[0] == '-') {
        size_t e = n;
        while (e > 0 && (b[e-1] == '\n' || b[e-1] == '\r')) e--;
        printf("%.*s\n", (int)e, b);
    } else if (b[0] == ':') {
        size_t e = n;
        while (e > 0 && (b[e-1] == '\n' || b[e-1] == '\r')) e--;
        printf("%.*s\n", (int)(e - 1), b + 1);
    } else if (b[0] == '$') {
        long bl = strtol(b + 1, NULL, 10);
        const char *body;
        if (bl < 0) { printf("(nil)\n"); return; }
        body = strchr(b, '\n') + 1;
        fwrite(body, 1, (size_t)bl, stdout);
        printf("\n");
    } else if (b[0] == '*') {
        long ne = strtol(b + 1, NULL, 10);
        const char *p = strchr(b, '\n') + 1;
        const char *end = b + n;
        long k = 0;
        if (ne < 0) { printf("(nil)\n"); return; }
        if (ne == 0) { printf("(empty)\n"); return; }
        for (k = 0; k < ne && p < end; k++) {
            if (*p == '$') {
                long bl = strtol(p + 1, NULL, 10);
                p = strchr(p, '\n') + 1;
                if (bl < 0) printf("%ld) (nil)\n", k + 1);
                else {
                    printf("%ld) ", k + 1);
                    fwrite(p, 1, (size_t)bl, stdout);
                    printf("\n");
                    p += bl + 2;
                }
            } else if (*p == ':' || *p == '+' || *p == '-') {
                const char *e = strchr(p, '\n');
                printf("%ld) %.*s\n", k + 1, (int)(e - p - 1), p + 1);
                p = e + 1;
            } else break;
        }
    }
}

int sdb_client_command(int argc, char **argv) {
    /* build RESP: *<n>\r\n$<len>\r\n<arg>\r\n... */
    size_t i;
    char hb[64];
    int h = snprintf(hb, sizeof hb, "*%d\r\n", argc);
    char *rep = NULL; size_t replen = 0;
    if (send_all(hb, (size_t)h) != 0) { fprintf(stderr, "send failed\n"); return -1; }
    for (i = 0; i < (size_t)argc; i++) {
        size_t L = strlen(argv[i]);
        h = snprintf(hb, sizeof hb, "$%lu\r\n", (unsigned long)L);
        if (send_all(hb, (size_t)h) != 0) return -1;
        if (send_all(argv[i], L) != 0) return -1;
        if (send_all("\r\n", 2) != 0) return -1;
    }
    if (read_reply(&rep, &replen) != 0) { fprintf(stderr, "no reply\n"); return -1; }
    print_reply(rep, replen);
    free(rep);
    return 0;
}

int sdb_client_connect(const char *host, int port) {
    struct sockaddr_in a;
    memset(&a, 0, sizeof a);
    a.sin_family = AF_INET;
    a.sin_port = htons((unsigned short)port);
    a.sin_addr.s_addr = inet_addr(host);
    if (a.sin_addr.s_addr == (unsigned long)0xFFFFFFFFu) {
        fprintf(stderr, "bad host '%s'\n", host);
        return -1;
    }
    g_fd = socket(AF_INET, SOCK_STREAM, 0);
    if (g_fd == SOCK_INVALID) { perror("socket"); return -1; }
    if (connect(g_fd, (struct sockaddr*)&a, sizeof a) != 0) {
#ifdef _WIN32
        fprintf(stderr, "connect %s:%d failed (%d)\n", host, port, WSAGetLastError());
#else
        perror("connect");
#endif
        sock_close(g_fd);
        g_fd = SOCK_INVALID;
        return -1;
    }
    return 0;
}

void sdb_client_close(void) {
    if (g_fd != SOCK_INVALID) {
        sock_close(g_fd);
        g_fd = SOCK_INVALID;
    }
}
