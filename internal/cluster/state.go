package cluster

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"github.com/hashicorp/raft"
	"superdb/internal/engine"
	"superdb/internal/storage"
)

// replicatedSnapshot is the FSM snapshot image.
type replicatedSnapshot struct {
	Index  uint64            `json:"index"`
	DB     json.RawMessage   `json:"db"`
	Ranges []Range           `json:"ranges"`
	Assign map[string]uint64 `json:"assign"`
	NextID uint64            `json:"next_range_id"`
}

// ReplicatedState is the deterministic state machine behind the Raft log:
// engine data, range directory, and table assignment evolve only here.
type ReplicatedState struct {
	mu      sync.Mutex
	db      *engine.Database
	ranges  *RangeStore
	walPath string
	index   uint64
}

// NewReplicatedState wraps the node's engine and range directory.
func NewReplicatedState(db *engine.Database, ranges *RangeStore, walPath string) *ReplicatedState {
	return &ReplicatedState{db: db, ranges: ranges, walPath: walPath}
}

// AppliedIndex returns the last committed Raft index applied locally.
func (s *ReplicatedState) AppliedIndex() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.index
}

func (s *ReplicatedState) applyCommitted(idx uint64, e *Entry) *EntryResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	var res *EntryResult
	switch e.Kind {
	case EntryExec:
		res = s.execLocked(e)
	case EntryRange:
		res = s.rangeLocked(e)
	default:
		return &EntryResult{OK: false, Message: "unknown entry kind"}
	}
	// Advance even on failure: outcomes are deterministic functions of the
	// entry plus prior state, so a replay reproduces the same result and
	// must not re-execute earlier entries.
	s.index = idx
	return res
}

func (s *ReplicatedState) execLocked(e *Entry) *EntryResult {
	if e.RangeID != 0 {
		r, err := s.ranges.lookupLocked(e.RangeID)
		if err != nil {
			return &EntryResult{OK: false, Message: err.Error()}
		}
		if e.Generation != 0 && e.Generation != r.Generation {
			return &EntryResult{OK: false, Message: fmt.Sprintf("stale range generation: have %d want %d", r.Generation, e.Generation)}
		}
	}
	if e.Atomic {
		clone, err := s.db.Clone()
		if err != nil {
			return &EntryResult{OK: false, Message: "clone: " + err.Error()}
		}
		affected := 0
		for i, sql := range e.SQLs {
			r, err := clone.Exec(sql)
			if err != nil {
				return &EntryResult{OK: false, Message: fmt.Sprintf("statement %d: %v", i+1, err)}
			}
			affected += r.Affected
		}
		if err := s.db.ReplaceFrom(clone); err != nil {
			return &EntryResult{OK: false, Message: "commit: " + err.Error()}
		}
		s.appendWAL(e.SQLs)
		return &EntryResult{OK: true, Affected: affected}
	}
	affected := 0
	for i, sql := range e.SQLs {
		r, err := s.db.Exec(sql)
		if err != nil {
			return &EntryResult{OK: false, Message: fmt.Sprintf("statement %d: %v", i+1, err)}
		}
		affected += r.Affected
	}
	s.appendWAL(e.SQLs)
	return &EntryResult{OK: true, Affected: affected}
}

func (s *ReplicatedState) rangeLocked(e *Entry) *EntryResult {
	if e.Op == nil {
		return &EntryResult{OK: false, Message: "range entry without op"}
	}
	if err := s.ranges.applyOpLocked(e.Op); err != nil {
		return &EntryResult{OK: false, Message: err.Error()}
	}
	return &EntryResult{OK: true}
}

// appendWAL keeps the legacy WAL file as a compatible side-copy of committed
// writes. It is advisory: Raft log + snapshots are the authority.
func (s *ReplicatedState) appendWAL(sqls []string) {
	if s.walPath == "" || len(sqls) == 0 {
		return
	}
	_ = storage.AppendWALBatch(s.walPath, sqls)
}

func (s *ReplicatedState) snapshot() (raft.FSMSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := s.db.Snapshot()
	if err != nil {
		return nil, err
	}
	img := replicatedSnapshot{
		Index:  s.index,
		DB:     raw,
		Ranges: s.ranges.allLocked(),
		Assign: s.ranges.assignLocked(),
		NextID: s.ranges.nextLocked(),
	}
	b, err := json.Marshal(img)
	if err != nil {
		return nil, err
	}
	return &fsmSnapshot{data: b}, nil
}

func (s *ReplicatedState) restore(rc io.ReadCloser) error {
	b, err := io.ReadAll(rc)
	if err != nil {
		return err
	}
	var img replicatedSnapshot
	if err := json.Unmarshal(b, &img); err != nil {
		return fmt.Errorf("decode cluster snapshot: %w", err)
	}
	fresh := engine.New()
	if len(img.DB) > 0 {
		if err := json.Unmarshal(img.DB, fresh); err != nil {
			return fmt.Errorf("restore engine state: %w", err)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.db.ReplaceFrom(fresh); err != nil {
		return err
	}
	s.ranges.restoreLocked(img.Ranges, img.Assign, img.NextID)
	s.index = img.Index
	return nil
}

type fsmSnapshot struct {
	data []byte
}

func (s *fsmSnapshot) Persist(sink raft.SnapshotSink) error {
	if _, err := sink.Write(s.data); err != nil {
		_ = sink.Cancel()
		return err
	}
	return sink.Close()
}

func (s *fsmSnapshot) Release() {}
