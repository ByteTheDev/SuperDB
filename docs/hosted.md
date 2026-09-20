# Hosted SuperDB (remote mode)

Remote mode turns SuperDB from an embedded/local database into a database
you host once and reach from anywhere. The same engine serves both paths:

```text
                    ┌── Embedded/local API (unchanged)
Application ────────┤
                    │
Remote application ─┼── SuperDB network protocol (SDB1 over TCP)
                    │
                    ▼
              SAME SuperDB engine
                    │
              Storage / Raft
```

Local mode, SQL, persistence, Raft, ranges, transactions, snapshots, and
benchmarks are unchanged. Remote mode is an additional access path.

## Start locally

```bash
superdb serve --host 127.0.0.1 --port 7432 --data ./data
```

Expected output:

```text
SuperDB Server
Listening: 127.0.0.1:7432
Protocol: superdb
Database: ./data
Ready for connections
```

With credentials:

```bash
export SUPERDB_USERNAME=admin
export SUPERDB_PASSWORD='...'
superdb serve --host 0.0.0.0 --port 7432 --data ./data
```

## Connect

```text
superdb://user:password@127.0.0.1:7432/main
```

Hosted example:

```text
superdb://user:password@my-superdb-host.example:7432/main?tls=true
```

Behind a TCP proxy (Railway and friends give you an arbitrary host/port):

```text
superdb://user:password@abc.proxy.rlwy.net:18432/mydb?tls=true
```

The hostname and port are never hardcoded; any reachable TCP address works,
including Docker networks, VMs, localhost, IPv6 (`superdb://user@[::1]:7432/main`),
and custom domains.

## Go

```go
import "superdb"

db, err := superdb.Connect(os.Getenv("SUPERDB_URL"))
if err != nil {
    log.Fatal(err)
}
defer db.Close()

rows, err := db.Query(ctx, "SELECT * FROM users WHERE id = ?", 42)
res, err := db.Exec(ctx, "INSERT INTO users (name) VALUES (?)", "Antonio")
```

Params travel separately from SQL and are bound server-side with escaping;
the client never interpolates values into SQL strings. Writes are never
retried automatically: an interrupted write has unknown commit state.

## TLS

```bash
superdb serve --host 0.0.0.0 --port 7432 --data ./data \
  --tls-cert ./cert.pem --tls-key ./key.pem
```

Clients opt in with `?tls=true` (certificate verification ON by default).
Use `?tls=insecure` only for localhost/development. The server also works
behind a cloud TLS/TCP proxy: terminate TLS at the proxy and run SuperDB
in plaintext behind it.

## Authentication

Set `SUPERDB_USERNAME` / `SUPERDB_PASSWORD` (or `--username/--password`).
Comparisons are constant-time and passwords are never logged. AUTH happens
before any QUERY/EXEC/BEGIN/COMMIT/ROLLBACK; after 5 failures the
connection is closed. The auth payload has a `mechanism` field
(`password` today, `token` reserved) so API keys can be added later.

Without credentials the server still starts (convenient for localhost dev)
but logs that auth is disabled; set credentials in production.

## Configuration

Flags override environment:

| Flag | Env | Default |
|---|---|---|
| `--host` | `SUPERDB_HOST` | `127.0.0.1` |
| `--port` | `SUPERDB_PORT` / `PORT` | `7432` |
| `--data` | `SUPERDB_DATA_DIR` | `./data` |
| `--mode` | `SUPERDB_MODE` | `wal` |
| `--username` | `SUPERDB_USERNAME` | "" |
| `--password` | `SUPERDB_PASSWORD` | "" |
| `--tls-cert` | `SUPERDB_TLS_CERT` | "" |
| `--tls-key` | `SUPERDB_TLS_KEY` | "" |
| `--max-connections` | `SUPERDB_MAX_CONNECTIONS` | `128` |
| `--health-addr` | `SUPERDB_HEALTH_ADDR` | "" (disabled) |

`PORT` is respected for Railway/Fly/Render-style platforms.

## Docker

```bash
docker build -t superdb .
docker run -p 7432:7432 \
  -v superdb-data:/data \
  -e SUPERDB_USERNAME=admin \
  -e SUPERDB_PASSWORD=... \
  superdb
```

Data lives in `/data` (a `VOLUME`), suitable for a persistent volume.
Durable state is never confined to ephemeral container storage.

## Health checks

Run a separate HTTP probe for platforms that need it:

```bash
superdb serve --health-addr :8080 ...
```

- `GET /health` → process alive (`{"status":"ok"}`)
- `GET /ready` → ready for DB traffic (`{"status":"ready"}`)

These never expose secrets or data.

## Protocol (SDB1)

Framed binary protocol, JSON payloads (language-independent, no `gob`).
All integers big-endian. Max frame 16 MiB, max query 4 MiB.

```text
0..3   magic "SDB1"
4..5   version uint16 (1)
6      message type uint8 (HELLO=1 AUTH=2 PING=3 QUERY=4 EXEC=5
                          BEGIN=6 COMMIT=7 ROLLBACK=8 CLOSE=9 RESPONSE=0x80)
7      flags uint8 (reserved, 0)
8..15  request_id uint64
16..19 payload length uint32
20..   JSON payload
```

Responses echo `request_id` so clients can pipeline/multiplex; per-connection
writes are serialized. Unknown versions get a protocol error, never
undefined behavior. Error codes: `AUTH_FAILED AUTH_REQUIRED
INVALID_REQUEST QUERY_ERROR NOT_FOUND NOT_LEADER TIMEOUT UNSUPPORTED
INTERNAL_ERROR PROTOCOL_ERROR`. Reads/writes route through Raft when the
node is clustered (writes require the leader; followers return `NOT_LEADER`
with `retryable:true` instead of fake-acking before quorum).

## Local vs remote

- Local (`server` command, legacy JSON on `--addr`): unchanged.
- Remote (`serve` command, SDB1 on `--host/--port`): auth, TLS, framing,
  multiplexing, health, metrics, connection strings, Go client.
- Both call the same `engine.Database` + Raft paths and the same
  WAL/snapshot persistence.
