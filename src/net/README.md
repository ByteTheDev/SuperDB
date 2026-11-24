# `net/` — sockets only (thin transport layer)

| File | Owns |
|---|---|
| `server.c` | TCP `select()` loop (up to 256 clients), client buffers, `send_all`. Portable: POSIX + Winsock. Never touches the db directly — everything goes through `sdb_execute()` |
| `superdb_client.c/.h` | Blocking RESP client transport used by `superdb-cli` (connect, send, read one reply, pretty-print) |

Rules: all wire bytes flow through `proto/`; this layer only moves
them between fds. No command logic here.
