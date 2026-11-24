/* sdb_net — implementation. See sdb_net.h. */
#if !defined(_WIN32) && !defined(_POSIX_C_SOURCE)
#  define _POSIX_C_SOURCE 200809L
#endif
#include "sdb_net.h"

#include <stdio.h>
#include <string.h>
#include <stdlib.h>

#ifdef _WIN32
#  define SDB_CLOSE closesocket
#else
#  include <sys/types.h>
#  include <sys/socket.h>
#  include <sys/select.h>
#  include <netinet/in.h>
#  include <netinet/tcp.h>
#  include <arpa/inet.h>
#  include <netdb.h>
#  include <unistd.h>
#  include <errno.h>
#  define SDB_CLOSE close
#endif

int sdb_net_init(void) {
#ifdef _WIN32
    WSADATA wd;
    return WSAStartup(MAKEWORD(2, 2), &wd) == 0 ? 0 : -1;
#else
    return 0;
#endif
}

void sdb_net_cleanup(void) {
#ifdef _WIN32
    WSACleanup();
#endif
}

static void set_reuse(sdb_sock_t fd) {
    int one = 1;
    setsockopt(fd, SOL_SOCKET, SO_REUSEADDR, (const char*)&one, sizeof one);
}

sdb_sock_t sdb_net_listen(const char *host, int port) {
    struct sockaddr_in a;
    sdb_sock_t ls = socket(AF_INET, SOCK_STREAM, 0);
    if (ls == SDB_SOCK_INVALID) return SDB_SOCK_INVALID;
    set_reuse(ls);
    memset(&a, 0, sizeof a);
    a.sin_family = AF_INET;
    a.sin_port = htons((unsigned short)port);
    if (strcmp(host, "0.0.0.0") == 0) a.sin_addr.s_addr = htonl(INADDR_ANY);
    else a.sin_addr.s_addr = inet_addr(host);
    if (bind(ls, (struct sockaddr*)&a, sizeof a) != 0 || listen(ls, 128) != 0) {
        SDB_CLOSE(ls);
        return SDB_SOCK_INVALID;
    }
    return ls;
}

sdb_sock_t sdb_net_connect(const char *host, int port) {
    struct addrinfo hints, *res = NULL, *ai;
    char portbuf[16];
    sdb_sock_t fd = SDB_SOCK_INVALID;
    snprintf(portbuf, sizeof portbuf, "%d", port);
    memset(&hints, 0, sizeof hints);
    hints.ai_family = AF_INET;
    hints.ai_socktype = SOCK_STREAM;
    if (getaddrinfo(host, portbuf, &hints, &res) != 0) return SDB_SOCK_INVALID;
    for (ai = res; ai; ai = ai->ai_next) {
        fd = socket(ai->ai_family, ai->ai_socktype, ai->ai_protocol);
        if (fd == SDB_SOCK_INVALID) continue;
        if (connect(fd, ai->ai_addr, (int)ai->ai_addrlen) == 0) break;
        SDB_CLOSE(fd);
        fd = SDB_SOCK_INVALID;
    }
    freeaddrinfo(res);
    if (fd != SDB_SOCK_INVALID) {
        int one = 1;
        setsockopt(fd, IPPROTO_TCP, TCP_NODELAY, (const char*)&one, sizeof one);
    }
    return fd;
}

int sdb_net_poll_readable(sdb_sock_t fd, int timeout_ms) {
    fd_set rfds;
    struct timeval tv;
    int r;
    FD_ZERO(&rfds);
    FD_SET(fd, &rfds);
    tv.tv_sec = timeout_ms / 1000;
    tv.tv_usec = (long)(timeout_ms % 1000) * 1000L;
#ifdef _WIN32
    r = select(0, &rfds, NULL, NULL, &tv);
#else
    r = select(fd + 1, &rfds, NULL, NULL, &tv);
#endif
    if (r < 0) {
#ifdef _WIN32
        return -1;
#else
        return errno == EINTR ? 0 : -1;
#endif
    }
    return r > 0 ? 1 : 0;
}

int sdb_net_send_all(sdb_sock_t fd, const void *p, size_t n) {
    const char *c = (const char*)p;
    size_t sent = 0;
    while (sent < n) {
#ifdef _WIN32
        int r = send(fd, c + sent, (int)(n - sent), 0);
#else
        ssize_t r = send(fd, c + sent, n - sent, 0);
#endif
        if (r <= 0) return -1;
        sent += (size_t)r;
    }
    return 0;
}

long sdb_net_recv(sdb_sock_t fd, void *p, size_t n) {
#ifdef _WIN32
    int r = recv(fd, (char*)p, (int)(n > 65536 ? 65536 : n), 0);
#else
    ssize_t r = recv(fd, p, n, 0);
#endif
    return (long)r;
}

void sdb_net_shutdown(sdb_sock_t fd) {
    if (fd == SDB_SOCK_INVALID) return;
#ifdef _WIN32
    shutdown(fd, SD_BOTH);
#else
    shutdown(fd, SHUT_RDWR);
#endif
}

void sdb_net_close(sdb_sock_t fd) {
    if (fd != SDB_SOCK_INVALID) SDB_CLOSE(fd);
}
