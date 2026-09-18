# SuperDB

SuperDB is a lightweight, fast SQL table database written in Go. It runs as a standalone TCP server and supports an in-memory engine with configurable durability.

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

WAL and snapshot files use a versioned, zlib-compressed SUPERDB storage format. Older line-based WAL files, plain JSON snapshots, and the previous filenames remain readable. The TCP API still sends and receives normal JSON.

## Supported SQL

`CREATE TABLE`, `INSERT`, `SELECT`, `UPDATE`, `DELETE`, `BEGIN`, `COMMIT`, and `ROLLBACK`.

The v1 query engine intentionally focuses on primary-key equality and simple literal filters.
