# SuperDB architecture

```
include/superdb.h       PUBLIC API (stable; versioned SUPERDB_VERSION)
src/
  core/                 the embeddable engine (`superdb install` vendors this)
    superdb.c           public API implementation (orchestration only)
    superdb_int.h           private: entry + db structs, lock helpers
    superdb_util.h/.c       time, little-endian codecs, fsync, atomic replace
    superdb_crc.h/.c        CRC32 table + streaming (record integrity)
    superdb_store.h/.c      in-memory hash index (chained, cached hash+len)
    superdb_coll.h/.c       list/hash/set value containers
    superdb_glob.h/.c       glob matching for SCAN
    superdb_thread.h/.c     mutex + thread primitives
    superdb_log.h/.c        append-only log: encode, replay, compaction
  proto/                wire protocol — no db, no sockets
    superdb_resp.h/.c       RESP-subset framing: reply builders + request parsers
    superdb_cmd.h/.c        table-driven commands (auth, MULTI, stats, slowlog)
  net/                  sockets only (thin transport)
    server.c            TCP accept loop, one connection thread per client
    superdb_client.h/.c     blocking client transport (used by cli)
    superdb_net.h/.c        socket helpers shared by server and replica client
    superdb_repl.h/.c       log replication client (primary -> replica)
  cli/                  user-facing entry points (thin)
    cli.c               arg parsing + REPL
    superdb_tool.c      `superdb` project manager + offline toolkit
tests/                  test_basic (CRUD/persist), test_batch (group commit),
                        test_resp (protocol unit tests, no db needed),
                        test_tool.sh (CLI roundtrip)
bench/                  bench_sdb, bench_sqlite, probe_fsync
examples/               example (embed), batch (group commit), demo.sdbc (script)
editors/vscode-superdb/ VS Code extension (.sdb inspector, grammar, commands)
tools/                  OS installers + .sdb file-association files
```

## Data flow

```
client --RESP--> server.c --argv--> superdb_cmd --superdb_*--> superdb.c
                                                              |-- superdb_store (memtable)
                                                              +-- superdb_log   (append log)
```

`server.c` never touches the db directly — everything goes through
`sdb_dispatch()` in the command layer. `superdb_resp` never touches the db at
all, which is why it has fast dependency-free unit tests. The replica client in
`src/net/superdb_repl.*` feeds primary log bytes into the same core.

## Ownership rules

- `superdb.h` is the only public header. Everything under `src/core`,
  `src/proto` and `src/net` is private; don't include it from outside
  `src/`, `tests/` or `bench/`.
- `superdb_int.h` owns the `struct superdb` layout. Only `superdb.c`,
  `superdb_store.c` and `superdb_log.c` may touch struct fields. Everyone else
  goes through the public API or the small `_locked` helpers.
- `_locked` suffix = caller must hold the db mutex.
- On-disk format is owned by `superdb_log.h` (`SDB_MAGIC`, record layout).
  It is SDB2 (since 2.0); SDB1 files are migrated in the replay path. Change it
  only with a version bump + migration.
- `superdb_cmd.c` may copy keys (NUL-safety) but must never retain pointers
  into request buffers.

## Adding a command

1. Implement a handler in `src/proto/superdb_cmd.c` and add its name, arity and
   flags to `cmd_tab`; reply with `sb_*`.
2. Add cases to `tests/test_resp.c` if parsing is involved, `tests/test_cmd.c`
   for dispatch, or the relevant core suite (`test_basic`, `test_types`,
   `test_txn`, `test_repl`, `test_compact`).
3. Document in `docs/PROTOCOL.md` and the `COMMAND`/`HELP` string.

## Adding a core op

1. Index change in `src/core/superdb_store.*`, persistence in `src/core/superdb_log.*`,
   public function in `src/core/superdb.c` + declaration in `include/superdb.h`.
2. Keep the log format backward-compatible (old files must still load).
3. Add tests in `tests/test_basic.c` (and a bench in `bench/` if hot-path).
