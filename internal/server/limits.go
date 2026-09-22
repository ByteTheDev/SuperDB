package server

import (
	"context"
	"errors"
	"sync/atomic"
	"time"
)

// ErrBusy is returned when a request cannot be admitted because the
// server is at its configured concurrency limit. It is retryable: the
// client may retry after backoff.
var ErrBusy = errors.New("server busy: too many concurrent queries")

// defaultAcquireTimeout bounds how long a request waits for an inflight
// slot when Limits.AcquireTimeout is unset.
const defaultAcquireTimeout = 5 * time.Second

// Limits configures connection and query safety caps shared by the local
// TCP server (HandleOptions) and the hosted server (remote.Config). Every
// field is optional: zero disables that limit and preserves the historical
// unbounded behavior.
type Limits struct {
	// QueryTimeout bounds execution of a single statement. The engine
	// checks cancellation at entry and periodically inside scan loops,
	// so timed-out queries abort mid-scan. 0 disables; a negative value
	// is treated as an already-expired deadline.
	QueryTimeout time.Duration
	// MaxResultRows caps the rows a SELECT may return. Queries exceeding
	// the cap abort with a row-limit error instead of materializing an
	// unbounded result. 0 disables.
	MaxResultRows int
	// MaxResultBytes caps the encoded response size per request. Larger
	// responses are replaced with an error. 0 disables (protocol framing
	// limits still apply).
	MaxResultBytes int
	// MaxInflightQueries bounds requests executing concurrently across
	// all connections (backpressure). Excess requests wait up to
	// AcquireTimeout, then fail with ErrBusy. 0 disables.
	MaxInflightQueries int
	// AcquireTimeout bounds the wait for an inflight-query slot.
	// <= 0 uses defaultAcquireTimeout.
	AcquireTimeout time.Duration
	// MaxConnections bounds concurrent client connections. Connections
	// beyond the cap are rejected immediately. 0 disables.
	MaxConnections int
	// MaxBatchStatements bounds statements per "sqls" request.
	// 0 disables.
	MaxBatchStatements int
	// MaxTxStatements bounds write statements buffered by a session
	// transaction (BEGIN..COMMIT). Exceeding the cap aborts the
	// transaction and frees its clone. 0 disables.
	MaxTxStatements int
	// MaxTxDatabaseRows rejects BEGIN when the database holds more rows
	// than this, bounding the per-connection clone a transaction pins.
	// 0 disables.
	MaxTxDatabaseRows int
	// MaxRequestBytes caps one request payload. <= 0 uses the protocol
	// default (16 MiB).
	MaxRequestBytes int
}

// Limiter enforces the admission-control parts of Limits, shared by all
// sessions on one listener. Methods are safe on a nil receiver so
// sessions can hold a limiter unconditionally.
type Limiter struct {
	limits   Limits
	connSem  chan struct{}
	querySem chan struct{}

	rejectedConns   atomic.Uint64
	rejectedQueries atomic.Uint64
}

// NewLimiter builds a Limiter for one listener. Channels are only
// allocated for enabled caps so a zero Limits carries no overhead.
func NewLimiter(l Limits) *Limiter {
	lim := &Limiter{limits: l}
	if l.MaxConnections > 0 {
		lim.connSem = make(chan struct{}, l.MaxConnections)
	}
	if l.MaxInflightQueries > 0 {
		lim.querySem = make(chan struct{}, l.MaxInflightQueries)
	}
	return lim
}

// RejectedConns reports connections refused by MaxConnections.
func (l *Limiter) RejectedConns() uint64 {
	if l == nil {
		return 0
	}
	return l.rejectedConns.Load()
}

// RejectedQueries reports requests refused by MaxInflightQueries.
func (l *Limiter) RejectedQueries() uint64 {
	if l == nil {
		return 0
	}
	return l.rejectedQueries.Load()
}

// AcquireConn takes a connection slot. It never blocks: callers close
// the connection when it reports false.
func (l *Limiter) AcquireConn() bool {
	if l == nil || l.connSem == nil {
		return true
	}
	select {
	case l.connSem <- struct{}{}:
		return true
	default:
		l.rejectedConns.Add(1)
		return false
	}
}

// ReleaseConn returns a connection slot.
func (l *Limiter) ReleaseConn() {
	if l == nil || l.connSem == nil {
		return
	}
	<-l.connSem
}

// AcquireQuery takes an inflight-query slot, waiting up to
// AcquireTimeout (or ctx cancellation) before failing with ErrBusy.
func (l *Limiter) AcquireQuery(ctx context.Context) error {
	if l == nil || l.querySem == nil {
		return nil
	}
	wait := l.limits.AcquireTimeout
	if wait <= 0 {
		wait = defaultAcquireTimeout
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case l.querySem <- struct{}{}:
		return nil
	case <-ctx.Done():
		l.rejectedQueries.Add(1)
		return ctx.Err()
	case <-timer.C:
		l.rejectedQueries.Add(1)
		return ErrBusy
	}
}

// ReleaseQuery returns an inflight-query slot.
func (l *Limiter) ReleaseQuery() {
	if l == nil || l.querySem == nil {
		return
	}
	<-l.querySem
}
