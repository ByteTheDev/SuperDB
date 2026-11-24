# SuperDB — super ultra lightweight local-first database

A tiny **key-value store** in **pure C99** with **zero dependencies**.
Local-first: your data lives in a single append-only log file on disk,
with an in-memory hash index. Optional TCP server + CLI included.

```
bin/superdb.exe          project CLI: init/install, offline get/set/dump, serve/start/stop, .sdb assoc
bin/superdb-server.exe   TCP server (select loop, 256 clients)
bin/superdb-cli.exe      one-shot client + REPL
include/superdb.h        embeddable library (core modules in src/core/*)
lib/libsuperdb.a         static lib (util/crc/store/log/api)
```

## Quick start

### Windows (no `make` needed)

```bat
build.bat
bin\superdb-server.exe --db ./data.sdb --port 7381
```

In another terminal:

```bat
bin\superdb-cli.exe SET hello world
bin\superdb-cli.exe GET hello
bin\superdb-cli.exe            :: interactive REPL
```

### Project CLI (`superdb`)

Scaffold a repo, work offline (no server), manage the server lifecycle —
see [`docs/CLI.md`](docs/CLI.md):

```sh
superdb init myapp --db ./data.sdb   # superdb.json + .vscode/ + .gitignore + example
superdb install myapp                # vendor src/core/ into third_party/superdb
superdb set greeting hello           # offline, direct on the .sdb file
superdb get greeting
superdb dump --out backup.json       # JSON backup (values base64, TTLs kept)
superdb start / stop / status        # background server via pid file
superdb doctor                        # binaries, config, db health, assoc
superdb assoc                         # register the .sdb file association
```

Plus a VS Code extension (`.sdb` inspector, `.sdbc` script highlighting,
one-click server commands) — see [`docs/VSCODE.md`](docs/VSCODE.md).

### POSIX (make)

```sh
make
./bin/superdb-server --db ./data.sdb --port 7381
./bin/superdb-cli SET hello world
./bin/superdb-cli GET hello
make test
```

### Embed it (local-first, no server)

```c
#include "superdb.h"

superdb_t *db = superdb_open("./hello.sdb", NULL);
superdb_set_str(db, "greeting", "hello, local-first!");
char *v = superdb_get_str(db, "greeting");  // malloc'd
printf("%s\n", v);
superdb_free(v);
superdb_close(db);
```

See `examples/example.c`. Compile against the static lib (or add all
`src/core/*.c` to your build):

```sh
make lib                       # produces lib/libsuperdb.a
gcc -std=c99 -O2 -Iinclude your_app.c -Llib -lsuperdb -o app [-lpthread]
# Windows: ... -lws2_32 (only needed for server/cli, not the lib)
```

CMake alternative: `cmake -B build && cmake --build build` (see
`CMakeLists.txt`; installs headers, lib and tools via `cmake --install`).

## Commands

| Command | Meaning |
|---|---|
| `PING [msg]` | liveness check |
| `SET key value [EX seconds]` | store (binary-safe over RESP) |
| `GET key` | fetch (`(nil)` if missing) |
| `DEL key [key...]` | delete, returns count |
| `EXISTS key [key...]` | count existing |
| `KEYS [prefix*]` | list keys, `*` = all |
| `EXPIRE key seconds` / `TTL key` / `PTTL key` | expiry |
| `INCR key [by]` / `INCRBY key delta` | integer counter |
| `DBSIZE` | number of keys |
| `INFO` | version + stats |
| `COMPACT` (`SAVE`) | rewrite log with live keys only |
| `FLUSHALL` | delete everything |
| `QUIT` / `SHUTDOWN` | close conn / stop server |

Values are **binary-safe** (can contain `\0`, newlines). Keys are C
strings up to 64 KB (no embedded NUL).

## Wire protocol

RESP subset (Redis-compatible framing), plus inline `telnet` commands:

- Request: `*<n>\r\n$<len>\r\n<bytes>\r\n...` **or** `SET foo bar\n`
- Replies: `+OK`, `-ERR msg`, `:int`, `$<len>\r\n<bytes>\r\n` (`$-1` = nil), `*<n>` arrays

So `nc`/`telnet` works for debugging, and the CLI uses RESP for
binary safety.

## How it works

```
            ┌─────────────┐
  SET ──►   │ hash index  │ ──► GET (O(1), in-memory)
            │ (FNV-1a)    │
            └──────┬──────┘
                   │ append
                   ▼
         ┌───────────────────┐
         │ data.sdb (append  │  record: crc32 | op | expire_at | klen | vlen | key | val
         │  -only log)       │  header: "SDB1"
         └───────────────────┘
```

- **Write:** append record + `fflush` + `fsync` (`sync_writes=1`).
- **Read:** hash lookup; lazy expiry check.
- **Open:** replay log, verify CRC per record, **truncate torn tail**
  (crash mid-write is recovered automatically — verified by test).
- **Compaction:** when dead bytes > threshold (default 4 MB), rewrite
  only live keys to `data.sdb.tmp` + atomic `rename`. Also on demand
  via `COMPACT`.
- **Expiry:** absolute ms timestamp per key, persisted in the log.
  Lazy on read + swept on compact/load.
- **Threading:** library is mutex-guarded; server is single-threaded
  `select()` (no threads, works on Winsock + POSIX).

## Durability knobs

```c
superdb_options_t opts = {
    .sync_writes = 1,          /* 1 = fsync every write (crash-safe), 0 = fast */
    .compact_threshold = 4*1024*1024, /* dead bytes before auto-compact, 0 = off */
    .initial_buckets = 1024,
};
superdb_t *db = superdb_open("./data.sdb", &opts);
```

### Group commit (the fast durable path)

Per-op `fsync` caps every engine at disk latency (~500/s here).
Batch N ops behind **one** `fsync` — crash-safe per batch boundary:

```c
superdb_batch_begin(db);
for (...) superdb_set(db, k, v, n);   /* no fsync inside */
superdb_batch_end(db);                /* exactly one fsync */
superdb_sync(db);                     /* flush + fsync any time */
```

## Speed: SuperDB vs SQLite (same box, 16B key / 100B value)

| Workload | SuperDB | SQLite | Result |
|---|---|---|---|
| Durable SET, 1 fsync/op (`--sync` / autocommit `synchronous=FULL`) | ~510/s (= raw `_commit` floor) | 106/s | **4.8x** (SQLite fsyncs 2-3x per txn) |
| Durable batched SET, 1 fsync/1000 (`--sync --batch 1000` / one txn per 1000, FULL) | **324,000/s** | 70,000/s | **4.6x** |
| Fast SET (`--nosync` / `WAL + synchronous=NORMAL`, batched) | **1,400,000/s** | 205,000/s | **6.8x** |
| GET (hit) | **3.8–4.4M/s** | 18k (FULL) – 306k (WAL) | **12–240x** |
| MIXED 50/50 SET/GET, nosync | 2,160,000/s | — | — |

Reproduce: `bench/bench_sdb --n 100000 [--sync] [--batch 1000]`
vs `bench/bench_sqlite --n 100000 --mode autocommit|batch|wal`
(sources in `bench/`; sqlite3 via MSYS2 `mingw-w64-ucrt-x86_64-sqlite3`).

What made v2 fast (from 3 parallel optimization passes):
in-memory hash/length cached per entry (hash once), table-driven CRC32,
zero-malloc stageless log append with single-`fwrite` fast path, 1 MB
stdio buffering with no per-op `fflush` when `sync=0`, single-lock `INCR`
with stack parse, atomic-replace compaction. On-disk format unchanged —
v1 files load as-is.

## Layout

```
include/superdb.h       public API (CRUD, expiry, counters, group commit)
src/
  core/                 embeddable engine: superdb.c (API), superdb_int (structs),
                        superdb_util/superdb_crc, superdb_store (index), superdb_log (log)
  proto/                wire protocol: superdb_resp (framing) + superdb_cmd (dispatch)
  net/                  sockets: server.c (select loop), superdb_client (transport)
  cli/                  entry points: cli.c (REPL), superdb_tool.c (superdb CLI)
tests/                  test_basic, test_batch, test_resp (protocol unit tests),
                        test_tool.sh (project-CLI roundtrip)
bench/                  bench_sdb, bench_sqlite, probe_fsync
examples/               example (embed), batch (group commit), demo.sdbc (script)
editors/vscode-superdb/ VS Code extension (.sdb inspector, .sdbc grammar)
tools/                  OS installers (install.sh/.bat) + .sdb association files
docs/                   ARCHITECTURE.md, PROTOCOL.md, CLI.md, VSCODE.md
```
See `docs/ARCHITECTURE.md` for the module map and ownership rules,
`docs/PROTOCOL.md` for the wire spec, `CONTRIBUTING.md` for workflow.

## Limits (by design)

- Keys: ≤ 64 KB, no NUL bytes. Values: ≤ 64 MB, arbitrary bytes.
- Single file per DB. No replication, no transactions, no secondary
  indexes — that's what keeps it under 100 KB.
- `select()` server: up to 256 concurrent clients, localhost-oriented.
  Bind `--host 0.0.0.0` only behind a firewall you trust (no auth).

## License

MIT — do whatever you want.
