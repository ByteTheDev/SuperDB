package cluster

import (
	"context"
	"time"
)

// ReplicaRole describes what a node does for a range.
type ReplicaRole string

const (
	RoleLeader   ReplicaRole = "leader"
	RoleFollower ReplicaRole = "follower"
	RoleLearner  ReplicaRole = "learner"
)

// ReplicaState tracks replication progress for one range on one node.
// Generation guards against stale leaders after moves/splits.
type ReplicaState struct {
	RangeID    uint64
	Role       ReplicaRole
	Leader     string
	Generation uint64
	Applied    uint64
	Committed  uint64
}

// WriteConcern states the durability condition a write must satisfy before
// it may be acknowledged to a client.
type WriteConcern struct {
	// RequiredAcks is the number of replicas that must durably commit the
	// write. 1 means leader commit (still a Raft quorum round); values
	// above the voter count are rejected explicitly, never faked.
	RequiredAcks int
}

// Replicator applies writes for replicated ranges.
type Replicator interface {
	// Replicate commits sql for rangeID and blocks until concern is
	// satisfied or ctx ends. It never acknowledges before durability.
	Replicate(ctx context.Context, rangeID uint64, sql string, concern WriteConcern) error
	// State returns the local replica state for a range.
	State(rangeID uint64) (ReplicaState, error)
}

// RaftReplicator commits writes through the Raft log: acknowledgement
// happens only after a quorum has durably stored the entry.
type RaftReplicator struct {
	node *Node
}

// Replicate proposes one SQL statement as a Raft entry.
func (r *RaftReplicator) Replicate(ctx context.Context, rangeID uint64, sql string, concern WriteConcern) error {
	_, err := r.node.writeCommitted(ctx, []string{sql}, false, rangeID, concern)
	return err
}

// State delegates to node replica state.
func (r *RaftReplicator) State(rangeID uint64) (ReplicaState, error) {
	return r.node.replicaState(rangeID)
}

// proposeTimeout bounds a single commit round.
func proposeTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, 10*time.Second)
}
