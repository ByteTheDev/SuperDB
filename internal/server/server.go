package server

import (
	"bufio"
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"superdb/internal/engine"
	"superdb/internal/storage"
)

func Handle(c net.Conn, db *engine.Database, mode, data string) {
	HandleWithOptions(c, db, mode, data, HandleOptions{})
}

type HandleOptions struct {
	Production bool
	WALWriter  *storage.WALWriter
	// AuthKey, when non-empty, requires every request to carry a matching
	// "auth" (or "auth_key") field. Mismatches are rejected before any
	// SQL executes. Empty disables authentication (backward compatible).
	AuthKey string
	// Cluster, when set, routes statements through Raft quorum commits.
	// Multi-statement session transactions are rejected in this mode;
	// use atomic batches instead.
	Cluster ClusterExec
	// Limits bounds per-request resource use (timeouts, result size,
	// batch and transaction caps). Zero fields disable each cap.
	Limits Limits
	// Limiter applies MaxConnections/MaxInflightQueries admission control.
	// It must be created once per listener (NewLimiter) and shared by
	// every connection. Nil disables admission control.
	Limiter *Limiter
}

// ClusterExec is the subset of cluster.Node used by SQL sessions.
type ClusterExec interface {
	Exec(ctx context.Context, sql string) (engine.Result, error)
	ExecAtomic(ctx context.Context, sqls []string) ([]engine.Result, error)
}

func HandleWithOptions(c net.Conn, db *engine.Database, mode, data string, options HandleOptions) {
	defer c.Close()
	if tcp, ok := c.(*net.TCPConn); ok {
		_ = tcp.SetNoDelay(true)
		if options.Production {
			_ = tcp.SetKeepAlive(true)
			_ = tcp.SetKeepAlivePeriod(30 * time.Second)
			_ = tcp.SetReadBuffer(4 << 20)
			_ = tcp.SetWriteBuffer(4 << 20)
		} else {
			_ = tcp.SetReadBuffer(256 << 10)
			_ = tcp.SetWriteBuffer(256 << 10)
		}
	}
	r := bufio.NewReaderSize(c, 64<<10)
	w := bufio.NewWriterSize(c, 64<<10)
	if !options.Limiter.AcquireConn() {
		_ = writeResponse(w, map[string]string{"error": "connection refused: server at connection limit"})
		return
	}
	defer options.Limiter.ReleaseConn()
	s := &session{db: db, walWriter: options.WALWriter, cluster: options.Cluster, limits: options.Limits, limiter: options.Limiter}
	for {
		q, err := readRequest(r, options.Limits.MaxRequestBytes)
		if err != nil {
			return
		}
		if !checkAuth(q.Auth, options.AuthKey) {
			out := map[string]string{"error": "unauthorized: invalid or missing auth key"}
			if err := writeResponse(w, out); err != nil {
				return
			}
			continue
		}
		out := executeRequest(q, s, mode, data)
		if err := writeResponseLimit(w, out, options.Limits.MaxResultBytes); err != nil {
			return
		}
	}
}

type session struct {
	db             *engine.Database
	tx             *engine.Database
	txSQL          []string
	batch          bool
	pendingPersist []string
	walWriter      *storage.WALWriter
	cluster        ClusterExec
	limits         Limits
	limiter        *Limiter
}

func (s *session) active() *engine.Database {
	if s.tx != nil {
		return s.tx
	}
	return s.db
}

func executeRequest(q request, s *session, mode, data string) any {
	// Admission control: one inflight slot per request (a batch counts
	// once). Waiting longer than AcquireTimeout fails fast with ErrBusy
	// so an overloaded node sheds load instead of queueing unboundedly.
	if err := s.limiter.AcquireQuery(context.Background()); err != nil {
		return map[string]string{"error": err.Error()}
	}
	defer s.limiter.ReleaseQuery()
	if len(q.SQLs) > 0 {
		if lim := s.limits.MaxBatchStatements; lim > 0 && len(q.SQLs) > lim {
			return map[string]string{"error": fmt.Sprintf("batch exceeds maximum of %d statements", lim)}
		}
		if q.Atomic && s.cluster != nil {
			ctx, cancel := context.WithTimeout(context.Background(), clusterTimeout(s.limits))
			defer cancel()
			results, err := s.cluster.ExecAtomic(ctx, q.SQLs)
			if err != nil {
				return map[string]string{"error": err.Error()}
			}
			out := make([]any, 0, len(results))
			for _, r := range results {
				out = append(out, capResultRows(r, s.limits))
			}
			return out
		}
		s.batch = true
		s.pendingPersist = nil
		out := make([]any, 0, len(q.SQLs))
		for _, sql := range q.SQLs {
			out = append(out, executeSQL(sql, s, mode, data))
		}
		s.batch = false
		if err := persistStatements(mode, data, s.pendingPersist, s.db, s.walWriter); err != nil {
			out = append(out, map[string]string{"error": "persist batch: " + err.Error()})
		}
		s.pendingPersist = nil
		return out
	}
	return executeSQL(q.SQL, s, mode, data)
}

// clusterTimeout returns the Raft round-trip budget: the configured
// per-statement timeout when set, else the historical 30s default.
func clusterTimeout(l Limits) time.Duration {
	if l.QueryTimeout > 0 {
		return l.QueryTimeout
	}
	return 30 * time.Second
}

// queryContext applies the per-statement timeout. Zero leaves execution
// unbounded. WithTimeout cancels synchronously when the deadline has
// already passed, so an already-expired timeout is honored even before
// the engine begins work.
func queryContext(l Limits) (context.Context, context.CancelFunc) {
	if l.QueryTimeout == 0 {
		return context.Background(), func() {}
	}
	return context.WithTimeout(context.Background(), l.QueryTimeout)
}

func executeSQL(raw string, s *session, mode, data string) any {
	if s.cluster != nil {
		trimmed := strings.TrimSpace(raw)
		if equalFold(trimmed, "BEGIN") || equalFold(trimmed, "COMMIT") || equalFold(trimmed, "ROLLBACK") {
			return map[string]string{"error": "session transactions are not supported in cluster mode; send atomic batches instead"}
		}
		ctx, cancel := context.WithTimeout(context.Background(), clusterTimeout(s.limits))
		defer cancel()
		res, err := s.cluster.Exec(ctx, raw)
		if err != nil {
			return map[string]string{"error": err.Error()}
		}
		return capResultRows(res, s.limits)
	}
	sql := strings.TrimSpace(raw)
	if equalFold(sql, "BEGIN") {
		if s.tx != nil {
			return map[string]string{"error": "transaction already active"}
		}
		// A session transaction clones the whole database; reject BEGIN
		// beyond the row budget so one connection cannot pin a huge copy.
		if lim := s.limits.MaxTxDatabaseRows; lim > 0 && s.db.RowCount() > lim {
			return map[string]string{"error": fmt.Sprintf("transaction rejected: database holds %d rows (limit %d)", s.db.RowCount(), lim)}
		}
		clone, err := s.db.Clone()
		if err != nil {
			return map[string]string{"error": "begin: " + err.Error()}
		}
		s.tx = clone
		s.txSQL = nil
		return map[string]string{"message": "BEGIN ok"}
	}
	if equalFold(sql, "ROLLBACK") {
		if s.tx == nil {
			return map[string]string{"error": "no transaction active"}
		}
		s.tx = nil
		s.txSQL = nil
		return map[string]string{"message": "ROLLBACK ok"}
	}
	if equalFold(sql, "COMMIT") {
		if s.tx == nil {
			return map[string]string{"error": "no transaction active"}
		}
		writes := writeStatements(s.txSQL)
		if len(writes) == 0 {
			s.tx = nil
			s.txSQL = nil
			return map[string]string{"message": "COMMIT ok"}
		}
		// Persist-before-publish with conflict detection: the WAL/snapshot
		// is written from the committed image first, then the image is
		// published only if no other writer committed since BEGIN.
		if err := s.db.CommitFrom(s.tx, func(img *engine.Database) error {
			return persistStatements(mode, data, writes, img, s.walWriter)
		}); err != nil {
			return map[string]string{"error": "commit: " + err.Error()}
		}
		s.tx = nil
		s.txSQL = nil
		return map[string]string{"message": "COMMIT ok"}
	}
	ctx, cancel := queryContext(s.limits)
	defer cancel()
	db := s.active()
	res, err := db.ExecWithOptions(ctx, raw, engine.ExecOptions{MaxRows: s.limits.MaxResultRows})
	if err != nil {
		return map[string]string{"error": execErrorMessage(err)}
	}
	if s.tx != nil {
		if isWriteSQL(raw) {
			if lim := s.limits.MaxTxStatements; lim > 0 && len(s.txSQL) >= lim {
				// Aborting frees the session clone, the dominant
				// per-connection memory cost of an oversized tx.
				s.tx = nil
				s.txSQL = nil
				return map[string]string{"error": fmt.Sprintf("transaction aborted: exceeded %d buffered statements", lim)}
			}
			s.txSQL = append(s.txSQL, raw)
		}
	} else if s.batch {
		if isWriteSQL(raw) {
			s.pendingPersist = append(s.pendingPersist, raw)
		}
	} else if isWriteSQL(raw) {
		if err := persistStatements(mode, data, []string{raw}, s.db, s.walWriter); err != nil {
			return map[string]string{"error": "persist: " + err.Error()}
		}
	}
	return res
}

// capResultRows enforces Limits.MaxResultRows on results produced outside
// the local engine path (cluster execs cannot carry ExecOptions through
// deterministic Raft apply). Local statements are already capped inside
// the engine, so this is a second, cheap boundary check.
func capResultRows(res engine.Result, l Limits) any {
	if l.MaxResultRows > 0 && len(res.Rows) > l.MaxResultRows {
		return map[string]string{"error": fmt.Sprintf("result row limit exceeded: %d rows (limit %d)", len(res.Rows), l.MaxResultRows)}
	}
	return res
}

// execErrorMessage maps context failures to stable client-facing text.
func execErrorMessage(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "query timeout exceeded"
	case errors.Is(err, context.Canceled):
		return "query canceled"
	default:
		return err.Error()
	}
}

// checkAuth reports whether the supplied key satisfies the required key.
// Empty required disables authentication. Comparison is constant-time to
// avoid leaking the key via timing.
func checkAuth(supplied, required string) bool {
	if required == "" {
		return true
	}
	return subtle.ConstantTimeCompare([]byte(supplied), []byte(required)) == 1
}

// isWriteSQL mirrors engine.Database.Exec's read classification: only
// SELECT statements skip durability. Everything else (including invalid
// SQL, which never reaches persistence because Exec errors first) is a
// write.
func isWriteSQL(raw string) bool {
	s := strings.TrimSpace(raw)
	if len(s) < 6 {
		return true
	}
	for i := 0; i < 6; i++ {
		c := s[i]
		if 'a' <= c && c <= 'z' {
			c -= 'a' - 'A'
		}
		if c != "SELECT"[i] {
			return true
		}
	}
	return false
}

// writeStatements drops read-only statements from a recorded batch so the
// WAL only carries statements that can mutate state.
func writeStatements(sqls []string) []string {
	out := sqls[:0]
	for _, sql := range sqls {
		if isWriteSQL(sql) {
			out = append(out, sql)
		}
	}
	return out
}

// equalFold reports whether s equals keyword case-insensitively without
// allocating the full upper-cased query string.
func equalFold(s, keyword string) bool {
	if len(s) != len(keyword) {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		k := keyword[i]
		if 'a' <= c && c <= 'z' {
			c -= 'a' - 'A'
		}
		if c != k {
			return false
		}
	}
	return true
}

func persistStatements(mode, data string, sqls []string, db *engine.Database, writer *storage.WALWriter) error {
	// Cluster sessions return before recording pending statements, so sqls
	// is empty here in cluster mode: Raft already appended the WAL side-copy
	// on every member during commit.
	if mode == "wal" {
		if writer != nil {
			return writer.Append(sqls)
		}
		return appendWALBatch(data, sqls)
	} else if mode == "snapshot" && len(sqls) > 0 {
		return saveSnapshot(data, db)
	}
	return nil
}
