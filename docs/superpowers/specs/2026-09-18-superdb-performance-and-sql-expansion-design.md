# SuperDB Performance and SQL Expansion Design

## Goal

Make SuperDB competitive with SQLite for supported in-memory workloads while
retaining a useful embedded engine and improving the TCP server path. Expand the
SQL surface incrementally without compromising existing WAL, snapshot, backup,
and recovery behavior.

## Scope

The work is intentionally staged:

1. Establish fair embedded and server benchmarks.
2. Add batch execution, transactions, and prepared-query reuse.
3. Optimize the engine hot paths with typed primary-key indexes, cached query
   metadata, and fewer allocations.
4. Expand queries with multi-row inserts, compound predicates, ordering, limits,
   and basic aggregates.
5. Add secondary indexes and limited schema evolution.

Existing SQL behavior and storage formats remain compatible. Unsupported SQL must
return a clear error rather than silently changing data.

## Architecture

`engine.Database` remains the embedded API. It gains explicit batch and
transaction boundaries while retaining `Exec` for compatibility. Query parsing is
split from execution so parsed statements can be reused safely.

Tables retain insertion order for scans, but primary-key lookups use a typed
index. Secondary indexes are explicit table metadata and are updated atomically
with row writes. Query execution uses indexes when predicates match them and falls
back to scans otherwise.

The TCP protocol gains a batch request shape that carries multiple SQL statements
and a transaction-aware connection session. Existing one-request/one-response
clients continue to work. Batch responses preserve request order and identify the
first error without partially hiding earlier results.

WAL writes are grouped at transaction commit. Snapshot and recovery code continues
to use the existing versioned SUPERDB format, with tests covering committed and
rolled-back batches.

## SQL expansion

The first query expansion supports:

- multi-row `INSERT ... VALUES (...), (...)`;
- `WHERE` predicates joined by `AND` and `OR`;
- `ORDER BY <column> [ASC|DESC]`;
- `LIMIT <integer>`;
- `COUNT(*)`, `SUM`, `MIN`, and `MAX` for a single selected expression;
- `CREATE INDEX` on an existing table;
- `ALTER TABLE ... ADD COLUMN` with a NULL default.

The parser will reject malformed or unsupported combinations explicitly. Null
handling and aggregate semantics will be covered by tests before being exposed in
the benchmark.

## Benchmark and acceptance criteria

The benchmark will run the same workload in four modes:

- embedded SuperDB;
- embedded SQLite;
- TCP SuperDB with individual requests;
- TCP SuperDB with batches.

It will report median time, operations per second, and p95 latency over repeated
runs. The initial target is for embedded SuperDB to beat SQLite on the supported
primary-key insert/select workload and for batched TCP SuperDB to substantially
close the current protocol gap. No result will be claimed without fresh test and
benchmark output.

Correctness acceptance requires `go test ./...`, race-safe engine tests for the
new batch/session paths, storage recovery tests, and a clean `go vet ./...` run.

## Failure and compatibility behavior

- A failed transaction does not commit its writes.
- A malformed batch returns a structured error and does not panic or deadlock the
  connection.
- Existing JSON request frames remain valid.
- Existing snapshot, WAL, backup, and recovery files remain readable.
- Index maintenance failures abort the write rather than leaving a stale index.

## Out of scope for this iteration

Joins, query planner cost estimation, concurrent writers with serializable
isolation, replication, and a new on-disk table format are deferred until the
benchmark and correctness foundations are stable.
