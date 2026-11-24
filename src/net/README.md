# `net/` - sockets only (thin transport layer)

| File | Owns |
|---|---|
| `server.c` | TCP accept loop, one connection thread per client (bounded by `--max-clients`), client buffers and socket helpers. Portable: POSIX + Winsock. Never touches the db directly - everything goes through `sdb_dispatch()` |
| `superdb_client.c/.h` | Blocking RESP client transport used by `superdb-cli` (connect, send, read one reply, pretty-print) |
| `superdb_net.c/.h` | Socket helpers shared by the server and the replica client (listen/connect/poll/send/recv) |
| `superdb_repl.c/.h` | Replication client: pulls log bytes from a primary and feeds them into the replica db |

Rules: all wire bytes flow through `proto/`; this layer only moves
them between fds. No command logic here.
