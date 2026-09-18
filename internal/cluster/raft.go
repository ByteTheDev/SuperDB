package cluster

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"
	"superdb/internal/cluster/consensus"
)

// RaftTimeouts tunes the consensus layer. Production keeps hashicorp
// defaults; tests use aggressive values.
type RaftTimeouts struct {
	Heartbeat        time.Duration
	Election         time.Duration
	Commit           time.Duration
	SnapshotInterval time.Duration
}

// commitIndexKey is the stable-store key for the commit-tracking decorator.
var commitIndexKey = []byte("superdb/commit-index")

// trackingStore decorates a LogStore+StableStore pair with commit tracking
// so Raft replays committed (but unsnapshotted) logs after a restart.
// Without this, commitIndex is volatile and post-restart state would silently
// drop committed writes — a durability lie.
type trackingStore struct {
	logs   raft.LogStore
	stable raft.StableStore
}

func (t *trackingStore) GetLog(index uint64, log *raft.Log) error { return t.logs.GetLog(index, log) }
func (t *trackingStore) StoreLog(log *raft.Log) error             { return t.logs.StoreLog(log) }
func (t *trackingStore) StoreLogs(logs []*raft.Log) error         { return t.logs.StoreLogs(logs) }
func (t *trackingStore) DeleteRange(min, max uint64) error        { return t.logs.DeleteRange(min, max) }
func (t *trackingStore) FirstIndex() (uint64, error)              { return t.logs.FirstIndex() }
func (t *trackingStore) LastIndex() (uint64, error)               { return t.logs.LastIndex() }
func (t *trackingStore) Set(key []byte, val []byte) error         { return t.stable.Set(key, val) }
func (t *trackingStore) Get(key []byte) ([]byte, error)           { return t.stable.Get(key) }
func (t *trackingStore) SetUint64(key []byte, val uint64) error   { return t.stable.SetUint64(key, val) }
func (t *trackingStore) GetUint64(key []byte) (uint64, error)     { return t.stable.GetUint64(key) }

// GetCommitIndex returns the highest staged commit index (0 when none).
func (t *trackingStore) GetCommitIndex() (uint64, error) {
	v, err := t.stable.GetUint64(commitIndexKey)
	if err != nil {
		return 0, nil
	}
	return v, nil
}

// StageCommitIndex durably records the commit index alongside the log.
func (t *trackingStore) StageCommitIndex(index uint64) error {
	return t.stable.SetUint64(commitIndexKey, index)
}

func defaultRaftTimeouts() RaftTimeouts {
	return RaftTimeouts{Heartbeat: time.Second, Election: 5 * time.Second, Commit: 50 * time.Millisecond}
}

// raftNode wraps hashicorp/raft: leader election, log replication, joint-
// consensus membership changes, and file snapshots. No custom consensus.
type raftNode struct {
	mu        sync.RWMutex
	r         *raft.Raft
	transport *raft.NetworkTransport
	bolt      *raftboltdb.BoltStore
	observer  chan raft.Observation
	leaderCh  chan bool
}

func raftDir(dataDir string) string { return filepath.Join(dataDir, "raft") }

// hasRaftState reports whether durable Raft state already exists.
func hasRaftState(dataDir string) bool {
	st, err := os.Stat(filepath.Join(raftDir(dataDir), "raft.db"))
	return err == nil && st.Size() > 0
}

func openRaft(localID, advertise string, mux *consensus.Mux, dataDir string, timeouts RaftTimeouts, fsm raft.FSM, snapThreshold uint64) (*raftNode, error) {
	if timeouts.Heartbeat <= 0 {
		timeouts = defaultRaftTimeouts()
	}
	dir := raftDir(dataDir)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, NewError(CodePersist, "create raft dir: "+err.Error())
	}
	bolt, err := raftboltdb.NewBoltStore(filepath.Join(dir, "raft.db"))
	if err != nil {
		return nil, NewError(CodePersist, "open raft store: "+err.Error())
	}
	logs := &trackingStore{logs: bolt, stable: bolt}
	snaps, err := raft.NewFileSnapshotStoreWithLogger(dir, 2, hclog.NewNullLogger())
	if err != nil {
		_ = bolt.Close()
		return nil, NewError(CodePersist, "open snapshot store: "+err.Error())
	}
	layer := consensus.NewRaftStreamLayer(mux.RaftCh(), &netAddr{advertise})
	transport := raft.NewNetworkTransport(layer, 3, 10*time.Second, io.Discard)
	cfg := raft.DefaultConfig()
	cfg.LocalID = raft.ServerID(localID)
	cfg.HeartbeatTimeout = timeouts.Heartbeat
	cfg.ElectionTimeout = timeouts.Election
	cfg.CommitTimeout = timeouts.Commit
	cfg.LeaderLeaseTimeout = timeouts.Heartbeat / 2
	if cfg.LeaderLeaseTimeout <= 0 {
		cfg.LeaderLeaseTimeout = time.Millisecond
	}
	cfg.SnapshotThreshold = snapThreshold
	cfg.SnapshotInterval = timeouts.SnapshotInterval
	if cfg.SnapshotInterval <= 0 {
		cfg.SnapshotInterval = 120 * time.Second
	}
	cfg.RestoreCommittedLogs = false
	if os.Getenv("SUPERDB_RAFT_DEBUG") != "" {
		cfg.Logger = hclog.New(&hclog.LoggerOptions{Name: "raft", Level: hclog.Debug, Output: os.Stderr})
	} else {
		cfg.Logger = hclog.NewNullLogger()
	}
	r, err := raft.NewRaft(cfg, fsm, logs, bolt, snaps, transport)
	if err != nil {
		_ = bolt.Close()
		return nil, NewError(CodeTransport, "start raft: "+err.Error())
	}
	n := &raftNode{r: r, transport: transport, bolt: bolt, observer: make(chan raft.Observation, 16), leaderCh: make(chan bool, 1)}
	r.RegisterObserver(raft.NewObserver(n.observer, true, func(o *raft.Observation) bool {
		_, ok := o.Data.(raft.LeaderObservation)
		return ok
	}))
	return n, nil
}

// Bootstrap elects self as the first voter of a new cluster.
func (n *raftNode) Bootstrap(selfID, addr string) error {
	future := n.r.BootstrapCluster(raft.Configuration{Servers: []raft.Server{
		{ID: raft.ServerID(selfID), Address: raft.ServerAddress(addr), Suffrage: raft.Voter},
	}})
	return future.Error()
}

// Apply proposes one entry and waits for quorum commit.
func (n *raftNode) Apply(e *Entry, timeout time.Duration) (*EntryResult, error) {
	b, err := json.Marshal(e)
	if err != nil {
		return nil, NewError(CodeTransport, "encode entry: "+err.Error())
	}
	future := n.r.Apply(b, timeout)
	if err := future.Error(); err != nil {
		return nil, raftApplyError(err)
	}
	resp, ok := future.Response().(*EntryResult)
	if !ok || resp == nil {
		return nil, NewError(CodeTransport, "empty apply response")
	}
	if !resp.OK {
		return resp, NewError(CodeTransport, resp.Message)
	}
	return resp, nil
}

// Barrier guarantees linearizable reads on the leader.
func (n *raftNode) Barrier(timeout time.Duration) error {
	return raftApplyError(n.r.Barrier(timeout).Error())
}

// Leader returns the current leader ID and address.
func (n *raftNode) Leader() (id, addr string) {
	a, i := n.r.LeaderWithID()
	return string(i), string(a)
}

// IsLeader reports local leadership.
func (n *raftNode) IsLeader() bool { return n.r.State() == raft.Leader }

// State returns the Raft role.
func (n *raftNode) State() raft.RaftState { return n.r.State() }

// Stats exposes Raft telemetry for observability.
func (n *raftNode) Stats() map[string]string { return n.r.Stats() }

// LastIndex returns the latest log index.
func (n *raftNode) LastIndex() uint64 { return n.r.LastIndex() }

// AppliedIndex returns the latest applied index.
func (n *raftNode) AppliedIndex() uint64 { return n.r.AppliedIndex() }

// AddVoter proposes a joint-consensus membership change adding a voter.
func (n *raftNode) AddVoter(id, addr string, timeout time.Duration) error {
	return raftApplyError(n.r.AddVoter(raft.ServerID(id), raft.ServerAddress(addr), 0, timeout).Error())
}

// AddNonvoter adds a node as a non-voting replica.
func (n *raftNode) AddNonvoter(id, addr string, timeout time.Duration) error {
	return raftApplyError(n.r.AddNonvoter(raft.ServerID(id), raft.ServerAddress(addr), 0, timeout).Error())
}

// DemoteVoter demotes a voter to non-voter.
func (n *raftNode) DemoteVoter(id string, timeout time.Duration) error {
	return raftApplyError(n.r.DemoteVoter(raft.ServerID(id), 0, timeout).Error())
}

// RemoveServer proposes a joint-consensus removal.
func (n *raftNode) RemoveServer(id string, timeout time.Duration) error {
	return raftApplyError(n.r.RemoveServer(raft.ServerID(id), 0, timeout).Error())
}

// TransferLeadership moves leadership without an outage.
func (n *raftNode) TransferLeadership(timeout time.Duration) error {
	return raftApplyError(n.r.LeadershipTransfer().Error())
}

// Configuration returns the latest committed membership.
func (n *raftNode) Configuration() (raft.Configuration, error) {
	future := n.r.GetConfiguration()
	if err := future.Error(); err != nil {
		return raft.Configuration{}, raftApplyError(err)
	}
	return future.Configuration(), nil
}

// LeaderCh notifies on leadership transitions.
func (n *raftNode) LeaderCh() <-chan raft.Observation { return n.observer }

// Shutdown stops Raft and closes durable stores.
func (n *raftNode) Shutdown() {
	_ = n.r.Shutdown().Error()
	_ = n.transport.Close()
	_ = n.bolt.Close()
}

// barrierTimeout converts a context deadline into a Raft barrier timeout.
func barrierTimeout(ctx context.Context, fallback time.Duration) time.Duration {
	if dl, ok := ctx.Deadline(); ok {
		if d := time.Until(dl); d > 0 {
			return d
		}
		return time.Millisecond
	}
	return fallback
}

func raftApplyError(err error) error {
	if err == nil {
		return nil
	}
	if err == raft.ErrNotLeader {
		return &Error{Code: CodeNoLeader, Message: "not the leader", Retryable: true}
	}
	if err == raft.ErrLeadershipLost {
		return &Error{Code: CodeNoLeader, Message: "leadership lost during commit", Retryable: true}
	}
	return NewError(CodeUnavailable, err.Error())
}

type netAddr struct{ s string }

func (a *netAddr) Network() string { return "tcp" }
func (a *netAddr) String() string  { return a.s }
