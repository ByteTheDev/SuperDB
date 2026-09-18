package cluster

import (
	"sync"
	"time"
)

// State is the last known health of a member.
type State string

const (
	StateAlive   State = "alive"
	StateSuspect State = "suspect"
	StateLeaving State = "leaving"
	StateRemoved State = "removed"
)

// Member is one known cluster node.
type Member struct {
	ID       string    `json:"id"`
	Addr     string    `json:"addr"`
	State    State     `json:"state"`
	LastSeen time.Time `json:"last_seen"`
	Version  uint64    `json:"version"`
	Region   string    `json:"region,omitempty"`
}

// Membership is a thread-safe registry of known nodes.
type Membership struct {
	mu      sync.RWMutex
	members map[string]*Member
	self    string
	version uint64
}

// NewMembership creates a registry containing only self.
func NewMembership(selfID, selfAddr string) *Membership {
	m := &Membership{members: make(map[string]*Member), self: selfID}
	m.members[selfID] = &Member{ID: selfID, Addr: selfAddr, State: StateAlive, LastSeen: time.Now(), Version: 1}
	return m
}

// SelfID returns the local node ID.
func (m *Membership) SelfID() string { return m.self }

// Upsert adds or updates a member. Removed members stay tombstoned.
func (m *Membership) Upsert(id, addr string, state State, version uint64) {
	if id == "" || addr == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	existing, ok := m.members[id]
	if !ok {
		m.version++
		m.members[id] = &Member{ID: id, Addr: addr, State: state, LastSeen: time.Now(), Version: version}
		return
	}
	if existing.State == StateRemoved && state != StateAlive {
		return
	}
	if version >= existing.Version {
		existing.Addr = addr
		existing.State = state
		existing.Version = version
		m.version++
	}
	existing.LastSeen = time.Now()
}

// MergeBulk replaces/updates from a remote member list.
func (m *Membership) MergeBulk(remote []Member) {
	for _, r := range remote {
		if r.ID == m.self {
			continue
		}
		if r.ID == "" || r.Addr == "" {
			continue
		}
		m.UpsertMember(r)
	}
}

// UpsertMember adds or updates a member from a full descriptor.
func (m *Membership) UpsertMember(r Member) {
	if r.ID == "" || r.Addr == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	existing, ok := m.members[r.ID]
	if !ok {
		m.version++
		r.LastSeen = time.Now()
		m.members[r.ID] = &Member{ID: r.ID, Addr: r.Addr, State: r.State, LastSeen: r.LastSeen, Version: r.Version, Region: r.Region}
		return
	}
	if existing.State == StateRemoved && r.State != StateAlive {
		return
	}
	if r.Version >= existing.Version {
		existing.Addr = r.Addr
		existing.State = r.State
		existing.Version = r.Version
		if r.Region != "" {
			existing.Region = r.Region
		}
		m.version++
	} else if r.Region != "" {
		existing.Region = r.Region
	}
	existing.LastSeen = time.Now()
}

// SetRegion records the region label of a member.
func (m *Membership) SetRegion(id, region string) {
	if region == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if mb, ok := m.members[id]; ok {
		mb.Region = region
	}
}

// MarkAlive records a successful health check.
func (m *Membership) MarkAlive(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if mb, ok := m.members[id]; ok {
		mb.State = StateAlive
		mb.LastSeen = time.Now()
	}
}

// MarkSuspect records a failed health check. It never deletes the member:
// a temporary network failure is not proof of permanent disappearance.
func (m *Membership) MarkSuspect(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if mb, ok := m.members[id]; ok && mb.State == StateAlive {
		mb.State = StateSuspect
	}
}

// Remove tombstones a member. Reserved for explicit graceful removal.
func (m *Membership) Remove(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if mb, ok := m.members[id]; ok {
		mb.State = StateRemoved
		m.version++
	}
}

// Get returns a copy of a member.
func (m *Membership) Get(id string) (Member, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	mb, ok := m.members[id]
	if !ok {
		return Member{}, false
	}
	return *mb, true
}

// AddrOf returns the advertised address of a member.
func (m *Membership) AddrOf(id string) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	mb, ok := m.members[id]
	if !ok || mb.State == StateRemoved {
		return "", false
	}
	return mb.Addr, true
}

// List returns a snapshot of all non-removed members.
func (m *Membership) List() []Member {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Member, 0, len(m.members))
	for _, mb := range m.members {
		if mb.State == StateRemoved {
			continue
		}
		out = append(out, *mb)
	}
	return out
}

// Count returns the number of visible members.
func (m *Membership) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	n := 0
	for _, mb := range m.members {
		if mb.State != StateRemoved {
			n++
		}
	}
	return n
}

// SweepSuspects marks alive members suspect when not seen for longer than after.
func (m *Membership) SweepSuspects(after time.Duration, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, mb := range m.members {
		if id == m.self || mb.State != StateAlive {
			continue
		}
		if now.Sub(mb.LastSeen) > after {
			mb.State = StateSuspect
		}
	}
}
