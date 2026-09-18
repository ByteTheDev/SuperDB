package cluster

import (
	"fmt"
	"sort"
	"sync"
)

// Range describes one keyspace partition.
type Range struct {
	ID         uint64   `json:"id"`
	StartKey   string   `json:"start_key"`
	EndKey     string   `json:"end_key"`
	Replicas   []string `json:"replicas"`
	Leader     string   `json:"leader"`
	Generation uint64   `json:"generation"`
}

// Contains reports whether key belongs to this range.
// Empty EndKey means unbounded above.
func (r Range) Contains(key string) bool {
	if key < r.StartKey {
		return false
	}
	if r.EndKey != "" && key >= r.EndKey {
		return false
	}
	return true
}

// HasReplica reports whether nodeID is a replica of this range.
func (r Range) HasReplica(nodeID string) bool {
	for _, n := range r.Replicas {
		if n == nodeID {
			return true
		}
	}
	return false
}

// RangeStore is a thread-safe range directory plus the table->range
// assignment map. Mutations arrive only via committed Raft entries so every
// member converges on the same directory.
type RangeStore struct {
	mu     sync.RWMutex
	ranges map[uint64]Range
	assign map[string]uint64
	nextID uint64
}

// NewRangeStore creates an empty range directory.
func NewRangeStore() *RangeStore {
	return &RangeStore{ranges: make(map[uint64]Range), assign: make(map[string]uint64), nextID: 1}
}

// EnsureSingleRange creates the initial full-keyspace range when none exists.
func (s *RangeStore) EnsureSingleRange(nodeID string) Range {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.ranges) > 0 {
		for _, r := range s.ranges {
			return r
		}
	}
	r := Range{ID: s.nextID, StartKey: "", EndKey: "", Replicas: []string{nodeID}, Leader: nodeID, Generation: 1}
	s.ranges[r.ID] = r
	s.nextID++
	return r
}

// All returns ranges ordered by ID.
func (s *RangeStore) All() []Range {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.allLocked()
}

func (s *RangeStore) allLocked() []Range {
	out := make([]Range, 0, len(s.ranges))
	for _, r := range s.ranges {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Lookup finds the range owning key.
func (s *RangeStore) Lookup(key string) (Range, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.ranges {
		if r.Contains(key) {
			return r, nil
		}
	}
	return Range{}, NewError(CodeRangeNotFound, "no range owns key")
}

// ByID returns one range by ID.
func (s *RangeStore) ByID(id uint64) (Range, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lookupLocked(id)
}

func (s *RangeStore) lookupLocked(id uint64) (Range, error) {
	r, ok := s.ranges[id]
	if !ok {
		return Range{}, NewError(CodeRangeNotFound, fmt.Sprintf("range %d not found", id))
	}
	return r, nil
}

// TableRange resolves the range currently serving a table. Unassigned
// tables deterministically fall back to the lowest-ID range.
func (s *RangeStore) TableRange(table string) Range {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if id, ok := s.assign[table]; ok {
		if r, ok := s.ranges[id]; ok {
			return r
		}
	}
	var best Range
	first := true
	for _, r := range s.ranges {
		if first || r.ID < best.ID {
			best, first = r, false
		}
	}
	return best
}

// Assignment returns a copy of the table->range map.
func (s *RangeStore) Assignment() map[string]uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.assignLocked()
}

func (s *RangeStore) assignLocked() map[string]uint64 {
	out := make(map[string]uint64, len(s.assign))
	for k, v := range s.assign {
		out[k] = v
	}
	return out
}

func (s *RangeStore) nextLocked() uint64 { return s.nextID }

// nextRangeID returns the next allocatable range ID.
func (s *RangeStore) nextRangeID() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.nextID
}

// ReplaceBulk replaces the directory from a remote snapshot (join path).
func (s *RangeStore) ReplaceBulk(ranges []Range) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ranges = make(map[uint64]Range, len(ranges))
	max := uint64(0)
	for _, r := range ranges {
		s.ranges[r.ID] = r
		if r.ID > max {
			max = r.ID
		}
	}
	s.nextID = max + 1
	if s.nextID == 0 {
		s.nextID = 1
	}
}

// restore replaces directory, assignment, and ID counter from a Raft
// snapshot image.
func (s *RangeStore) restore(ranges []Range, assign map[string]uint64, nextID uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.restoreLocked(ranges, assign, nextID)
}

func (s *RangeStore) restoreLocked(ranges []Range, assign map[string]uint64, nextID uint64) {
	s.ranges = make(map[uint64]Range, len(ranges))
	max := uint64(0)
	for _, r := range ranges {
		s.ranges[r.ID] = r
		if r.ID > max {
			max = r.ID
		}
	}
	s.assign = make(map[string]uint64, len(assign))
	for k, v := range assign {
		s.assign[k] = v
	}
	s.nextID = nextID
	if s.nextID <= max {
		s.nextID = max + 1
	}
	if s.nextID == 0 {
		s.nextID = 1
	}
}

// UpdateLeader records a leader change and bumps generation.
func (s *RangeStore) UpdateLeader(rangeID uint64, leader string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.ranges[rangeID]
	if !ok {
		return NewError(CodeRangeNotFound, "range not found")
	}
	r.Leader = leader
	r.Generation++
	s.ranges[rangeID] = r
	return nil
}

// SetRangeLeader tracks the Raft leader for serving without bumping the
// fencing generation: elections are not range moves.
func (s *RangeStore) SetRangeLeader(rangeID uint64, leader string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.ranges[rangeID]; ok {
		r.Leader = leader
		s.ranges[rangeID] = r
	}
}

// applyOp applies a committed RangeOp verbatim after fencing checks.
// It takes the directory lock itself; callers must not hold it.
func (s *RangeStore) applyOp(op *RangeOp) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.applyOpLocked(op)
}

func (s *RangeStore) applyOpLocked(op *RangeOp) error {
	for id, gen := range op.BaseGenerations {
		r, ok := s.ranges[id]
		if !ok {
			return NewError(CodeRangeNotFound, fmt.Sprintf("range %d not found", id))
		}
		if r.Generation != gen {
			return fmt.Errorf("stale range generation for %d: have %d want %d", id, r.Generation, gen)
		}
	}
	for _, id := range op.Remove {
		delete(s.ranges, id)
	}
	for _, r := range op.Ranges {
		if r.ID == 0 {
			r.ID = s.nextID
			s.nextID++
		} else if r.ID >= s.nextID {
			s.nextID = r.ID + 1
		}
		s.ranges[r.ID] = r
	}
	for t, id := range op.Assign {
		s.assign[t] = id
	}
	for _, t := range op.Unassign {
		delete(s.assign, t)
	}
	return nil
}
