# SuperDB Agent Guide

This is the single source of truth for AI agents working on SuperDB. Read it before changing code and update it when commands, storage, protocol, or package boundaries change.

## Project

SuperDB is a lightweight Go SQL database with an in-memory engine, TCP server/client, compressed SUPERDB storage, snapshots, WAL durability, backups, recovery, and benchmarks. It is not currently PostgreSQL-compatible, distributed, authenticated, TLS-secured, or enterprise-ready.

Module: `superdb`. Main programs:

- `cmd/superdb`: server and administration CLI.
- `cmd/superdb-cli`: TCP SQL client.

## Repository map

```
cmd/superdb/main.go              Server/admin command entry point
cmd/superdb-cli/main.go          Client flags and entry point
cmd/superdb-cli/client.go        TCP framing, batching, responses
internal/engine/database.go      Dispatch, locking, clone/replace
internal/engine/types.go         Database, table, column, result types
internal/engine/parser.go        Shared SQL parsing/literals
internal/engine/create.go        CREATE TABLE
internal/engine/schema.go        CREATE INDEX and ALTER/schema changes
internal/engine/insert.go        INSERT
internal/engine/select.go        SELECT, filters, ordering, limits, aggregates
internal/engine/update.go        UPDATE
internal/engine/delete.go        DELETE
internal/engine/cancel.go        Per-statement cancellation checks in scan loops
internal/engine/snapshot.go      JSON snapshot serialization
internal/server/server.go        TCP lifecycle and SQL sessions
internal/server/protocol.go      Length-prefixed JSON protocol
internal/server/limits.go        Limits config + Limiter admission control
internal/server/persistence.go   Snapshot/WAL loading and persistence
internal/server/backups.go       Timestamped backups and recovery
internal/server/cluster.go       Bridge: start cluster node around local engine
internal/cluster/node.go         Node lifecycle, Raft writes/reads, ranges ops, RPC handlers
internal/cluster/raft.go         hashicorp/raft wrapper + commit-tracking store decorator
internal/cluster/fsm.go          Raft log entries (exec/range) and FSM glue
internal/cluster/state.go        Deterministic apply, FSM snapshots, WAL side-copy
internal/cluster/consensus/mux.go Single-port first-byte mux (internal RPC vs Raft)
internal/cluster/identity.go     Persistent node/cluster IDs (cluster.json)
internal/cluster/membership.go   Member directory, regions, suspicion (never auto-delete)
internal/cluster/transport.go    Internal length-prefixed JSON RPC + conn reuse
internal/cluster/ranges.go       Range directory + table assignment + fenced ops
internal/cluster/routing.go      Decentralized table/key -> range/node routing
internal/cluster/replication.go  RaftReplicator/WriteConcern (quorum-enforced)
internal/cluster/stats.go        Atomic observability counters
internal/cluster/errors.go       Structured cluster errors
internal/wire/wire.go            SDB1 framed protocol (magic/version/type/reqID/len + JSON)
internal/remote/config.go        Hosted server config, env vars, limits, constant-time auth
internal/remote/metrics.go       Atomic server observability counters
internal/remote/server.go        Hosted TCP/TLS server, graceful shutdown, cluster routing
internal/remote/session.go       Per-connection session/tx over existing engine APIs
internal/remote/health.go        HTTP /health and /ready probes (no secrets)
connstring.go                    superdb:// URL parsing (root superdb package)
client.go                        First-party Go remote client (root superdb package)
errors.go                        Structured server errors (root superdb package)
docs/hosted.md                   Hosted deployment, protocol, TLS, Docker guide
internal/storage/format.go       SUPERDB1 encoding/decoding and zlib
internal/storage/snapshot.go     Snapshot read/write
internal/storage/wal.go          WAL append/replay/writer
internal/storage/paths.go        Canonical and legacy paths
bench/compare.py                 SuperDB/SQLite workloads
```

Keep engine, server, storage, and CLI responsibilities separate. Add focused files instead of returning to one large file.

## Verification

Run before claiming work is complete:

```bash
gofmt -w <changed-go-files>
go test ./...
go vet ./...
go build ./...
git diff --check
```

For locking, transactions, indexes, or server changes:

```bash
go test -race ./...
```

The benchmark is:

```bash
python bench/compare.py
```

It currently compares SuperDB with SQLite. It does not produce CockroachDB results unless CockroachDB is running and a compatible adapter is added. Never invent benchmark numbers.

## Running SuperDB

No command opens the interactive menu:

```bash
go run ./cmd/superdb
```

Menu options currently start the server, show status, create a backup, restore a backup, or compact a snapshot.

Server examples:

```bash
go run ./cmd/superdb server --mode memory
go run ./cmd/superdb server --mode wal --data-dir ./SUPERDB
go run ./cmd/superdb server --mode snapshot --data-dir ./SUPERDB
go run ./cmd/superdb --production --data-dir ./SUPERDB
go run ./cmd/superdb server --profile production --data-dir ./SUPERDB
```

Production enables WAL when mode is memory, larger TCP buffers, keepalive, and the optimized WAL writer. It does not enable authentication or TLS; do not expose it to an untrusted network.

Administration commands:

```bash
go run ./cmd/superdb status --data-dir ./SUPERDB
go run ./cmd/superdb backup ./backups/today --data-dir ./SUPERDB
go run ./cmd/superdb restore ./backups/today --data-dir ./SUPERDB
go run ./cmd/superdb recover --backup-dir ./BACKUPS --data-dir ./SUPERDB
go run ./cmd/superdb compact --data-dir ./SUPERDB
go run ./cmd/superdb update --check
```

Shared flags:

- `--data-dir DIR`: storage directory, default `./SUPERDB`.
- `--data DIR`: alias for `--data-dir`.
- `--addr HOST:PORT`: TCP address, default `127.0.0.1:7654`.
- `--mode memory|wal|snapshot`.

Server-only flags include `--profile standard|production`, `--production`, `--backup-dir DIR`, `--backup-interval DURATION`, `--cluster-addr` (alias `--listen`), `--advertise`, `--join`, and `--region`. Cluster mode is off unless `--cluster-addr` is set; see `docs/cluster.md`. Cluster mode runs hashicorp/raft (new `go.mod` deps); session transactions are rejected there in favor of atomic batches.

Safety-limit flags on `server` (all default to disabled): `--query-timeout`, `--max-result-rows`, `--max-result-bytes`, `--max-inflight-queries`, `--query-queue-timeout`, `--max-connections`, `--max-batch-statements`, `--max-tx-statements`, `--max-tx-database-rows`, `--max-request-bytes`. `serve` exposes the same set minus connection/batch/request caps (already covered by `MaxConnections` and frame size) plus `SUPERDB_*` env vars; see `docs/hosted.md`.

`status` reports configured values and local snapshot/WAL file sizes. `backup` copies active `.spdb` files. `restore` copies them into the data directory. `recover` validates and restores the newest timestamped snapshot backup. `compact` rewrites a snapshot from the loaded snapshot state.

Client examples:

```bash
go run ./cmd/superdb-cli --addr 127.0.0.1:7654 "CREATE TABLE users (id INT PRIMARY KEY, name TEXT)"
go run ./cmd/superdb-cli --addr 127.0.0.1:7654 "INSERT INTO users VALUES (1, 'Ada')"
go run ./cmd/superdb-cli --addr 127.0.0.1:7654 "SELECT * FROM users WHERE id = 1"
echo "SELECT * FROM users" | go run ./cmd/superdb-cli --addr 127.0.0.1:7654
```

The client sends multiple stdin queries over one connection and prints one JSON response per query. On-disk compression does not change the normal JSON network format.

## SQL behavior

Supported statements include:

```sql
CREATE TABLE users (id INT PRIMARY KEY, name TEXT, score FLOAT, active BOOL);
CREATE INDEX users_name ON users (name);
ALTER TABLE users ADD COLUMN email TEXT;
INSERT INTO users VALUES (1, 'Ada', 10.5, true);
INSERT INTO users VALUES (2, 'Grace', 9.5, false), (3, 'Linus', 8.0, true);
SELECT * FROM users;
SELECT id, name FROM users WHERE score >= 9 ORDER BY score DESC LIMIT 10;
SELECT COUNT(*) FROM users WHERE active = true;
UPDATE users SET active = false WHERE id = 1;
DELETE FROM users WHERE id = 1;
BEGIN;
COMMIT;
ROLLBACK;
```

Types are `INT`, `FLOAT`, `TEXT`, and `BOOL`. JSON decoding can turn numbers into `float64`; preserve numeric conversion behavior when changing snapshots or parsers.

Important behavior:

- Primary keys use direct lookup fast paths.
- Secondary indexes must stay correct after insert, update, delete, clone, and replace.
- WHERE, ordering, limits, aggregates, and logical predicates are only the implemented subset of SQL.
- SELECT clause order is WHERE, ORDER BY, LIMIT; reject empty WHERE clauses and misordered clauses without panicking. Keyword searches must skip quoted literals and identifier substrings.
- Ordered predicates must agree across small-table, large-table, and indexed paths. MIN/MAX compare numeric values numerically; integer sorting preserves int64 precision. COUNT(column) excludes NULL; COUNT(*) counts all matching rows.
- Transactions are session-local clones until COMMIT.
- WAL records mutating SQL, not reads.
- Batches execute and return results in order.

Before adding SQL, inspect existing parser helpers and add tests for valid syntax, invalid syntax, type conversion, indexes, and persistence/replay.

## Network protocol

Every TCP message is:

```
4-byte unsigned big-endian payload length
JSON payload
```

Requests:

```json
{"sql":"SELECT * FROM users"}
{"sqls":["INSERT INTO users VALUES (1, 'Ada')","SELECT * FROM users"]}
```

Responses use the same length prefix and JSON. Successful responses are engine `Result` values; errors contain an `error` string. Requests are limited to 16 MiB (`Limits.MaxRequestBytes` adjusts it). `Limits.MaxResultBytes` replaces oversized responses with an error.

Do not silently change framing, field names, or response ordering. Change client and server together and add protocol tests. TCP_NODELAY, socket buffers, and buffered writes are intentional performance behavior.

## Safety limits

`server.Limits` (internal/server/limits.go) is shared by the local TCP server and the hosted `remote` server. `server.Limiter` enforces `MaxConnections` and `MaxInflightQueries` admission control; over-cap queries wait up to `AcquireTimeout` (default 5s) then fail fast. The engine takes `context.Context` plus `ExecOptions` (`ExecContext`/`ExecWithOptions`): scan loops poll cancellation every 64 rows via `rowTicker`, and `MaxRows` caps emitted rows at `maxRows+1` so over-cap results abort with `ErrResultTooLarge` instead of buffering unbounded output. Session transactions are bounded by `MaxTxStatements` (overflow aborts and frees the clone) and `MaxTxDatabaseRows` (rejects `BEGIN` when `Database.RowCount` exceeds it). Hosted-protocol limit errors map to `TIMEOUT`, `SERVER_BUSY` (retryable), and `RESULT_TOO_LARGE`.

## Storage format

Default layout:

```
SUPERDB/
  snapshot.spdb
  wal.spdb
```

The custom format begins with `SUPERDB1`. The frame contains magic, record kind (`S` snapshot or `W` WAL), version, uncompressed length, stored length, and payload. Snapshot/version-1 frames use zlib. WAL version 2 supports batched WAL payloads. `internal/storage/format.go` is authoritative.

Compatibility is required:

- Continue reading old plain JSON snapshots.
- Continue reading old line-based WAL.
- Continue reading legacy `superdb.snapshot` and `superdb.wal`.
- Reject truncated, oversized, invalid, and trailing data.
- Use atomic storage helpers; do not write database files directly from commands.

Durability modes:

- `memory`: no automatic persistence.
- `wal`: load snapshot, replay WAL, append mutations.
- `snapshot`: load and rewrite snapshot after mutations.

Storage changes require fresh database, restart/replay, corruption, legacy compatibility, backup, and recovery tests.

## Backups and safety

Automatic backups use `--backup-dir` and `--backup-interval`. Recovery validates a candidate before replacing the active snapshot. Never delete user database files in a normal command. Restore, recover, and compact modify durable state and must remain explicit.

## Concurrency

The database and tables use locks. The server uses one goroutine per TCP connection. Transactions clone and replace database state. Do not hold database locks while doing slow network or filesystem work. Run race tests for locking, transactions, indexes, and server changes.

Cluster leaders finish a Raft barrier before Start returns so committed rows are
available after snapshot/log recovery. Genesis creation also waits for replay
before deciding whether an initial range is missing. Follower local reads remain
relaxed; callers requiring linearizable reads must use the consistent-read API.

## Performance

Existing performance features include batched requests, TCP_NODELAY, larger buffers, buffered writes, primary-key lookup, secondary indexes, optimized WAL writing, and a production profile.

Benchmark reports must document machine, OS, Go/database versions, schema, row count, durability mode, protocol/batching, cold/warm cache, setup exclusion, repetitions, and variance. Do not compare numbers from different workloads or claim CockroachDB results without actually running it.

## Change rules

Before editing:

1. Read this guide and inspect relevant tests.
2. Run `git status` and preserve unrelated changes.
3. Identify whether the change affects engine, protocol, storage compatibility, CLI, or multiple areas.

While editing:

- Use focused files and package boundaries.
- Use `apply_patch` for source edits.
- Add tests beside the changed package.
- Preserve storage and protocol compatibility.
- Update README and this guide when behavior changes.
- Do not add enterprise/security claims without implementation and tests.

After editing, run formatting, tests, vet, build, race tests when relevant, benchmark when relevant, and `git diff --check`.

## Known limitations and roadmap

Not yet implemented: authorization, sharded storage, auto-splitting, cross-shard transactions, Prometheus metrics, full SQL grammar, formal migrations, and a valid CockroachDB comparison benchmark. Raft quorum writes/failover, range split/move/assign, atomic batches, and region placement exist (all voters hold all data); see `docs/cluster.md`. Local status is file/configuration status, not a live remote health check. Access keys (local `--auth-key`), username/password auth and TLS (`serve`), and connection/query safety limits exist; see "Safety limits".

Recommended priorities:

1. Local-server TLS and stronger authorization.
2. Checksums and stronger backup verification.
3. Prepared statements and streaming result sets.
4. Proper interactive SQL shell.
5. Metrics and health/readiness endpoints.
6. Reproducible SuperDB/SQLite/PostgreSQL/CockroachDB benchmark harness.

## Handoff format

Report:

```
Summary:
- What changed

Files:
- Important files

Verification:
- Commands and results

Compatibility/risk:
- Storage, protocol, migration, or security concerns

Remaining work:
- Intentionally incomplete items
```

Never claim production readiness without verifying security, recovery, concurrency, tests, and operations.

