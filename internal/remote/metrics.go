package remote

import (
	"sync/atomic"
	"time"
)

// Metrics tracks server observability counters. All fields are atomically
// updated so concurrent connections never corrupt them.
type Metrics struct {
	activeConns     atomic.Int64
	totalConns      atomic.Int64
	rejectedConns   atomic.Int64
	queries         atomic.Uint64
	failedQueries   atomic.Uint64
	rejectedQueries atomic.Uint64
	authFailures    atomic.Uint64
	bytesRx         atomic.Uint64
	bytesTx         atomic.Uint64
	latencyNanos    atomic.Uint64
	startedAt       time.Time
}

// Snapshot is a point-in-time metrics view safe to expose.
type Snapshot struct {
	ActiveConns     int64   `json:"active_connections"`
	TotalConns      int64   `json:"total_connections"`
	RejectedConns   int64   `json:"rejected_connections"`
	Queries         uint64  `json:"queries"`
	FailedQueries   uint64  `json:"failed_queries"`
	RejectedQueries uint64  `json:"rejected_queries"`
	AuthFailures    uint64  `json:"auth_failures"`
	BytesRx         uint64  `json:"bytes_received"`
	BytesTx         uint64  `json:"bytes_sent"`
	AvgLatencyMs    float64 `json:"avg_latency_ms"`
	UptimeSec       int64   `json:"uptime_sec"`
}

// Snapshot copies current counters.
func (m *Metrics) Snapshot() Snapshot {
	q := m.queries.Load()
	avg := 0.0
	if q > 0 {
		avg = float64(m.latencyNanos.Load()) / float64(q) / 1e6
	}
	up := int64(0)
	if !m.startedAt.IsZero() {
		up = int64(time.Since(m.startedAt).Seconds())
	}
	return Snapshot{
		ActiveConns:     m.activeConns.Load(),
		TotalConns:      m.totalConns.Load(),
		RejectedConns:   m.rejectedConns.Load(),
		Queries:         q,
		FailedQueries:   m.failedQueries.Load(),
		RejectedQueries: m.rejectedQueries.Load(),
		AuthFailures:    m.authFailures.Load(),
		BytesRx:         m.bytesRx.Load(),
		BytesTx:         m.bytesTx.Load(),
		AvgLatencyMs:    avg,
		UptimeSec:       up,
	}
}
