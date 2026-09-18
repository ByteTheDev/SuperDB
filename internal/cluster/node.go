package cluster

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"superdb/internal/engine"
	"superdb/internal/storage"
)

// Config controls one cluster node.
type Config struct {
	DataDir           string
	ListenAddr        string
	AdvertiseAddr     string
	JoinAddrs         []string
	HeartbeatInterval time.Duration
	PingTimeout       time.Duration
	SuspectAfter      time.Duration
}

// Health describes node liveness.
type Health string

const (
	HealthHealthy  Health = "healthy"
	HealthDegraded Health = "degraded"
	HealthShutdown Health = "shutdown"
)

// Node is one SuperDB cluster member: identity, membership, ranges,
// transport, routing, replication foundation, and observability.
type Node struct {
	cfg      Config
	identity Identity
	db       *engine.Database
	members  *Membership
	ranges   *RangeStore
	router   *Router
	stats    *Stats
	xport    *Transport
	repl     *LocalReplicator

	mu       sync.RWMutex
	listener net.Listener
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	health   Health
	started  time.Time
	applied  uint64
}

func fillDefaults(cfg Config) Config {
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = 500 * time.Millisecond
	}
	if cfg.PingTimeout <= 0 {
		cfg.PingTimeout = 2 * time.Second
	}
	if cfg.SuspectAfter <= 0 {
		cfg.SuspectAfter = 5 * time.Second
	}
	if cfg.AdvertiseAddr == "" {
		cfg.AdvertiseAddr = cfg.ListenAddr
	}
	return cfg
}

// New creates a node around an existing local storage engine. The engine
// stays the source of truth; clustering adds routing/membership on top.
func New(cfg Config, db *engine.Database) *Node {
	cfg = fillDefaults(cfg)
	n := &Node{cfg: cfg, db: db, stats: NewStats(), xport: NewTransport(), ranges: NewRangeStore(), health: HealthHealthy}
	n.repl = NewLocalReplicator(
		func(rangeID uint64, sql string) error {
			if _, err := n.db.Exec(sql); err != nil {
				return NewError(CodeTransport, err.Error())
			}
			n.mu.Lock()
			n.applied++
			n.mu.Unlock()
			return nil
		},
		func(rangeID uint64) (ReplicaState, error) {
			rs := n.ranges.All()
			for _, r := range rs {
				if r.ID == rangeID {
					role := RoleFollower
					if r.Leader == n.identity.NodeID {
						role = RoleLeader
					}
					n.mu.RLock()
					applied := n.applied
					n.mu.RUnlock()
					return ReplicaState{RangeID: r.ID, Role: role, Leader: r.Leader, Generation: r.Generation, Applied: applied, Committed: applied}, nil
				}
			}
			return ReplicaState{}, NewError(CodeRangeNotFound, "range not found")
		},
	)
	return n
}

// DB exposes the local engine for local-mode parity and tests.
func (n *Node) DB() *engine.Database { return n.db }

// AdvertiseAddr returns the address this node tells peers to reach it.
func (n *Node) AdvertiseAddr() string { return n.cfg.AdvertiseAddr }

// Forward executes SQL on the remote node at addr over internal transport.
func (n *Node) Forward(ctx context.Context, addr, sql string) (engine.Result, error) {
	var res engine.Result
	cctx, cancel := context.WithTimeout(ctx, n.cfg.PingTimeout)
	defer cancel()
	if err := n.xport.Call(cctx, addr, KindForward, forwardPayload{SQL: sql}, &res); err != nil {
		return engine.Result{}, err
	}
	return res, nil
}

// Identity returns persistent node/cluster identity.
func (n *Node) Identity() Identity { return n.identity }

// Members exposes the membership directory.
func (n *Node) Members() *Membership { return n.members }

// Ranges exposes the range directory.
func (n *Node) Ranges() *RangeStore { return n.ranges }

// Router exposes the routing layer.
func (n *Node) Router() *Router { return n.router }

// StatsSnapshot returns observability counters.
func (n *Node) StatsSnapshot() Snapshot { return n.stats.Load() }

// Start bootstraps or joins, then serves internal traffic.
func (n *Node) Start(ctx context.Context) error {
	if n.cfg.ListenAddr == "" {
		return NewError(CodeInvalidArgument, "listen address required")
	}
	id, ok, err := LoadIdentity(n.cfg.DataDir)
	if err != nil {
		return err
	}
	if !ok {
		id = Identity{NodeID: newID(), ClusterID: newID(), Advertise: n.cfg.AdvertiseAddr}
	}
	if id.Advertise == "" {
		id.Advertise = n.cfg.AdvertiseAddr
	}
	n.identity = id
	n.members = NewMembership(id.NodeID, n.cfg.AdvertiseAddr)

	ln, err := net.Listen("tcp", n.cfg.ListenAddr)
	if err != nil {
		return NewError(CodeTransport, "listen: "+err.Error())
	}
	n.listener = ln
	n.registerHandlers()

	nctx, cancel := context.WithCancel(context.Background())
	n.ctx = nctx
	n.cancel = cancel
	n.started = time.Now()
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		_ = n.xport.Serve(nctx, ln)
	}()

	if len(n.cfg.JoinAddrs) > 0 {
		if jerr := n.join(ctx); jerr != nil {
			_ = ln.Close()
			return jerr
		}
	}
	n.ranges.EnsureSingleRange(n.identity.NodeID)
	n.router = NewRouter(n.ranges, n.members, n.identity.NodeID)

	if serr := StoreIdentity(n.cfg.DataDir, n.identity); serr != nil {
		n.Shutdown()
		return serr
	}
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		n.heartbeatLoop(nctx)
	}()
	return nil
}

func (n *Node) join(ctx context.Context) error {
	var lastErr error
	for _, seed := range n.cfg.JoinAddrs {
		var meta metadataPayload
		cctx, cancel := context.WithTimeout(ctx, n.cfg.PingTimeout)
		err := n.xport.Call(cctx, seed, KindMetadata, nil, &meta)
		cancel()
		if err != nil {
			lastErr = err
			continue
		}
		if meta.ClusterID == "" {
			lastErr = NewError(CodeClusterMismatch, "seed returned no cluster id")
			continue
		}
		// Adopt the existing cluster identity; our node ID stays unique.
		n.identity.ClusterID = meta.ClusterID
		n.ranges.ReplaceBulk(meta.Ranges)
		n.members.MergeBulk(meta.Members)
		// Announce ourselves so every node sees the membership.
		jm := joinPayload{Member: Member{ID: n.identity.NodeID, Addr: n.cfg.AdvertiseAddr, State: StateAlive, LastSeen: time.Now(), Version: 1}, ClusterID: meta.ClusterID}
		cctx2, cancel2 := context.WithTimeout(ctx, n.cfg.PingTimeout)
		jerr := n.xport.Call(cctx2, seed, KindJoin, jm, nil)
		cancel2()
		if jerr != nil {
			lastErr = jerr
			continue
		}
		n.members.Upsert(n.identity.NodeID, n.cfg.AdvertiseAddr, StateAlive, 1)
		return nil
	}
	if lastErr == nil {
		lastErr = NewError(CodeUnavailable, "no join addresses")
	}
	return lastErr
}

// Shutdown leaves gracefully and stops all node goroutines.
func (n *Node) Shutdown() {
	if !n.markShutdown() {
		return
	}
	n.announceLeave()
	n.stop()
}

// Kill stops the node without a leave announcement, simulating a crash.
// Peers must observe suspect state, never instant removal.
func (n *Node) Kill() {
	if !n.markShutdown() {
		return
	}
	n.stop()
}

func (n *Node) markShutdown() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.health == HealthShutdown {
		return false
	}
	n.health = HealthShutdown
	return true
}

func (n *Node) stop() {
	if n.cancel != nil {
		n.cancel()
	}
	if n.listener != nil {
		_ = n.listener.Close()
	}
	n.xport.Close()
	n.wg.Wait()
}

func (n *Node) announceLeave() {
	// Best-effort leave: tell each known peer; ignore all errors.
	if n.members == nil {
		return
	}
	for _, m := range n.members.List() {
		if m.ID == n.identity.NodeID {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = n.xport.Call(ctx, m.Addr, KindLeave, leavePayload{ID: n.identity.NodeID}, nil)
		cancel()
	}
}

// Exec routes SQL: local ranges execute on the embedded engine, remote
// ranges forward to the owning node. Local mode has zero network overhead.
func (n *Node) Exec(ctx context.Context, sql string) (engine.Result, error) {
	start := time.Now()
	table := ExtractTable(sql)
	route, rerr := n.router.RouteTable(table)
	if rerr != nil {
		n.stats.Observe(sql, time.Since(start), false, true)
		return engine.Result{}, rerr
	}
	if route.Local {
		res, err := n.db.Exec(sql)
		n.stats.Observe(sql, time.Since(start), false, err != nil)
		return res, err
	}
	var res engine.Result
	cctx, cancel := context.WithTimeout(ctx, n.cfg.PingTimeout)
	defer cancel()
	if err := n.xport.Call(cctx, route.Addr, KindForward, forwardPayload{SQL: sql}, &res); err != nil {
		n.stats.Observe(sql, time.Since(start), true, true)
		return engine.Result{}, err
	}
	n.stats.Observe(sql, time.Since(start), true, false)
	return res, nil
}

// Ping checks a peer with timeout and cancellation.
func (n *Node) Ping(ctx context.Context, addr string) (PingReply, error) {
	var out PingReply
	cctx, cancel := context.WithTimeout(ctx, n.cfg.PingTimeout)
	defer cancel()
	err := n.xport.Call(cctx, addr, KindPing, pingPayload{From: n.identity.NodeID}, &out)
	n.stats.ObservePing(err != nil)
	if err == nil {
		n.members.MergeBulk(out.Members)
		n.members.MarkAlive(out.NodeID)
		return out, nil
	}
	return PingReply{}, err
}

func (n *Node) heartbeatLoop(ctx context.Context) {
	t := time.NewTicker(n.cfg.HeartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			for _, m := range n.members.List() {
				if m.ID == n.identity.NodeID {
					continue
				}
				var out PingReply
				cctx, cancel := context.WithTimeout(ctx, n.cfg.PingTimeout)
				err := n.xport.Call(cctx, m.Addr, KindPing, pingPayload{From: n.identity.NodeID}, &out)
				cancel()
				n.stats.ObservePing(err != nil)
				if err != nil {
					n.members.MarkSuspect(m.ID)
					continue
				}
				if out.ClusterID != "" && out.ClusterID != n.identity.ClusterID {
					continue
				}
				n.members.MergeBulk(out.Members)
				n.members.MarkAlive(m.ID)
				n.members.Upsert(out.NodeID, m.Addr, StateAlive, m.Version)
			}
			n.members.SweepSuspects(n.cfg.SuspectAfter, now)
		}
	}
}

// Status is the observability snapshot for dashboards. It never gates
// serving: the database keeps working when no dashboard is watching.
func (n *Node) Status() Status {
	n.mu.RLock()
	health := n.health
	n.mu.RUnlock()
	members := n.members.List()
	ranges := n.ranges.All()
	snap := n.stats.Load()
	return Status{
		ClusterID: n.identity.ClusterID,
		NodeID:    n.identity.NodeID,
		Addr:      n.cfg.AdvertiseAddr,
		Health:    health,
		UptimeSec: int64(time.Since(n.started).Seconds()),
		Peers:     members,
		Ranges:    ranges,
		Stats:     snap,
		Storage:   n.storageStats(),
	}
}

type storageStats struct {
	SnapshotBytes int64  `json:"snapshot_bytes"`
	WALBytes      int64  `json:"wal_bytes"`
	DataDir       string `json:"data_dir"`
}

func (n *Node) storageStats() storageStats {
	var snap, wal int64
	if fi, err := os.Stat(storage.SnapshotPath(n.cfg.DataDir)); err == nil {
		snap = fi.Size()
	}
	if fi, err := os.Stat(storage.WALPath(n.cfg.DataDir)); err == nil {
		wal = fi.Size()
	}
	return storageStats{SnapshotBytes: snap, WALBytes: wal, DataDir: filepath.Base(n.cfg.DataDir)}
}

// Status is the dashboard/control-plane view of one node.
type Status struct {
	ClusterID string       `json:"cluster_id"`
	NodeID    string       `json:"node_id"`
	Addr      string       `json:"addr"`
	Health    Health       `json:"health"`
	UptimeSec int64        `json:"uptime_sec"`
	Peers     []Member     `json:"peers"`
	Ranges    []Range      `json:"ranges"`
	Stats     Snapshot     `json:"stats"`
	Storage   storageStats `json:"storage"`
}

// --- RPC payloads ---

type pingPayload struct {
	From string `json:"from"`
}

// PingReply is returned by health checks. It piggybacks the responder's
// member list so clusters converge without a separate gossip round.
type PingReply struct {
	NodeID    string   `json:"node_id"`
	ClusterID string   `json:"cluster_id"`
	State     Health   `json:"state"`
	UptimeSec int64    `json:"uptime_sec"`
	Members   []Member `json:"members,omitempty"`
}

type joinPayload struct {
	Member    Member `json:"member"`
	ClusterID string `json:"cluster_id"`
}

type leavePayload struct {
	ID string `json:"id"`
}

type metadataPayload struct {
	ClusterID string   `json:"cluster_id"`
	Members   []Member `json:"members"`
	Ranges    []Range  `json:"ranges"`
}

type lookupPayload struct {
	Table string `json:"table"`
	Key   string `json:"key"`
}

type lookupReply struct {
	Range  Range  `json:"range"`
	Addr   string `json:"addr"`
	Local  bool   `json:"local"`
	Leader string `json:"leader"`
}

type forwardPayload struct {
	SQL string `json:"sql"`
}

func (n *Node) registerHandlers() {
	n.xport.Handle(KindPing, func(ctx context.Context, _ json.RawMessage) (any, error) {
		n.mu.RLock()
		h := n.health
		n.mu.RUnlock()
		var up int64
		if !n.started.IsZero() {
			up = int64(time.Since(n.started).Seconds())
		}
		return PingReply{NodeID: n.identity.NodeID, ClusterID: n.identity.ClusterID, State: h, UptimeSec: up, Members: n.members.List()}, nil
	})
	n.xport.Handle(KindJoin, func(_ context.Context, raw json.RawMessage) (any, error) {
		var p joinPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, NewError(CodeInvalidArgument, "bad join payload")
		}
		if p.ClusterID != "" && p.ClusterID != n.identity.ClusterID {
			return nil, NewError(CodeClusterMismatch, "joining node belongs to a different cluster")
		}
		if p.Member.ID == "" || p.Member.Addr == "" {
			return nil, NewError(CodeInvalidArgument, "join requires id and addr")
		}
		n.members.Upsert(p.Member.ID, p.Member.Addr, StateAlive, 1)
		return map[string]string{"status": "joined"}, nil
	})
	n.xport.Handle(KindLeave, func(_ context.Context, raw json.RawMessage) (any, error) {
		var p leavePayload
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, NewError(CodeInvalidArgument, "bad leave payload")
		}
		n.members.Remove(p.ID)
		return map[string]string{"status": "removed"}, nil
	})
	n.xport.Handle(KindMetadata, func(_ context.Context, _ json.RawMessage) (any, error) {
		return metadataPayload{ClusterID: n.identity.ClusterID, Members: n.members.List(), Ranges: n.ranges.All()}, nil
	})
	n.xport.Handle(KindLookup, func(_ context.Context, raw json.RawMessage) (any, error) {
		var p lookupPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, NewError(CodeInvalidArgument, "bad lookup payload")
		}
		route, err := n.router.RouteKey(p.Table, p.Key)
		if err != nil {
			return nil, err
		}
		return lookupReply{Range: route.Range, Addr: route.Addr, Local: route.Local, Leader: route.Leader}, nil
	})
	n.xport.Handle(KindForward, func(_ context.Context, raw json.RawMessage) (any, error) {
		var p forwardPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, NewError(CodeInvalidArgument, "bad forward payload")
		}
		if p.SQL == "" {
			return nil, NewError(CodeInvalidArgument, "forward requires sql")
		}
		start := time.Now()
		res, err := n.db.Exec(p.SQL)
		n.stats.Observe(p.SQL, time.Since(start), false, err != nil)
		if err != nil {
			return nil, NewError(CodeTransport, err.Error())
		}
		return res, nil
	})
}
