# Changelog

## v0.4.0 — Hosted access and authentication (2026-09-20)

SuperDB gains remote access and access control while local mode stays
unchanged.

Added:

- Hosted `superdb serve` command speaking the `SDB1` framed protocol over
  TCP/TLS, with env-based config (`SUPERDB_HOST`, `SUPERDB_PORT`, ...),
  username/password auth, connection limits, `/health` and `/ready` probes,
  and cluster routing. See `docs/hosted.md`.
- `superdb://user:password@host:port/db` connection-string parsing
  (`connstring.go`) and a first-party Go remote client (`client.go`).
- Legacy server access key: `server --auth-key` (or `SUPERDB_AUTH_KEY`),
  mirrored by the CLI. Requests without the correct key are rejected before
  any SQL runs; unset means auth disabled (backward compatible).
- Wire framing package (`internal/wire`), hosted server/session/metrics/
  health packages (`internal/remote`), `Dockerfile`, and hosted deployment
  docs.
- Engine hardening: concurrency/transaction/mutation fixes, snapshot
  improvements, and reliability tests.

## v0.2.0 — Raft cluster (2026-09-18)

SuperDB grows from a single-process database into a real distributed cluster
while local mode stays byte-for-byte identical in behavior and performance.

Added:

- Raft consensus via `hashicorp/raft` (Bolt-backed log + stable store, file
  snapshots). No custom consensus algorithm.
- Quorum writes: every cluster write commits through the Raft log and is
  acknowledged only after a quorum durably stores it. Oversized write
  concerns fail loudly instead of pretending.
- Automatic failover: Raft elections, leader-routed writes with stale-leader
  retry, and outage-free `MoveLeader` leadership transfer.
- Restart recovery: committed-log replay plus snapshots; the genesis range is
  itself a committed entry, so replays never hit an empty directory.
- Ranges: split/move/assign as generation-fenced operations, table assignment,
  and `Rebalance` converging serving state with the Raft configuration.
- Atomic batches: all-or-nothing multi-statement commits as a single Raft
  entry, plus an `{"sqls": [...], "atomic": true}` client protocol form.
  Session `BEGIN/COMMIT` is explicitly rejected in cluster mode.
- Reads: local by default, opt-in linearizable reads via leader barrier.
- Multi-region placement: `--region` labels, voter/region distribution in
  status, region-preferred leadership.
- New flags: `--cluster-addr` (alias `--listen`), `--advertise`, `--join`,
  `--region`. Cluster mode is off unless `--cluster-addr` is set.
- CI workflow: `gofmt` check, `vet`, `build`, and `go test -race ./...` on
  every push and pull request.
- 35+ new tests: 3-node joins, failover with write continuity, poison-entry
  convergence, snapshot restore, double restart, corrupt metadata/store
  handling, quorum enforcement, atomic rollback, region placement.

Changed:

- `go.mod` gains `hashicorp/raft`, `hashicorp/raft-boltdb/v2` (+ transitive
  deps). Local mode loads none of this.

Not yet (explicitly out of scope):

- Sharded storage — all voters hold all data; ranges partition serving.
- Auto-splitting, cross-shard transactions, internal auth/TLS.

Benchmarks (Windows 386, i5-12400F): local reads ~0.8µs through a cluster
node, routing ~268ns, loopback ping ~100µs, single-node Raft write commit
~7ms (Bolt fsync-bound; batching is future work). See `docs/cluster.md`.

## v0.1.1

- Checksum-verified `superdb update` self-updating support.

## v0.1.0

- Initial release: in-memory engine, TCP server/client, compressed SUPERDB
  storage, snapshots, WAL durability, backups, recovery, benchmarks,
  cross-platform installers.
