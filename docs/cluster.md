# SuperDB Cluster

## Local vs Cluster

```
SuperDB Local
    Application -> SuperDB storage engine

SuperDB Cluster (v2: fully-replicated Raft group + partitioned serving)
    Application
        |
        v
    Any SuperDB node (writes route to the Raft leader)
        |
        +---- Range 1 replicas (voters, quorum-committed)
        +---- Range 2 replicas (split/move/assign via Raft log)
```

Local mode is unchanged: no cluster components initialize, reads/writes never
touch network abstractions, and all existing files, WAL, APIs, and benchmarks
keep working.

## Running a cluster

Single node (bootstraps `cluster.json` identity plus Raft state in `<data>/raft`):

```bash
go run ./cmd/superdb server --mode wal --data ./node1 \
  --cluster-addr 127.0.0.1:7432 --addr 127.0.0.1:7654
```

Join a second and third node (real joint-consensus voters, not stubs):

```bash
go run ./cmd/superdb server --mode wal --data ./node2 --region east \
  --cluster-addr 127.0.0.1:7433 --addr 127.0.0.1:7655 \
  --join 127.0.0.1:7432
go run ./cmd/superdb server --mode wal --data ./node3 --region west \
  --cluster-addr 127.0.0.1:7434 --addr 127.0.0.1:7656 \
  --join 127.0.0.1:7432
```

Flags: `--cluster-addr` (alias `--listen`) enables cluster mode on one shared
port (a first-byte mux separates internal RPC from Raft traffic);
`--advertise` defaults to it; `--join` takes comma-separated seeds;
`--region` labels the node for placement. Client SQL stays on `--addr` with
the unchanged length-prefixed JSON protocol, plus an optional
`{"sqls": [...], "atomic": true}` batch form.

## What is implemented

- **Real consensus**: `github.com/hashicorp/raft` (Bolt-backed log + stable
  store, file snapshots). No custom consensus algorithm. Single fully-
  replicated Raft group: every voter holds all data; ranges partition
  *serving*, and their directory itself is replicated state.
- **Quorum writes**: every write commits through the Raft log and is
  acknowledged only after a quorum durably stores it. `WriteConcern` above
  the voter count fails loudly (`no_quorum`).
- **Automatic failover**: Raft elections; writes route to the current leader
  with one stale-leader retry; `MoveLeader` transfers leadership outage-free.
- **Restart recovery**: committed-log replay plus snapshots restore engine,
  ranges, and assignment. The genesis range is itself a committed entry, so
  replays never hit an empty directory. Verified by restart tests.
- **Ranges**: split/move/assign proposed as fully-computed post-images with
  generation fencing; stale generations rejected deterministically.
- **Atomic batches**: multi-statement all-or-nothing commits as a single Raft
  entry (clone/apply/swap). Session `BEGIN/COMMIT` is rejected in cluster
  mode with an explicit error pointing at atomic batches.
- **Reads**: local (fast, bounded-stale) by default; `ExecConsistent` offers
  linearizable reads via leader barrier.
- **Placement**: region labels, voter/region distribution in status,
  `Rebalance` converging serving replicas with the Raft configuration,
  `PreferRegionLeader` for quorum-locality.
- **Membership**: bootstrap/join/list, suspicion on timeout (never
  auto-delete), graceful removal via joint-consensus `RemoveServer`.
- **Observability**: `Node.Status()` adds Raft role/leader/term/commit/applied,
  placement, and assignment to the previous counters. Serving never depends
  on dashboards.

## What is NOT implemented (no claims)

Sharded storage (all voters hold all data; ranges partition serving, not
bytes), automatic range splitting by load (manual `SplitRange` only),
cross-shard 2PC (atomic batches are single-entry commits), multi-region
witness/learner auto-tuning, auth/TLS on internal traffic. Linearizable reads
are opt-in; default reads are local.

## Benchmarks (2026-09-18, Windows 386, i5-12400F)

| Benchmark | Result (before → now) |
|---|---|
| Raw local engine SELECT | ~785ns → ~400ns (noise/unchanged code) |
| Cluster local read (SELECT, no Raft) | ~3.1µs → ~0.8µs |
| Raft write commit (INSERT, 1 node) | — → ~7ms (Bolt fsync per commit on this box) |
| Routing lookup | ~273ns → ~268ns |
| Metadata snapshot | ~897ns → ~426ns |
| Ping (loopback TCP) | ~132µs → ~100µs |
| Remote forward vs local read | ~93µs vs ~1.9µs → ~87µs vs ~0.9µs |

Local-mode paths are untouched; cluster overhead applies only with
`--cluster-addr`. Write latency is dominated by per-commit fsync; batching
and async persistence are the known next optimizations.

## Remaining work

1. Sharded storage (per-range Raft groups or partitioned logs).
2. Auto-split by size/load and automatic rebalancing.
3. Cross-shard distributed transactions (2PC over single-entry atomicity).
4. Learner/witness auto-tuning across regions.
5. Internal auth/TLS, bounded backpressure budgets.
6. Write batching / group commit for lower commit latency.
