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

The server writes timestamped snapshots to the backup directory. `recover` validates
the newest snapshot before replacing the active snapshot.

For the production profile, use either form:

```bash
go run ./cmd/superdb server --production --data ./SUPERDB
go run ./cmd/superdb --production --data ./SUPERDB
```

Production defaults to WAL durability, enables TCP keep-alive, and uses larger
socket buffers. Use `--profile standard` or omit the flag for the normal profile;
explicit `--mode` settings still override the production WAL default.

### Developer commands

Run `go run ./cmd/superdb` with no command to open the interactive menu. Every menu action is also available directly:

```bash
go run ./cmd/superdb status --data-dir ./SUPERDB
go run ./cmd/superdb backup ./backups/today --data-dir ./SUPERDB
go run ./cmd/superdb restore ./backups/today --data-dir ./SUPERDB
go run ./cmd/superdb compact --data-dir ./SUPERDB
```

The common flags are `--data-dir` (also available as `--data`), `--addr`, and `--mode`. `status` shows the configured server settings and storage file sizes. `backup` copies the compressed `.spdb` files, `restore` copies them back, and `compact` rewrites a fresh compressed snapshot.

WAL and snapshot files use a versioned, zlib-compressed SUPERDB storage format. Older line-based WAL files, plain JSON snapshots, and the previous filenames remain readable. The TCP API still sends and receives normal JSON.

## Supported SQL

`CREATE TABLE`, `INSERT`, `SELECT`, `UPDATE`, `DELETE`, `BEGIN`, `COMMIT`, and `ROLLBACK`.

The v1 query engine intentionally focuses on primary-key equality and simple literal filters.
