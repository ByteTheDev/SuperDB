package cluster

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/raft"
	"superdb/internal/cluster/consensus"
	"superdb/internal/engine"
	"superdb/internal/storage"
)

// Config controls one cluster node.
type Config struct {
	DataDir           string
	ListenAddr        string
	AdvertiseAddr     string
	JoinAddrs         []string
	Region            string
	HeartbeatInterval time.Duration
	PingTimeout       time.Duration
	SuspectAfter      time.Duration
	Raft              RaftTimeouts
	SnapshotThreshold uint64
}

// Health describes node liveness.
type Health string

const (
	HealthHealthy  Health = "healthy"
	HealthDegraded Health = "degraded"
	HealthShutdown Health = "shutdown"
)

// Node is one SuperDB cluster member: identity, Raft consensus, membership,
// ranges, transport, routing, and observability around a local engine.
type Node struct {
	cfg      Config
	identity Identity
	db       *engine.Database
	members  *Membership
	ranges   *RangeStore
	router   *Router
	stats    *Stats
	xport    *Transport
	repl     *RaftReplicator

	rstate *ReplicatedState
	rn     *raftNode
	mux    *consensus.Mux

	mu       sync.RWMutex
	listener net.Listener
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	health   Health
	started  time.Time
	leader   atomic.Value // string: current raft leader ID
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
	if cfg.Raft.Heartbeat <= 0 {
		cfg.Raft = defaultRaftTimeouts()
	}
	if cfg.SnapshotThreshold == 0 {
		cfg.SnapshotThreshold = 1024
	}
	return cfg
}

// New creates a node around an existing local storage engine.
func New(cfg Config, db *engine.Database) *Node {
	cfg = fillDefaults(cfg)
	n := &Node{cfg: cfg, db: db, stats: NewStats(), xport: NewTransport(), ranges: NewRangeStore(), health: HealthHealthy}
	n.repl = &RaftReplicator{node: n}
	n.leader.Store("")
	return n
}

// DB exposes the local engine.
func (n *Node) DB() *engine.Database { return n.db }

// Identity returns persistent node/cluster identity.
func (n *Node) Identity() Identity { return n.identity }

// Members exposes the membership directory.
func (n *Node) Members() *Membership { return n.members }

// Ranges exposes the range directory.
func (n *Node) Ranges() *RangeStore { return n.ranges }

// Router exposes the routing layer.
func (n *Node) Router() *Router { return n.router }

// Region returns the configured region label.
func (n *Node) Region() string { return n.cfg.Region }

// StatsSnapshot returns observability counters.
func (n *Node) StatsSnapshot() Snapshot { return n.stats.Load() }

// AdvertiseAddr returns the address peers use to reach this node.
func (n *Node) AdvertiseAddr() string { return n.cfg.AdvertiseAddr }

// IsLeader reports Raft leadership.
func (n *Node) IsLeader() bool { return n.rn != nil && n.rn.IsLeader() }

// RaftState returns the Raft role.
func (n *Node) RaftState() string {
	if n.rn == nil {
		return "shutdown"
	}
	return n.rn.State().String()
}

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

// Start bootstraps or joins, then serves internal and consensus traffic.
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
	n.members.SetRegion(id.NodeID, n.cfg.Region)

	ln, err := net.Listen("tcp", n.cfg.ListenAddr)
	if err != nil {
		return NewError(CodeTransport, "listen: "+err.Error())
	}
	n.listener = ln
	// Capture before openRaft creates the store file.
	hadRaftState := hasRaftState(n.cfg.DataDir)
	n.mux = consensus.NewMux(ln)
	n.rstate = NewReplicatedState(n.db, n.ranges, storage.WALPath(n.cfg.DataDir))
	rn, err := openRaft(id.NodeID, n.cfg.AdvertiseAddr, n.mux, n.cfg.DataDir, n.cfg.Raft, &fsm{state: n.rstate}, n.cfg.SnapshotThreshold)
	if err != nil {
		_ = ln.Close()
		return err
	}
	n.rn = rn
	// Publish everything RPC handlers touch before serving: handlers run on
	// connection goroutines the moment Serve starts.
	n.router = NewRouter(n.ranges, n.members, n.identity.NodeID)
	n.started = time.Now()
	n.registerHandlers()

	nctx, cancel := context.WithCancel(context.Background())
	n.ctx = nctx
	n.cancel = cancel

	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		_ = n.xport.Serve(nctx, n.mux.River())
	}()

	bootstrapped := hadRaftState
	if len(n.cfg.JoinAddrs) == 0 && !bootstrapped {
		if err := rn.Bootstrap(id.NodeID, n.cfg.AdvertiseAddr); err != nil {
			n.stop()
			return NewError(CodeTransport, "bootstrap: "+err.Error())
		}
		bootstrapped = true
	}
	if len(n.cfg.JoinAddrs) > 0 && !hadRaftState {
		if jerr := n.join(ctx); jerr != nil {
			n.stop()
			return jerr
		}
	}
	// The initial range must itself be a committed log entry: only then do
	// restarts and new joiners converge on the same directory through
	// replay instead of in-memory defaults that diverge.
	if err := n.ensureGenesis(ctx); err != nil {
		n.stop()
		return err
	}
	n.syncLeaderRanges()

	if serr := StoreIdentity(n.cfg.DataDir, n.identity); serr != nil {
		n.stop()
		return serr
	}
	n.identity.Version = identityVersion
	n.wg.Add(2)
	go func() {
		defer n.wg.Done()
		n.heartbeatLoop(nctx)
	}()
	go func() {
		defer n.wg.Done()
		n.observeLoop(nctx)
	}()
	if bootstrapped && len(n.cfg.JoinAddrs) == 0 {
		_ = n.waitLeader(10 * time.Second)
	}
	return nil
}

// ensureGenesis commits the initial full-keyspace range when the directory
// is empty. Explicit ID 1 keeps every member convergent; no exec entry can
// reference a range before genesis exists, because leaders reject writes
// against an empty directory.
func (n *Node) ensureGenesis(ctx context.Context) error {
	if len(n.ranges.All()) > 0 {
		return nil
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if len(n.ranges.All()) > 0 {
			return nil
		}
		if n.rn.IsLeader() {
			genesis := &RangeOp{Type: RangeOpSplit, Ranges: []Range{{
				ID: 1, StartKey: "", EndKey: "",
				Replicas: []string{n.identity.NodeID}, Leader: n.identity.NodeID, Generation: 1,
			}}}
			if _, err := n.rn.Apply(&Entry{V: 1, Kind: EntryRange, Op: genesis}, 10*time.Second); err != nil {
				return err
			}
			continue
		}
		select {
		case <-ctx.Done():
			return NewError(CodeNoLeader, "no leader to commit genesis range")
		case <-time.After(50 * time.Millisecond):
		}
	}
	return NewError(CodeNoLeader, "genesis range not established")
}

func (n *Node) waitLeader(d time.Duration) error {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if id, _ := n.rn.Leader(); id != "" {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return NewError(CodeNoLeader, "no leader elected yet")
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
		n.identity.ClusterID = meta.ClusterID
		n.ranges.ReplaceBulk(meta.Ranges)
		n.members.MergeBulk(meta.Members)
		target := seed
		if meta.LeaderAddr != "" {
			target = meta.LeaderAddr
		}
		jm := joinPayload{
			Member:    Member{ID: n.identity.NodeID, Addr: n.cfg.AdvertiseAddr, State: StateAlive, LastSeen: time.Now(), Version: 1, Region: n.cfg.Region},
			ClusterID: meta.ClusterID,
		}
		cctx2, cancel2 := context.WithTimeout(ctx, n.cfg.PingTimeout)
		jerr := n.xport.Call(cctx2, target, KindJoin, jm, nil)
		cancel2()
		if jerr != nil {
			lastErr = jerr
			continue
		}
		n.members.UpsertMember(Member{ID: n.identity.NodeID, Addr: n.cfg.AdvertiseAddr, State: StateAlive, LastSeen: time.Now(), Version: 1, Region: n.cfg.Region})
		return nil
	}
	if lastErr == nil {
		lastErr = NewError(CodeUnavailable, "no join addresses")
	}
	return lastErr
}

// Shutdown leaves gracefully and stops all node subsystems.
func (n *Node) Shutdown() {
	if !n.markShutdown() {
		return
	}
	n.announceLeave()
	n.removeSelf()
	n.stop()
}

// Kill stops the node without announcements, simulating a crash.
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
	if n.rn != nil {
		n.rn.Shutdown()
	}
	if n.mux != nil {
		_ = n.mux.Close()
	} else if n.listener != nil {
		_ = n.listener.Close()
	}
	n.xport.Close()
	n.wg.Wait()
}

func (n *Node) announceLeave() {
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

// removeSelf asks the leader to drop us from the Raft configuration.
// Guarded: never remove the last voter (that would destroy quorum).
func (n *Node) removeSelf() {
	if n.rn == nil {
		return
	}
	cfg, err := n.rn.Configuration()
	if err != nil {
		return
	}
	voters := 0
	for _, s := range cfg.Servers {
		if s.Suffrage == raft.Voter {
			voters++
		}
	}
	if voters <= 1 {
		return
	}
	_, leaderAddr := n.rn.Leader()
	if leaderAddr == "" {
		return
	}
	if n.rn.IsLeader() {
		_ = n.rn.TransferLeadership(5 * time.Second)
		time.Sleep(500 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = n.forwardRemove(ctx, string(leaderAddr), n.identity.NodeID)
}

func (n *Node) forwardRemove(ctx context.Context, addr, id string) error {
	if addr == n.cfg.AdvertiseAddr {
		return n.rn.RemoveServer(id, 5*time.Second)
	}
	return n.xport.Call(ctx, addr, KindRemove, removePayload{ID: id}, nil)
}

// --- write / read paths ---

func isRead(sql string) bool {
	upper := strings.ToUpper(strings.TrimSpace(sql))
	return strings.HasPrefix(upper, "SELECT")
}

// Exec routes one statement: writes commit through Raft quorum on the
// leader; reads execute on the local engine.
func (n *Node) Exec(ctx context.Context, sql string) (engine.Result, error) {
	start := time.Now()
	if isRead(sql) {
		res, err := n.db.Exec(sql)
		n.stats.Observe(sql, time.Since(start), false, err != nil)
		if err != nil {
			return engine.Result{}, NewError(CodeTransport, err.Error())
		}
		return res, nil
	}
	table := ExtractTable(sql)
	rng := n.ranges.TableRange(strings.ToLower(table))
	res, err := n.writeCommitted(ctx, []string{sql}, false, rng.ID, WriteConcern{RequiredAcks: 0})
	n.stats.Observe(sql, time.Since(start), false, err != nil)
	return res, err
}

// ExecAtomic commits multiple statements as one Raft entry: all-or-nothing.
func (n *Node) ExecAtomic(ctx context.Context, sqls []string) ([]engine.Result, error) {
	start := time.Now()
	if len(sqls) == 0 {
		return nil, NewError(CodeInvalidArgument, "empty batch")
	}
	for _, sql := range sqls {
		if isRead(sql) {
			return nil, NewError(CodeInvalidArgument, "atomic batches accept writes only")
		}
	}
	table := ExtractTable(sqls[0])
	rng := n.ranges.TableRange(strings.ToLower(table))
	res, err := n.writeCommitted(ctx, sqls, true, rng.ID, WriteConcern{RequiredAcks: 0})
	n.stats.Observe("BATCH", time.Since(start), false, err != nil)
	if err != nil {
		return nil, err
	}
	return []engine.Result{res}, nil
}

// ExecConsistent performs a linearizable read via leader barrier.
func (n *Node) ExecConsistent(ctx context.Context, sql string) (engine.Result, error) {
	start := time.Now()
	if !isRead(sql) {
		return n.Exec(ctx, sql)
	}
	if !n.rn.IsLeader() {
		if _, addr := n.rn.Leader(); addr != "" && string(addr) != n.cfg.AdvertiseAddr {
			var res engine.Result
			cctx, cancel := context.WithTimeout(ctx, n.cfg.PingTimeout)
			defer cancel()
			if err := n.xport.Call(cctx, string(addr), KindForward, forwardPayload{SQL: sql, Consistent: true}, &res); err == nil {
				n.stats.Observe(sql, time.Since(start), true, false)
				return res, nil
			}
		}
	}
	cctx, cancel := context.WithTimeout(ctx, n.cfg.PingTimeout)
	defer cancel()
	if err := n.rn.Barrier(barrierTimeout(cctx, n.cfg.PingTimeout)); err != nil {
		n.stats.Observe(sql, time.Since(start), false, true)
		return engine.Result{}, err
	}
	res, err := n.db.Exec(sql)
	n.stats.Observe(sql, time.Since(start), false, err != nil)
	if err != nil {
		return engine.Result{}, NewError(CodeTransport, err.Error())
	}
	return res, nil
}

func (n *Node) writeCommitted(ctx context.Context, sqls []string, atomic bool, rangeID uint64, concern WriteConcern) (engine.Result, error) {
	if concern.RequiredAcks > 0 {
		cfg, err := n.rn.Configuration()
		if err != nil {
			return engine.Result{}, err
		}
		voters := 0
		for _, s := range cfg.Servers {
			if s.Suffrage == raft.Voter {
				voters++
			}
		}
		if concern.RequiredAcks > voters {
			return engine.Result{}, NewError(CodeNoQuorum, "write concern exceeds voter count")
		}
	}
	if !n.rn.IsLeader() {
		if _, addr := n.rn.Leader(); addr != "" {
			var res engine.Result
			cctx, cancel := context.WithTimeout(ctx, n.cfg.PingTimeout)
			defer cancel()
			fp := forwardPayload{SQLs: sqls, Atomic: atomic, RangeID: rangeID}
			if len(sqls) == 1 {
				fp.SQL = sqls[0]
			}
			if err := n.xport.Call(cctx, string(addr), KindForward, fp, &res); err == nil {
				return res, nil
			} else if ce, ok := AsError(err); ok && ce.Code == CodeNoLeader {
				if _, addr2 := n.rn.Leader(); addr2 != "" && string(addr2) != string(addr) {
					cctx2, cancel2 := context.WithTimeout(ctx, n.cfg.PingTimeout)
					defer cancel2()
					if err2 := n.xport.Call(cctx2, string(addr2), KindForward, fp, &res); err2 == nil {
						return res, nil
					} else {
						return engine.Result{}, err2
					}
				}
			} else {
				return engine.Result{}, err
			}
		}
		return engine.Result{}, NewError(CodeNoLeader, "no leader available for write")
	}
	rng, err := n.ranges.ByID(rangeID)
	if err != nil {
		return engine.Result{}, err
	}
	pctx, cancel := proposeTimeout(ctx)
	defer cancel()
	result, err := n.rn.Apply(&Entry{V: 1, Kind: EntryExec, SQLs: sqls, Atomic: atomic, RangeID: rangeID, Generation: rng.Generation}, 10*time.Second)
	_ = pctx
	if err != nil {
		return engine.Result{}, err
	}
	return n.resultFor(result, sqls)
}

func (n *Node) resultFor(result *EntryResult, sqls []string) (engine.Result, error) {
	if len(sqls) == 1 && !isRead(sqls[0]) {
		// Return the engine-shaped result for single writes.
		if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(sqls[0])), "INSERT") ||
			strings.HasPrefix(strings.ToUpper(strings.TrimSpace(sqls[0])), "UPDATE") ||
			strings.HasPrefix(strings.ToUpper(strings.TrimSpace(sqls[0])), "DELETE") {
			return engine.Result{Affected: result.Affected}, nil
		}
		return engine.Result{Message: "ok"}, nil
	}
	return engine.Result{Affected: result.Affected}, nil
}

func (n *Node) replicaState(rangeID uint64) (ReplicaState, error) {
	rng, err := n.ranges.ByID(rangeID)
	if err != nil {
		return ReplicaState{}, err
	}
	role := RoleFollower
	if id, _ := n.rn.Leader(); id == n.identity.NodeID {
		role = RoleLeader
	}
	return ReplicaState{
		RangeID: rangeID, Role: role, Leader: rng.Leader,
		Generation: rng.Generation, Applied: n.rstate.AppliedIndex(), Committed: n.rn.LastIndex(),
	}, nil
}

// --- range management (proposed through Raft) ---

// SplitRange divides a range at splitKey; tables named in leftTables move to
// the left child, the rest to the right child. Generations fence the op.
func (n *Node) SplitRange(ctx context.Context, rangeID uint64, splitKey string, leftTables []string) (Range, Range, error) {
	rng, err := n.ranges.ByID(rangeID)
	if err != nil {
		return Range{}, Range{}, err
	}
	left := Range{StartKey: rng.StartKey, EndKey: splitKey, Replicas: append([]string(nil), rng.Replicas...), Leader: rng.Leader, Generation: rng.Generation + 1}
	right := Range{StartKey: splitKey, EndKey: rng.EndKey, Replicas: append([]string(nil), rng.Replicas...), Leader: rng.Leader, Generation: rng.Generation + 1}
	op := &RangeOp{Type: RangeOpSplit, Ranges: []Range{left, right}, Remove: []uint64{rangeID},
		BaseGenerations: map[uint64]uint64{rangeID: rng.Generation}}
	// Assignment resolves after IDs are allocated: propose the split first,
	// then assign tables to the returned children in a second entry.
	if _, err := n.proposeRange(ctx, op); err != nil {
		return Range{}, Range{}, err
	}
	children := n.childrenOf(splitKey)
	if len(children) != 2 {
		return Range{}, Range{}, NewError(CodeTransport, "split did not yield two ranges")
	}
	assign2 := make(map[string]uint64)
	for _, t := range leftTables {
		assign2[strings.ToLower(t)] = children[0].ID
	}
	// Tables previously on the parent default to the right child unless listed.
	for _, t := range n.tablesOn(rangeID) {
		if _, ok := assign2[strings.ToLower(t)]; !ok {
			assign2[strings.ToLower(t)] = children[1].ID
		}
	}
	op2 := &RangeOp{Type: RangeOpAssign, Assign: assign2,
		BaseGenerations: map[uint64]uint64{children[0].ID: children[0].Generation, children[1].ID: children[1].Generation}}
	if _, err := n.proposeRange(ctx, op2); err != nil {
		return Range{}, Range{}, err
	}
	l, _ := n.ranges.ByID(children[0].ID)
	rr, _ := n.ranges.ByID(children[1].ID)
	return l, rr, nil
}

func (n *Node) tablesOn(rangeID uint64) []string {
	var out []string
	// Tables resolve to this range either by assignment or by default routing
	// when unassigned; only assigned ones are tracked explicitly.
	for t, id := range n.ranges.Assignment() {
		if id == rangeID {
			out = append(out, t)
		}
	}
	return out
}

func (n *Node) childrenOf(splitKey string) []Range {
	var out []Range
	for _, r := range n.ranges.All() {
		if r.EndKey == splitKey || r.StartKey == splitKey {
			out = append(out, r)
		}
	}
	return out
}

// MoveRange changes the serving replicas of a range (generation fenced).
func (n *Node) MoveRange(ctx context.Context, rangeID uint64, replicas []string) (Range, error) {
	rng, err := n.ranges.ByID(rangeID)
	if err != nil {
		return Range{}, err
	}
	leader := rng.Leader
	found := false
	for _, r := range replicas {
		if r == leader {
			found = true
		}
	}
	if !found && len(replicas) > 0 {
		leader = replicas[0]
	}
	updated := rng
	updated.Replicas = replicas
	updated.Leader = leader
	updated.Generation++
	op := &RangeOp{Type: RangeOpMove, Ranges: []Range{updated}, BaseGenerations: map[uint64]uint64{rangeID: rng.Generation}}
	if _, err := n.proposeRange(ctx, op); err != nil {
		return Range{}, err
	}
	return n.ranges.ByID(rangeID)
}

// AssignTable routes a table to a range.
func (n *Node) AssignTable(ctx context.Context, table string, rangeID uint64) error {
	rng, err := n.ranges.ByID(rangeID)
	if err != nil {
		return err
	}
	op := &RangeOp{Type: RangeOpAssign, Assign: map[string]uint64{strings.ToLower(table): rangeID},
		BaseGenerations: map[uint64]uint64{rangeID: rng.Generation}}
	_, err = n.proposeRange(ctx, op)
	return err
}

func (n *Node) proposeRange(ctx context.Context, op *RangeOp) (*EntryResult, error) {
	if !n.rn.IsLeader() {
		if _, addr := n.rn.Leader(); addr != "" {
			var res EntryResult
			cctx, cancel := context.WithTimeout(ctx, n.cfg.PingTimeout)
			defer cancel()
			if err := n.xport.Call(cctx, string(addr), KindRangeOp, op, &res); err != nil {
				return nil, err
			}
			if !res.OK {
				return &res, NewError(CodeTransport, res.Message)
			}
			// Wait until the op is visible locally.
			deadline := time.Now().Add(10 * time.Second)
			for time.Now().Before(deadline) {
				if n.rangeOpVisible(op) {
					return &res, nil
				}
				time.Sleep(20 * time.Millisecond)
			}
			return &res, nil
		}
		return nil, NewError(CodeNoLeader, "no leader for range operation")
	}
	return n.rn.Apply(&Entry{V: 1, Kind: EntryRange, Op: op}, 10*time.Second)
}

func (n *Node) rangeOpVisible(op *RangeOp) bool {
	for t, id := range op.Assign {
		if got := n.ranges.TableRange(t); got.ID != id {
			return false
		}
	}
	for _, id := range op.Remove {
		if _, err := n.ranges.ByID(id); err == nil {
			return false
		}
	}
	return true
}

// Rebalance converges the serving directory with the committed Raft
// configuration in a single entry: every range's replicas become the voter
// set and its leader becomes the Raft leader. Call after joins, leaves, or
// failovers; range splits/moves stay explicit operations.
func (n *Node) Rebalance(ctx context.Context) error {
	cfg, err := n.rn.Configuration()
	if err != nil {
		return err
	}
	var voters []string
	for _, s := range cfg.Servers {
		if s.Suffrage == raft.Voter {
			voters = append(voters, string(s.ID))
		}
	}
	lid, _ := n.rn.Leader()
	if lid == "" {
		return NewError(CodeNoLeader, "no leader to rebalance toward")
	}
	op := &RangeOp{Type: RangeOpMove, BaseGenerations: make(map[uint64]uint64)}
	for _, r := range n.ranges.All() {
		if sameStrings(r.Replicas, voters) && r.Leader == lid {
			continue
		}
		updated := r
		updated.Replicas = append([]string(nil), voters...)
		updated.Leader = lid
		updated.Generation++
		op.Ranges = append(op.Ranges, updated)
		op.BaseGenerations[r.ID] = r.Generation
	}
	if len(op.Ranges) == 0 {
		return nil
	}
	_, err = n.proposeRange(ctx, op)
	return err
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, s := range a {
		seen[s]++
	}
	for _, s := range b {
		seen[s]--
		if seen[s] < 0 {
			return false
		}
	}
	return true
}

// MoveLeader transfers Raft leadership without an outage.
func (n *Node) MoveLeader(ctx context.Context) error {
	if n.rn.IsLeader() {
		return n.rn.TransferLeadership(10 * time.Second)
	}
	if _, addr := n.rn.Leader(); addr != "" {
		cctx, cancel := context.WithTimeout(ctx, n.cfg.PingTimeout)
		defer cancel()
		return n.xport.Call(cctx, string(addr), KindMoveLeader, nil, nil)
	}
	return NewError(CodeNoLeader, "no leader to move from")
}

// PreferRegionLeader moves leadership into a region when a voter there is
// reachable, keeping quorum latency local to where callers are.
func (n *Node) PreferRegionLeader(ctx context.Context, region string) error {
	id, _ := n.rn.Leader()
	if id == "" {
		return NewError(CodeNoLeader, "no leader yet")
	}
	if mb, ok := n.members.Get(id); ok && mb.Region == region {
		return nil
	}
	return n.MoveLeader(ctx)
}

// Placement reports voters and regions for dashboards.
func (n *Node) Placement() Placement {
	p := Placement{Region: n.cfg.Region, ByRegion: make(map[string]int)}
	cfg, err := n.rn.Configuration()
	if err != nil {
		return p
	}
	for _, s := range cfg.Servers {
		region := ""
		if mb, ok := n.members.Get(string(s.ID)); ok {
			region = mb.Region
		}
		p.Servers = append(p.Servers, PlacedServer{ID: string(s.ID), Addr: string(s.Address), Voter: s.Suffrage == raft.Voter, Region: region})
		p.ByRegion[region]++
	}
	return p
}

// Placement describes voter distribution across regions.
type Placement struct {
	Region   string         `json:"region"`
	Servers  []PlacedServer `json:"servers"`
	ByRegion map[string]int `json:"by_region"`
}

// PlacedServer is one Raft configuration entry with its region.
type PlacedServer struct {
	ID     string `json:"id"`
	Addr   string `json:"addr"`
	Voter  bool   `json:"voter"`
	Region string `json:"region"`
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
				n.members.UpsertMember(Member{ID: out.NodeID, Addr: m.Addr, State: StateAlive, Version: m.Version, Region: out.Region})
			}
			n.members.SweepSuspects(n.cfg.SuspectAfter, now)
		}
	}
}

func (n *Node) observeLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case ob := <-n.rn.LeaderCh():
			if lo, ok := ob.Data.(raft.LeaderObservation); ok {
				n.leader.Store(string(lo.LeaderID))
				n.syncLeaderRanges()
			}
		}
	}
}

func (n *Node) syncLeaderRanges() {
	id, _ := n.rn.Leader()
	if id == "" {
		return
	}
	n.leader.Store(id)
	for _, r := range n.ranges.All() {
		n.ranges.SetRangeLeader(r.ID, id)
	}
}

// Status is the observability snapshot for dashboards.
func (n *Node) Status() Status {
	n.mu.RLock()
	health := n.health
	n.mu.RUnlock()
	members := n.members.List()
	ranges := n.ranges.All()
	snap := n.stats.Load()
	st := Status{
		ClusterID: n.identity.ClusterID,
		NodeID:    n.identity.NodeID,
		Addr:      n.cfg.AdvertiseAddr,
		Region:    n.cfg.Region,
		Health:    health,
		UptimeSec: 0,
		Peers:     members,
		Ranges:    ranges,
		Stats:     snap,
		Storage:   n.storageStats(),
		Assign:    n.ranges.Assignment(),
	}
	if !n.started.IsZero() {
		st.UptimeSec = int64(time.Since(n.started).Seconds())
	}
	if n.rn != nil {
		lid, laddr := n.rn.Leader()
		st.Raft = RaftStatus{
			State:   n.rn.State().String(),
			Leader:  lid,
			Addr:    laddr,
			Applied: n.rstate.AppliedIndex(),
			Last:    n.rn.LastIndex(),
		}
		for k, v := range n.rn.Stats() {
			switch k {
			case "term":
				st.Raft.Term = v
			case "commit_index":
				st.Raft.Commit = v
			case "num_peers":
				st.Raft.Peers = v
			}
		}
		st.Placement = n.Placement()
	}
	return st
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
	ClusterID string            `json:"cluster_id"`
	NodeID    string            `json:"node_id"`
	Addr      string            `json:"addr"`
	Region    string            `json:"region"`
	Health    Health            `json:"health"`
	UptimeSec int64             `json:"uptime_sec"`
	Peers     []Member          `json:"peers"`
	Ranges    []Range           `json:"ranges"`
	Assign    map[string]uint64 `json:"assign"`
	Stats     Snapshot          `json:"stats"`
	Storage   storageStats      `json:"storage"`
	Raft      RaftStatus        `json:"raft"`
	Placement Placement         `json:"placement"`
}

// RaftStatus exposes consensus internals for operators.
type RaftStatus struct {
	State   string `json:"state"`
	Leader  string `json:"leader"`
	Addr    string `json:"addr"`
	Term    string `json:"term"`
	Commit  string `json:"commit"`
	Peers   string `json:"peers"`
	Applied uint64 `json:"applied_index"`
	Last    uint64 `json:"last_index"`
}

// --- RPC payloads ---

type pingPayload struct {
	From string `json:"from"`
}

// PingReply is returned by health checks.
type PingReply struct {
	NodeID    string   `json:"node_id"`
	ClusterID string   `json:"cluster_id"`
	Region    string   `json:"region"`
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

type removePayload struct {
	ID string `json:"id"`
}

type metadataPayload struct {
	ClusterID  string   `json:"cluster_id"`
	Members    []Member `json:"members"`
	Ranges     []Range  `json:"ranges"`
	LeaderAddr string   `json:"leader_addr"`
	LeaderID   string   `json:"leader_id"`
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
	SQL        string   `json:"sql,omitempty"`
	SQLs       []string `json:"sqls,omitempty"`
	Atomic     bool     `json:"atomic,omitempty"`
	RangeID    uint64   `json:"range_id,omitempty"`
	Consistent bool     `json:"consistent,omitempty"`
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
		return PingReply{NodeID: n.identity.NodeID, ClusterID: n.identity.ClusterID, Region: n.cfg.Region, State: h, UptimeSec: up, Members: n.members.List()}, nil
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
		if !n.rn.IsLeader() {
			_, addr := n.rn.Leader()
			if addr == "" {
				return nil, NewError(CodeNoLeader, "no leader to admit join")
			}
			return nil, &Error{Code: CodeNoLeader, Message: "join via leader " + string(addr), Retryable: true}
		}
		if err := n.rn.AddVoter(p.Member.ID, p.Member.Addr, 10*time.Second); err != nil {
			return nil, err
		}
		p.Member.State = StateAlive
		p.Member.LastSeen = time.Now()
		n.members.UpsertMember(p.Member)
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
	n.xport.Handle(KindRemove, func(_ context.Context, raw json.RawMessage) (any, error) {
		var p removePayload
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, NewError(CodeInvalidArgument, "bad remove payload")
		}
		if !n.rn.IsLeader() {
			return nil, NewError(CodeNoLeader, "only the leader removes voters")
		}
		if err := n.rn.RemoveServer(p.ID, 10*time.Second); err != nil {
			return nil, err
		}
		n.members.Remove(p.ID)
		return map[string]string{"status": "removed"}, nil
	})
	n.xport.Handle(KindMetadata, func(_ context.Context, _ json.RawMessage) (any, error) {
		lid, laddr := n.rn.Leader()
		return metadataPayload{ClusterID: n.identity.ClusterID, Members: n.members.List(), Ranges: n.ranges.All(), LeaderAddr: string(laddr), LeaderID: lid}, nil
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
	n.xport.Handle(KindForward, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p forwardPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, NewError(CodeInvalidArgument, "bad forward payload")
		}
		sqls := p.SQLs
		if len(sqls) == 0 {
			if p.SQL == "" {
				return nil, NewError(CodeInvalidArgument, "forward requires sql")
			}
			sqls = []string{p.SQL}
		}
		if p.Consistent && len(sqls) == 1 && isRead(sqls[0]) {
			start := time.Now()
			if !n.rn.IsLeader() {
				return nil, NewError(CodeNoLeader, "consistent read requires leader")
			}
			cctx, cancel := context.WithTimeout(ctx, n.cfg.PingTimeout)
			defer cancel()
			if err := n.rn.Barrier(barrierTimeout(cctx, n.cfg.PingTimeout)); err != nil {
				return nil, err
			}
			res, err := n.db.Exec(sqls[0])
			n.stats.Observe(sqls[0], time.Since(start), false, err != nil)
			if err != nil {
				return nil, NewError(CodeTransport, err.Error())
			}
			return res, nil
		}
		for _, sql := range sqls {
			if isRead(sql) {
				start := time.Now()
				res, err := n.db.Exec(sql)
				n.stats.Observe(sql, time.Since(start), false, err != nil)
				if err != nil {
					return nil, NewError(CodeTransport, err.Error())
				}
				return res, nil
			}
		}
		if !n.rn.IsLeader() {
			_, addr := n.rn.Leader()
			return nil, &Error{Code: CodeNoLeader, Message: "forward to leader " + string(addr), Retryable: true}
		}
		rng, err := n.ranges.ByID(p.RangeID)
		if err != nil && p.RangeID != 0 {
			return nil, err
		}
		result, err := n.rn.Apply(&Entry{V: 1, Kind: EntryExec, SQLs: sqls, Atomic: p.Atomic, RangeID: p.RangeID, Generation: rng.Generation}, 10*time.Second)
		if err != nil {
			return nil, err
		}
		return n.resultFor(result, sqls)
	})
	n.xport.Handle(KindRangeOp, func(_ context.Context, raw json.RawMessage) (any, error) {
		var op RangeOp
		if err := json.Unmarshal(raw, &op); err != nil {
			return nil, NewError(CodeInvalidArgument, "bad range op")
		}
		if !n.rn.IsLeader() {
			return nil, NewError(CodeNoLeader, "only the leader applies range ops")
		}
		res, err := n.rn.Apply(&Entry{V: 1, Kind: EntryRange, Op: &op}, 10*time.Second)
		if err != nil {
			return nil, err
		}
		return res, nil
	})
	n.xport.Handle(KindMoveLeader, func(_ context.Context, _ json.RawMessage) (any, error) {
		if !n.rn.IsLeader() {
			return nil, NewError(CodeNoLeader, "only the leader transfers leadership")
		}
		if err := n.rn.TransferLeadership(10 * time.Second); err != nil {
			return nil, err
		}
		return map[string]string{"status": "transferring"}, nil
	})
}
