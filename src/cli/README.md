# `cli/` — user-facing entry points (thin)

| File | Owns |
|---|---|
| `cli.c` | `superdb-cli`: arg parsing + interactive REPL. Transport lives in `net/superdb_client` |
| `superdb_tool.c` | `superdb`: project manager + offline toolkit (`init`, `install`, direct-file `get`/`set`/…, `dump`/`restore`, `serve`/`start`/`stop`/`status`, `assoc`, `doctor`). Links only against `core/` — it never speaks RESP |

Rules: keep `main()`s dumb — parse args, call the right module,
print results. No storage or protocol logic here.
