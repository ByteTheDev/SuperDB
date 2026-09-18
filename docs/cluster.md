# SuperDB Cluster

## Local vs Cluster

```
SuperDB Local
    Application -> SuperDB storage engine

SuperDB Cluster
    Application
        |
        v
    Any SuperDB node
        |
        +---- Range A replicas (today: 1 replica = the node itself)
        +---- Range B replicas (planned: splits move key ranges)
        +---- Range C replicas (planned: Raft replication + failover)
```

Local mode is unchanged: no cluster components initialize, reads/writes never
touch network abstractions, and all existing files, WAL, APIs, and benchmarks
keep working.

## Running a cluster

Single node (also bootstraps `cluster.json` identity in the data dir):

```bash
go run ./cmd/superdb server --mode wal --data ./node1 \
  --cluster-addr 127.0.0.1:7432 --addr 127.0.0.1:7654
```

Join a second node:

```bash
go run ./cmd/superdb server --mode wal --data ./node2 \
  --cluster-addr 127.0.0.1:7433 --addr 127.0.0.1:7655 \
  --join 127.0.0.1:7432
```

Flags: `--cluster-addr` (alias `--listen`) enables cluster mode and sets the
internal listen address; `--advertise` defaults to it; `--join` takes
comma-separated seed addresses. Client SQL traffic stays on `--addr` with the
unchanged length-prefixed JSON protocol.

## What is implemented

- Persistent node ID + cluster ID in `<data-dir>/cluster.json` (atomic write,
  restart preserves identity, corrupt files rejected with structured errors).
- Membership: bootstrap, join, list, suspect-on-timeout (never auto-delete),
  explicit graceful removal, ping replies carry member lists so joins converge.
- Internal transport: length-prefixed JSON, protocol version 1, connection
  reuse pool, request timeouts, context cancellation, ping/metadata/lookup/
  forward RPCs. Separate from the public client protocol.
- Ranges: single full-keyspace range per cluster today; `Range{ID, StartKey,
  EndKey, Replicas, Leader, Generation}` shaped for future splits/moves.
- Routing: every node routes independently (no central router); local ranges
  execute on the embedded engine, remote ranges forward over the transport.
- Replication foundation: `Replicator` interface, `ReplicaState`,
  `WriteConcern`; single-copy local durability now, quorum writes explicitly
  rejected (`no_quorum`) until Raft lands.
- Observability: `Node.Status()` reports cluster/node IDs, uptime, health,
  peers, ranges, request/read/write/forward/error counters, avg latency, WAL/
  snapshot sizes. Dashboards are optional; serving never depends on them.

## What is NOT implemented (no claims)

Raft consensus, quorum writes, automatic failover/leader election, range
splitting/merging/rebalancing, distributed transactions, multi-region
placement, auth/TLS. `WriteConcern{RequiredAcks>1}` returns an explicit error
rather than pretending to replicate.

## Benchmarks (2026-09-18, Windows 386, i5-12400F, 1000 iterations)

| Benchmark | Result |
|---|---|
| Raw local engine SELECT | ~785 ns/op |
| Cluster local Exec (same node) | ~3.1 µs/op (~2.3 µs routing+stats overhead) |
| Routing lookup | ~273 ns/op |
| Metadata snapshot | ~897 ns/op |
| Ping (loopback TCP) | ~132 µs/op |
| Remote forward vs local | ~93 µs vs ~1.9 µs |

Local-mode code paths are untouched; cluster overhead applies only when
`--cluster-addr` is set.

## Remaining work

1. Raft replication (mature library, never custom consensus).
2. Quorum writes + explicit durability acknowledgements.
3. Automatic failover / leader election.
4. Range splitting, merging, moves, rebalancing.
5. Distributed transactions.
6. Multi-region placement.
7. Cluster-aware backups, auth/TLS for internal traffic.
