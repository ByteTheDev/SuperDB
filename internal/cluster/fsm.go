package cluster

import (
	"encoding/json"
	"io"

	"github.com/hashicorp/raft"
)

// Raft log entry kinds. Every committed entry applies deterministically on
// every member, so the cluster converges without extra coordination.
const (
	EntryExec  = "exec"
	EntryRange = "range"
)

// RangeOpType describes a range-directory mutation.
type RangeOpType string

const (
	RangeOpSplit  RangeOpType = "split"
	RangeOpMerge  RangeOpType = "merge"
	RangeOpMove   RangeOpType = "move"
	RangeOpAssign RangeOpType = "assign"
)

// RangeOp carries a fully-computed post-image: the proposer computes, every
// member applies verbatim. BaseGenerations fences stale proposals.
type RangeOp struct {
	Type            RangeOpType       `json:"type"`
	Ranges          []Range           `json:"ranges,omitempty"`
	Remove          []uint64          `json:"remove,omitempty"`
	Assign          map[string]uint64 `json:"assign,omitempty"`
	Unassign        []string          `json:"unassign,omitempty"`
	BaseGenerations map[uint64]uint64 `json:"base_generations,omitempty"`
}

// Entry is one Raft log record.
type Entry struct {
	V          int      `json:"v"`
	Kind       string   `json:"kind"`
	SQLs       []string `json:"sqls,omitempty"`
	Atomic     bool     `json:"atomic,omitempty"`
	RangeID    uint64   `json:"range_id,omitempty"`
	Generation uint64   `json:"generation,omitempty"`
	Op         *RangeOp `json:"op,omitempty"`
}

// EntryResult is the deterministic outcome recorded for an entry.
type EntryResult struct {
	OK       bool   `json:"ok"`
	Message  string `json:"message,omitempty"`
	Affected int    `json:"affected,omitempty"`
}

// fsm is the raft.FSM: committed entries mutate the local engine, the range
// directory, and the legacy WAL side-copy, in log order, under one mutex.
type fsm struct {
	state *ReplicatedState
}

func (f *fsm) Apply(log *raft.Log) any {
	var e Entry
	if err := json.Unmarshal(log.Data, &e); err != nil {
		return &EntryResult{OK: false, Message: "decode entry: " + err.Error()}
	}
	return f.state.applyCommitted(log.Index, &e)
}

func (f *fsm) Snapshot() (raft.FSMSnapshot, error) {
	return f.state.snapshot()
}

func (f *fsm) Restore(rc io.ReadCloser) error {
	defer rc.Close()
	return f.state.restore(rc)
}
