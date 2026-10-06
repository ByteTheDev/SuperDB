# SuperDB

SuperDB is a lightweight, fast SQL table database written in Go. It runs as a standalone TCP server and supports an in-memory engine with configurable durability.

## Install

Release packages include both `superdb` (the server and administration commands)
and `superdb-cli` (the SQL client) for Linux, macOS, and Windows on amd64 and
arm64. Download a package from the [GitHub Releases](https://github.com/ByteTheDev/SuperDB/releases)
page, or use the platform installer scripts:

```bash
curl -fsSL https://raw.githubusercontent.com/ByteTheDev/SuperDB/master/install.sh | sh
```

```powershell
irm https://raw.githubusercontent.com/ByteTheDev/SuperDB/master/install.ps1 | iex
```

The scripts download the latest release, verify its SHA-256 checksum, and install
both binaries. Set `SUPERDB_VERSION` to install a specific version. On Unix, use
`--install-dir DIR`; on Windows, use `-InstallDir DIR`.

Verify an installation with:

```bash
superdb --version
superdb-cli --version
```

The client is compiled natively for Linux and macOS as well as Windows and uses
the same TCP/JSON API on every supported platform. It does not require SQLite
or any platform-specific runtime.

Update an existing installation without downloading the package manually:

```bash
superdb update --check
superdb update
```

Use `superdb update --version 0.1.1` to select a specific release. The updater
downloads the matching platform archive, verifies its SHA-256 checksum, and
updates both `superdb` and `superdb-cli`. On Windows, stop the running server
before updating if the updater reports that a binary is locked.

Maintainers create a release by pushing a version tag such as `v0.1.0`:

```bash
git tag v0.1.0
git push origin v0.1.0
```

## Run

```bash
go run ./cmd/superdb server --mode memory
go run ./cmd/superdb-cli --addr 127.0.0.1:7654 "CREATE TABLE users (id INT PRIMARY KEY, name TEXT, active BOOL)"
go run ./cmd/superdb-cli --addr 127.0.0.1:7654 "INSERT INTO users VALUES (1, 'Ada', true)"
go run ./cmd/superdb-cli --addr 127.0.0.1:7654 "SELECT * FROM users WHERE id = 1"
```

Modes are `memory`, `wal`, and `snapshot`. By default, WAL and snapshot data are stored in a `SUPERDB` folder in the project directory:

```text
SUPERDB/snapshot.spdb
SUPERDB/wal.spdb
```

Use `--data` to choose a different storage directory.

Automatic backups and recovery are supported as well:

```bash
go run ./cmd/superdb server --mode wal --data ./SUPERDB --backup-dir ./BACKUPS --backup-interval 15m
go run ./cmd/superdb recover --backup-dir ./BACKUPS --data-dir ./SUPERDB
```

The server writes timestamped snapshots and SHA-256 manifests to the backup
directory. Verify a backup file or the newest backup in a directory before
recovery:

```bash
go run ./cmd/superdb verify-backup ./BACKUPS
```

`recover` validates the newest snapshot and its checksum, when present, before
replacing the active snapshot. Older backups without manifests remain readable
and are checked by decoding the snapshot.

### Hosted mode (remote access)

```bash
superdb serve --host 0.0.0.0 --port 7432 --data ./data
```

This speaks the `SDB1` framed protocol and accepts remote clients via
`superdb://user:password@host:7432/main` (see [docs/hosted.md](docs/hosted.md)
for TLS, auth, Docker, health checks, and the Go client). Local mode above is unchanged.

For the production profile, use either form:

```bash
go run ./cmd/superdb server --production --data ./SUPERDB
go run ./cmd/superdb --production --data ./SUPERDB
```

Production defaults to WAL durability, enables TCP keep-alive, and uses larger
socket buffers. Use `--profile standard` or omit the flag for the normal profile;
explicit `--mode` settings still override the production WAL default.

To require an access key on every client request, start the server with
`--auth-key <key>` (or set `SUPERDB_AUTH_KEY`) and pass the same key to the
client with `--auth-key` (or `SUPERDB_AUTH_KEY`). Without the correct key the
server returns an `unauthorized` error and runs no SQL. When no key is
configured, authentication is disabled for backward compatibility.

Connection and query safety limits are available on both servers:

```bash
go run ./cmd/superdb server --query-timeout 10s --max-result-rows 100000 \
    --max-result-bytes 33554432 --max-inflight-queries 64 --max-connections 512
```

Additional caps: `--query-queue-timeout` (wait for an inflight slot before
rejecting, default 5s), `--max-batch-statements`, `--max-tx-statements`
(abort oversized session transactions), `--max-tx-database-rows` (reject
`BEGIN` when the database is too large to clone safely), and
`--max-request-bytes` (default 16 MiB). Every limit defaults to disabled.
The `serve` command exposes the same limits as flags and `SUPERDB_*`
environment variables; see [docs/hosted.md](docs/hosted.md).

### Developer commands

Run `go run ./cmd/superdb` with no command to open the interactive menu. Every menu action is also available directly:

```bash
go run ./cmd/superdb status --data-dir ./SUPERDB
go run ./cmd/superdb backup ./backups/today --data-dir ./SUPERDB
go run ./cmd/superdb restore ./backups/today --data-dir ./SUPERDB
go run ./cmd/superdb compact --data-dir ./SUPERDB
```

The common flags are `--data-dir` (also available as `--data`), `--addr`, and `--mode`. `status` shows the configured server settings and storage file sizes. `backup` copies the compressed `.spdb` files, `restore` copies them back, and `compact` rewrites a fresh compressed snapshot.

WAL and snapshot files use a versioned, zlib-compressed SUPERDB storage format. Active snapshot and WAL frames carry a CRC-32C checksum of their stored payload, so corruption is detected on every load, replay, and backup verification. Older line-based WAL files, plain JSON snapshots, the previous filenames, and older un-checksummed frames remain readable. The TCP API still sends and receives normal JSON.

## Cluster mode (foundation)

Single process behavior is unchanged. To run nodes that discover each other,
add `--cluster-addr` (internal traffic) alongside the usual `--addr` (client SQL):

```bash
go run ./cmd/superdb server --mode wal --data ./node1 --cluster-addr 127.0.0.1:7432
go run ./cmd/superdb server --mode wal --data ./node2 --cluster-addr 127.0.0.1:7433 --join 127.0.0.1:7432
```

Today this provides Raft quorum writes with automatic failover (hashicorp/raft,
Bolt-backed), persistent node/cluster identity, membership with failure
suspicion (never auto-delete), range split/move/assign, atomic batches, region
placement, and status counters. All voters currently hold all data (ranges
partition serving, not bytes); sharded storage, auto-splitting, and
cross-shard transactions are explicitly **not** implemented yet.
See [docs/cluster.md](docs/cluster.md) for the honest implemented-vs-planned split and benchmark numbers.

## Supported SQL

Schema and DDL: `CREATE TABLE`, `CREATE INDEX`, `ALTER TABLE ... ADD COLUMN`,
`ALTER TABLE ... DROP COLUMN`, `DROP TABLE [IF EXISTS]`, `DROP INDEX name ON t`,
and `TRUNCATE`.

Introspection: `SHOW TABLES`, `SHOW COLUMNS FROM t`, `SHOW INDEX[ES] FROM t`,
`DESCRIBE t` / `DESC t` — all read-only (they never reach the WAL and run as
local reads in cluster mode).

Writes: `INSERT ... VALUES`, `INSERT OR REPLACE ... VALUES` (upsert: existing
keys keep their row position), `INSERT INTO t SELECT ...` (the source result is
materialized before the destination locks, so self-inserts can't deadlock),
`UPDATE`, `DELETE`, `BEGIN`, `COMMIT`, `ROLLBACK`.

Reads: `SELECT` with `DISTINCT`, `WHERE`, `GROUP BY`, `HAVING`, `ORDER BY`,
`LIMIT`, and `OFFSET` — clauses must appear in that order.

WHERE terms support `=`, `!=`, `<>`, `<`, `<=`, `>`, `>=`, `IN (...)`,
`BETWEEN lo AND hi` (inclusive), and `LIKE` (`%` and `_` wildcards, case
sensitive) across SELECT, UPDATE, DELETE, and aggregate queries, combined with
`AND`/`OR`. Ordered comparisons use each column's declared type (INT/FLOAT
numerically, TEXT lexically). Predicates on unknown columns, unknown `ORDER BY`
columns, and aggregates over unknown columns return errors instead of silently
matching nothing. Single quotes inside text literals escape as `''`. Tables may
omit `PRIMARY KEY`; such tables insert rows under synthetic keys and preserve
insertion order.

Aggregates `COUNT`, `SUM`, `MIN`, `MAX`, and `AVG` work bare (`SELECT COUNT(*) ...`)
and grouped (`SELECT dept, COUNT(*) ... GROUP BY dept HAVING ...`). `GROUP BY`
select lists may mix grouping columns and aggregates; `HAVING` evaluates the same
operator set as `WHERE` against group results. Numeric `MIN`/`MAX` and integer
sorting use numeric order without losing integer precision. `COUNT(column)`
excludes NULL values, while `COUNT(*)` includes every matching row; `AVG` and
`SUM` skip NULLs and `AVG` over no non-NULL rows returns NULL. Ordered filters
work with both small tables and the large-table fast path.

Not supported: JOINs, subqueries, expressions in select lists or `SET`,
transactions spanning multiple connections, and `CREATE INDEX` on multiple
columns. A query may name only one table in `FROM`; aliases and comma-separated
tables are rejected rather than silently scanning one table.

Batch requests (`{"sqls": [...]}`) run each statement in order. Adding
`"atomic": true` makes the batch all-or-nothing in both local and cluster
modes: statements run against a private snapshot and publish only if every
one succeeds and no other writer committed meanwhile.

On cluster leader startup, SuperDB waits for committed log replay before
returning, including entries written after the restored snapshot. Follower local
reads retain their existing relaxed consistency; use consistent reads when needed.
