# `proto/` - wire protocol (no db, no sockets)

RESP-subset framing plus inline `telnet` parsing, and the table-driven
command dispatcher that turns `argv` into RESP replies.

| File | Owns |
|---|---|
| `superdb_resp.c/.h` | Reply builders (`+`/`-`/`:`/`$`/`*`) + request parsers (RESP + inline). Pure functions - has its own dependency-free unit test (`tests/test_resp.c`) |
| `superdb_cmd.c/.h` | `sdb_dispatch()`: `cmd_tab` handlers, AUTH, read-only replica checks, MULTI queueing, per-command stats, slowlog and SYNCLOG |

Rules: `superdb_resp` never touches the db; `superdb_cmd` may copy keys
(NUL-safety) but must never retain pointers into request buffers.
To add a command, see `docs/ARCHITECTURE.md`.
