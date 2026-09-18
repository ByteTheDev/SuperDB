package cluster

import (
	"sort"
	"sync"
)

// Range describes one keyspace partition. Initially a cluster holds a single
// range covering the entire keyspace (StartKey "" to EndKey "").
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

// RangeStore is a thread-safe range directory.
type RangeStore struct {
	mu     sync.RWMutex
	ranges map[uint64]Range
	nextID uint64
}

// NewRangeStore creates an empty range directory.
func NewRangeStore() *RangeStore {
	return &RangeStore{ranges: make(map[uint64]Range), nextID: 1}
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
