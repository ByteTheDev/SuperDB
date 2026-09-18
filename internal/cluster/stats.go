package cluster

import (
	"strings"
	"sync/atomic"
	"time"
)

// Stats holds observability counters. All hot-path updates use atomics so
// observability stays outside the critical storage path.
type Stats struct {
	requests  atomic.Uint64
	reads     atomic.Uint64
	writes    atomic.Uint64
	forwards  atomic.Uint64
	errors    atomic.Uint64
	latencyNs atomic.Uint64
	pings     atomic.Uint64
	failures  atomic.Uint64
	startedAt time.Time
}

// NewStats creates counters anchored at now.
func NewStats() *Stats { return &Stats{startedAt: time.Now()} }

// Observe records one completed request.
func (s *Stats) Observe(sql string, d time.Duration, forwarded bool, failed bool) {
	s.requests.Add(1)
	s.latencyNs.Add(uint64(d.Nanoseconds()))
	if forwarded {
		s.forwards.Add(1)
	}
	if failed {
		s.errors.Add(1)
	}
	upper := strings.ToUpper(strings.TrimSpace(sql))
	if strings.HasPrefix(upper, "SELECT") {
		s.reads.Add(1)
	} else {
		s.writes.Add(1)
	}
}

// ObservePing records a health-check outcome.
func (s *Stats) ObservePing(failed bool) {
	s.pings.Add(1)
	if failed {
		s.failures.Add(1)
	}
}

// Snapshot is a point-in-time copy for status endpoints.
type Snapshot struct {
	Requests     uint64  `json:"requests"`
	Reads        uint64  `json:"reads"`
	Writes       uint64  `json:"writes"`
	Forwards     uint64  `json:"forwards"`
	Errors       uint64  `json:"errors"`
	Pings        uint64  `json:"pings"`
	Failures     uint64  `json:"failures"`
	AvgLatencyMs float64 `json:"avg_latency_ms"`
	UptimeSec    int64   `json:"uptime_sec"`
}

// Load copies current counters.
func (s *Stats) Load() Snapshot {
	req := s.requests.Load()
	avg := 0.0
	if req > 0 {
		avg = float64(s.latencyNs.Load()) / float64(req) / 1e6
	}
	return Snapshot{
		Requests:     req,
		Reads:        s.reads.Load(),
		Writes:       s.writes.Load(),
		Forwards:     s.forwards.Load(),
		Errors:       s.errors.Load(),
		Pings:        s.pings.Load(),
		Failures:     s.failures.Load(),
		AvgLatencyMs: avg,
		UptimeSec:    int64(time.Since(s.startedAt).Seconds()),
	}
}
