# `core/` - the embeddable engine

Zero-dependency key-value store. This is the only module you need to
embed SuperDB in your own program (`superdb install` vendors exactly
these files).

| File | Owns |
|---|---|
| `superdb.c` | Public API (`include/superdb.h`): strings, lists, hashes, sets, expiry, scan, transactions, replication primitives, maintenance |
| `superdb_int.h` | Private `struct superdb` layout + lock helpers. Only core files may touch fields |
| `superdb_store.c/.h` | In-memory hash index (chained, cached hash+length) |
| `superdb_coll.c/.h` | List/hash/set value containers used by the typed API |
| `superdb_glob.c/.h` | Glob matching (`*`, `?`, ranges) for SCAN |
| `superdb_thread.c/.h` | Mutex + thread primitives (portable POSIX/Win32 wrappers) |
| `superdb_log.c/.h` | Append-only log: encode, replay, torn-tail recovery, compaction. Owns the on-disk format (`SDB2`, with SDB1 migration) |
| `superdb_util.c/.h` | Time, little-endian codecs, fsync, atomic replace |
| `superdb_crc.c/.h` | Table-driven CRC32 (record integrity) |

Rules: keep the log format backward-compatible (old files must load);
core never touches sockets or the CLI.
