# SuperDB architecture

```
include/superdb.h        PUBLIC API (stable; versioned SUPERDB_VERSION)
src/
  sdb_int.h              private: entry + db structs, lock helpers
  sdb_util.h/.c          time, little-endian codecs, fsync, atomic replace
  sdb_crc.h/.c           CRC32 table + streaming (record integrity)
  sdb_store.h/.c         in-memory hash index (chained, cached hash+len)
  sdb_log.h/.c           append-only log: encode, replay, compaction
  superdb.c              public API implementation (orchestration only)
  sdb_resp.h/.c          RESP-subset framing: reply builders + request parsers
  sdb_cmd.h/.c           command dispatch (argv -> db -> RESP reply)
  server.c               TCP sockets + select() event loop (thin)
  sdb_client.h/.c        blocking client transport (used by cli)
  cli.c                  arg parsing + REPL (thin)
tests/                   test_basic (CRUD/persist), test_batch (group commit),
                         test_resp (protocol unit tests, no db needed)
bench/                   bench_sdb, bench_sqlite, probe_fsync
examples/                example (embed), batch (group commit)
```

## Data flow

```
client --RESP--> server.c --argv--> sdb_cmd --superdb_*--> superdb.c
                                                              |-- sdb_store (memtable)
                                                              +-- sdb_log   (append log)
```

`server.c` never touches the db directly — everything goes through
`sdb_execute()`. `sdb_resp` never touches the db at all, which is why
it has fast dependency-free unit tests.

## Ownership rules

- `superdb.h` is the only public header. Everything under `src/sdb_*`
  is private; don't include it from outside `src/`, `tests/` or `bench/`.
- `sdb_int.h` owns the `struct superdb` layout. Only `superdb.c`,
  `sdb_store.c` and `sdb_log.c` may touch struct fields. Everyone else
  goes through the public API or the small `_locked` helpers.
- `_locked` suffix = caller must hold the db mutex.
- On-disk format is owned by `sdb_log.h` (`SDB_MAGIC`, record layout).
  It is stable since 1.0 — change it only with a version bump + migration
  in the replay path.
- `sdb_cmd.c` may copy keys (NUL-safety) but must never retain pointers
  into request buffers.

## Adding a command

1. Implement in `sdb_cmd.c` (`sdb_execute`), reply with `sb_*`.
2. Add cases to `tests/test_resp.c` if parsing is involved, or extend
   `tests/test_basic.c` for core semantics.
3. Document in `docs/PROTOCOL.md` and the `COMMAND`/`HELP` string.

## Adding a core op

1. Index change in `src/sdb_store.*`, persistence in `src/sdb_log.*`,
   public function in `src/superdb.c` + declaration in `include/superdb.h`.
2. Keep the log format backward-compatible (old files must still load).
3. Add tests in `tests/test_basic.c` (and бенч in `bench/` if hot-path).
