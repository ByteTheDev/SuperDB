# SuperDB Developer Documentation

This guide explains how developers should build, run, test, extend, benchmark, and contribute to SuperDB.

## What SuperDB is

SuperDB is a lightweight Go SQL database intended for local development, embedded experiments, and small client/server deployments. It has:

- An in-memory SQL engine.
- A TCP server and JSON client protocol.
- Memory, WAL, and snapshot durability modes.
- Compressed `.spdb` storage files.
- Timestamped backups and recovery.
- A command-line administration tool.
- A benchmark suite.

SuperDB is not currently a distributed database, PostgreSQL replacement, or enterprise-secured service. It does not currently provide authentication, authorization, TLS, replication, or a complete SQL grammar.

## Requirements

Install:

- Go compatible with the module version.
- Python 3 for `bench/compare.py`.
- Git.

Check tools:

```bash
go version
python --version
git --version
```

## Project layout

```
cmd/superdb/                 Server and database administration CLI
cmd/superdb-cli/             TCP SQL client
internal/engine/             SQL engine and database state
internal/server/             TCP protocol, sessions, persistence, backups
internal/storage/            SUPERDB format, snapshots, WAL, atomic files
bench/                       Performance benchmark scripts
docs/                        Design and developer documentation
SUPERDB/                     Default runtime storage directory
```

Keep responsibilities separated:

- Engine code should evaluate SQL and manage database state.
- Server code should manage connections, sessions, transactions, and persistence orchestration.
- Storage code should own file formats and durable file operations.
- CLI code should parse flags, call services, and print user-facing output.
- Tests should live beside the package they validate.

## First build

From the repository root:

```bash
go test ./...
go vet ./...
go build ./...
```

Format Go code before committing:

```gofmt -w cmd internal
```

Check whitespace and patch quality:

```git diff --check
```

## Running a development server

Memory mode is useful for quick experiments:

```bash
go run ./cmd/superdb server --mode memory
```

WAL mode is the normal durable development mode:

```bash
go run ./cmd/superdb server --mode wal --data-dir ./SUPERDB
```

Snapshot mode writes a full snapshot after mutations:

```bash
go run ./cmd/superdb server --mode snapshot --data-dir ./SUPERDB
```

The default listener is:

```
127.0.0.1:7654
```

Keep the default loopback address for local development. Do not expose a development server to the public internet; the current server does not authenticate clients or encrypt traffic.

Production profile:

```bash
go run ./cmd/superdb --production --data-dir ./SUPERDB
```

The production profile currently improves TCP buffering and WAL behavior and selects WAL when the mode is memory. It is not a security profile.

## Using the SQL client

Start the server, then use:

```bash
go run ./cmd/superdb-cli --addr 127.0.0.1:7654 "CREATE TABLE users (id INT PRIMARY KEY, name TEXT)"
go run ./cmd/superdb-cli --addr 127.0.0.1:7654 "INSERT INTO users VALUES (1, 'Ada')"
go run ./cmd/superdb-cli --addr 127.0.0.1:7654 "SELECT * FROM users"
```

The client can read multiple SQL lines from stdin:

```bash
@'
CREATE TABLE users (id INT PRIMARY KEY, name TEXT)
INSERT INTO users VALUES (1, 'Ada')
SELECT * FROM users
'@ | go run ./cmd/superdb-cli --addr 127.0.0.1:7654
```

The client sends requests over one TCP connection and prints one JSON response per query.

## Administration CLI

Open the interactive menu:

```bash
go run ./cmd/superdb
```

Direct commands:

```bash
go run ./cmd/superdb status --data-dir ./SUPERDB
go run ./cmd/superdb backup ./backups/dev --data-dir ./SUPERDB
go run ./cmd/superdb restore ./backups/dev --data-dir ./SUPERDB
go run ./cmd/superdb recover --backup-dir ./BACKUPS --data-dir ./SUPERDB
go run ./cmd/superdb compact --data-dir ./SUPERDB
go run ./cmd/superdb update --check
```

Common flags:

- `--data-dir`: database storage directory.
- `--data`: alias for `--data-dir`.
- `--addr`: server address.
- `--mode`: `memory`, `wal`, or `snapshot`.

## SQL engine development

Supported data types are:

- `INT`
- `FLOAT`
- `TEXT`
- `BOOL`

The implemented SQL surface includes table creation, indexes, altered columns, inserts, selects, updates, deletes, simple predicates, ordering, limits, aggregates, and basic transactions.

Relevant code:

- Add or change dispatch in `internal/engine/database.go`.
- Add SQL parsing helpers in `internal/engine/parser.go`.
- Put statement behavior in its focused file.
- Put schema/index behavior in `internal/engine/schema.go`.
- Add tests in `internal/engine/engine_test.go`.

When adding SQL syntax, test:

1. A normal valid query.
2. Invalid syntax.
3. Missing tables and columns.
4. Type conversion and null-like behavior if supported.
5. Primary-key and secondary-index behavior.
6. Updates and deletes affecting indexes.
7. Snapshot serialization.
8. WAL replay.
9. Transaction behavior if the statement can run in a transaction.

Do not implement SQL behavior only in the network layer. The engine must remain usable and testable without TCP.

## Transactions

Transactions are session-local:

```sql
BEGIN;
INSERT INTO users VALUES (2, 'Grace');
COMMIT;
```

The server clones the database for a transaction and replaces the shared database on commit. Rollback discards the clone.

Changes to cloning or replacement must preserve:

- Tables.
- Columns.
- Primary-key metadata.
- Secondary indexes.
- Row order.
- Type values.

Run race tests whenever transaction or locking code changes.

## Server development

The server accepts TCP connections and starts a session per connection. Each session tracks:

- Shared database.
- Optional transaction clone.
- Transaction SQL for persistence.
- Batch state.
- Optional optimized WAL writer.

Important server files:

- `internal/server/server.go`
- `internal/server/protocol.go`
- `internal/server/persistence.go`
- `internal/server/backups.go`

Do not hold database locks while doing slow network or filesystem work. Preserve connection cleanup, request-size validation, response ordering, and transaction isolation.

## Network protocol

Each message uses:

```
4-byte unsigned big-endian payload length
JSON payload
```

Single query request:

```json
{"sql":"SELECT * FROM users"}
```

Batch request:

```json
{"sqls":["INSERT INTO users VALUES (1, 'Ada')","SELECT * FROM users"]}
```

The response uses the same 4-byte length prefix followed by JSON. The protocol is intentionally normal JSON even though disk storage is compressed.

If the protocol changes:

1. Update `internal/server/protocol.go`.
2. Update `cmd/superdb-cli/client.go`.
3. Add or update server protocol tests.
4. Test malformed, oversized, partial, and multiple frames.
5. Preserve response order.
6. Document the change here and in `AGENTS.md`.

## Storage and durability

Default files:

```
SUPERDB/snapshot.spdb
SUPERDB/wal.spdb
```

The storage package owns the SUPERDB1 framing and zlib compression. Do not write these files directly from CLI or engine code.

Durability modes:

- `memory`: no automatic durable writes.
- `wal`: load snapshot, replay WAL, append mutating SQL.
- `snapshot`: load and rewrite snapshot after mutations.

Legacy compatibility must remain intact:

- Plain JSON snapshots.
- Line-based WAL.
- `superdb.snapshot`.
- `superdb.wal`.

Storage changes require tests for:

- Fresh files.
- Restart and replay.
- Multiple WAL records.
- Corrupt headers.
- Truncated payloads.
- Oversized payloads.
- Trailing bytes.
- Legacy formats.
- Atomic replacement failures.
- Backup and recovery.

Never casually change the storage magic, version, record kind, or header layout. If a format change is needed, design a migration/versioning path first.

## Backup and recovery development

Automatic backups are enabled on the server with:

```bash
go run ./cmd/superdb server --mode wal --backup-dir ./BACKUPS --backup-interval 15m
```

Manual backup:

```bash
go run ./cmd/superdb backup ./BACKUPS/manual --data-dir ./SUPERDB
```

Recovery:

```bash
go run ./cmd/superdb recover --backup-dir ./BACKUPS --data-dir ./SUPERDB
```

Recovery must validate a candidate before replacing active state. Do not delete active data as an intermediate step unless the operation is explicitly designed to be recoverable.

## Testing strategy

Package tests:

```bash
go test ./...
```

Race tests:

```bash
go test -race ./...
```

Focused package tests:

```bash
go test ./internal/engine -run TestName -count=1
go test ./internal/server -run TestName -count=1
go test ./internal/storage -run TestName -count=1
```

Benchmarks:

```bash
go test ./internal/engine -run '^$' -bench . -benchmem
python bench/compare.py
```

The Python benchmark includes bulk inserts, primary-key reads, filtered scans, updates, deletes, mixed workloads, indexed reads, aggregates, multi-row inserts, and durable WAL inserts. It compares against SQLite and includes client/protocol work. Treat it as a development comparison, not a standardized database benchmark.

When reporting performance, include:

- OS and machine.
- Go and database versions.
- Data size and schema.
- Durability mode.
- Client batching.
- Number of iterations.
- Whether setup is excluded.
- Average, variance, and outliers.

## Debugging checklist

For parser failures:

1. Reproduce with a focused engine test.
2. Inspect tokenization and shared parser helpers.
3. Confirm case handling and quoted strings.
4. Test malformed syntax.
5. Run the full engine package tests.

For persistence failures:

1. Inspect the exact data directory.
2. Run status.
3. Check snapshot and WAL headers.
4. Reproduce with a temporary directory.
5. Test fresh startup, restart, replay, and corruption.
6. Do not overwrite the user's real data while debugging.

For server failures:

1. Confirm the listener address.
2. Check whether another process owns the port.
3. Test with the CLI.
4. Test partial frames and multiple requests.
5. Run server tests and race tests.

## Performance engineering

Existing performance behavior includes:

- TCP_NODELAY.
- Larger socket buffers.
- Buffered writes.
- Batched requests.
- Primary-key lookup paths.
- Secondary indexes.
- Optimized WAL writer.
- Production profile.

Optimize only after measuring. Avoid making the protocol or storage format more complicated without a benchmark and compatibility plan. Streaming results, prepared statements, connection pooling, and binary protocol support are future improvements, not assumptions.

## Style and change boundaries

Use standard Go formatting and simple package APIs. Keep command parsing out of engine code. Keep storage format details out of the engine. Prefer explicit errors with context.

Use `apply_patch` for source changes. Preserve unrelated working-tree changes. Do not use destructive commands such as hard resets or broad deletion without explicit authorization.

Update documentation when:

- A command changes.
- A flag changes.
- SQL syntax changes.
- Storage compatibility changes.
- Protocol changes.
- A package responsibility changes.
- Verification commands change.

## Pull request checklist

Before submitting:

```bash
gofmt -w <changed-go-files>
go test ./...
go vet ./...
go build ./...
git diff --check
```

Also:

- Add tests for new behavior.
- Check old storage compatibility.
- Check CLI help/examples.
- Check error paths.
- Check concurrency if relevant.
- Update README, `AGENTS.md`, and this document when needed.
- Explain known limitations.
- Do not claim enterprise readiness without security, recovery, observability, and operational evidence.

## Current limitations

SuperDB currently lacks:

- Authentication.
- Authorization and roles.
- TLS.
- Replication and clustering.
- Prometheus metrics.
- Full SQL grammar.
- Formal schema migration tooling.
- A proper interactive SQL shell.
- A reproducible CockroachDB benchmark adapter.

These limitations should be stated clearly in documentation and release notes.

## Recommended development priorities

1. Authentication, TLS, and connection/query limits.
2. Checksums and stronger backup verification.
3. Prepared statements and streaming results.
4. Proper interactive SQL shell.
5. Health/readiness and metrics endpoints.
6. Reproducible comparisons with SQLite, PostgreSQL, and CockroachDB.

