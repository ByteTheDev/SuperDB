# `proto/` — wire protocol (no db, no sockets)

RESP-subset framing plus inline `telnet` parsing, and the command
dispatch table that turns `argv` into RESP replies.

| File | Owns |
|---|---|
| `superdb_resp.c/.h` | Reply builders (`+`/`-`/`:`/`$`/`*`) + request parsers (RESP + inline). Pure functions — has its own dependency-free unit test (`tests/test_resp.c`) |
| `superdb_cmd.c/.h` | `sdb_execute()`: command dispatch (`argv` → `superdb_*` → reply). Stateless apart from the passed-in db handle |

Rules: `superdb_resp` never touches the db; `superdb_cmd` may copy keys
(NUL-safety) but must never retain pointers into request buffers.
To add a command, see `docs/ARCHITECTURE.md`.
