/* sdb_net — TCP helpers shared by the server and the replica client.
 * Thin wrappers over POSIX sockets and Winsock. Private to SuperDB tools. */
#ifndef SDB_NET_H
#define SDB_NET_H

#include <stddef.h>

#ifdef _WIN32
#  ifndef WIN32_LEAN_AND_MEAN
#  define WIN32_LEAN_AND_MEAN
#  endif
#  include <winsock2.h>
#  include <ws2tcpip.h>
typedef SOCKET sdb_sock_t;
#  define SDB_SOCK_INVALID INVALID_SOCKET
#else
typedef int sdb_sock_t;
#  define SDB_SOCK_INVALID (-1)
#endif

int  sdb_net_init(void);     /* 0 = ok (WSAStartup on Windows) */
void sdb_net_cleanup(void);

/* Listening socket bound to host:port ("0.0.0.0" = all interfaces). */
sdb_sock_t sdb_net_listen(const char *host, int port);
/* Client connection to host:port (name or address). */
sdb_sock_t sdb_net_connect(const char *host, int port);
/* 1 = readable (or peer closed), 0 = timed out, -1 = error. */
int sdb_net_poll_readable(sdb_sock_t fd, int timeout_ms);
int  sdb_net_send_all(sdb_sock_t fd, const void *p, size_t n);  /* 0 = ok */
long sdb_net_recv(sdb_sock_t fd, void *p, size_t n);            /* >0 bytes, 0 closed, <0 error */
void sdb_net_shutdown(sdb_sock_t fd);  /* wakes a thread blocked in recv/accept */
void sdb_net_close(sdb_sock_t fd);

#endif
