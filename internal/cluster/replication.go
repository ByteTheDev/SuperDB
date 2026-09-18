package cluster

import (
	"context"
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
	// RequiredAcks is the number of replicas (including local) that must
	// durably apply the write. 1 = local durability only.
	RequiredAcks int
}

// Replicator applies writes for replicated ranges.
type Replicator interface {
	// Replicate durably applies sql for rangeID and blocks until concern is
	// satisfied or ctx ends. It must never acknowledge before durability.
	Replicate(ctx context.Context, rangeID uint64, sql string, concern WriteConcern) error
	// State returns the local replica state for a range.
	State(rangeID uint64) (ReplicaState, error)
}

// LocalReplicator implements single-copy durability: the write is
// acknowledged only after the caller has applied it locally. Quorum writes
// (RequiredAcks > 1) are explicitly rejected until Raft exists.
type LocalReplicator struct {
	apply func(rangeID uint64, sql string) error
	state func(rangeID uint64) (ReplicaState, error)
}

// NewLocalReplicator wires local apply/state callbacks.
func NewLocalReplicator(
	apply func(rangeID uint64, sql string) error,
	state func(rangeID uint64) (ReplicaState, error),
) *LocalReplicator {
	return &LocalReplicator{apply: apply, state: state}
}

// Replicate applies the write locally and enforces explicit durability.
func (r *LocalReplicator) Replicate(ctx context.Context, rangeID uint64, sql string, concern WriteConcern) error {
	if concern.RequiredAcks > 1 {
		return NewError(CodeNoQuorum, "quorum writes require Raft consensus (not implemented)")
	}
	if err := ctx.Err(); err != nil {
		return NewError(CodeTimeout, "replicate cancelled: "+err.Error())
	}
	if err := r.apply(rangeID, sql); err != nil {
		return err
	}
	return nil
}

// State delegates to the wired state callback.
func (r *LocalReplicator) State(rangeID uint64) (ReplicaState, error) {
	return r.state(rangeID)
}
