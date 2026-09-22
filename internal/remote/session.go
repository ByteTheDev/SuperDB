package remote

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"superdb/internal/cluster"
	"superdb/internal/engine"
	"superdb/internal/server"
	"superdb/internal/storage"
	"superdb/internal/wire"
)

// ClusterExec is the subset of cluster.Node used by remote sessions.
// It mirrors server.ClusterExec so there is one routing behavior, not two.
type ClusterExec interface {
	Exec(ctx context.Context, sql string) (engine.Result, error)
	ExecAtomic(ctx context.Context, sqls []string) ([]engine.Result, error)
}

// session holds per-connection state: auth status plus an optional
// session-local transaction clone. Connections are served by a single
// reader loop each, so no mutex is needed here; the connection write
// path is only ever used by that loop, keeping frames serialized.
type session struct {
	db        *engine.Database
	tx        *engine.Database
	txSQL     []string
	walWriter *storage.WALWriter
	cluster   ClusterExec
	mode      string
	dataDir   string
	limits    server.Limits
	authed    bool
	database  string
	authFails int
}

func (s *session) active() *engine.Database {
	if s.tx != nil {
		return s.tx
	}
	return s.db
}

// bind resolves ? params server-side with the shared safe binder.
func bind(sql string, params []any) (string, error) {
	if len(params) == 0 {
		if engine.PlaceholderCount(sql) > 0 {
			return "", errMissingParams()
		}
		return sql, nil
	}
	return engine.BindParams(sql, params)
}

// execOne executes a single already-bound statement honoring ctx
// cancellation and the session's configured limits.
func (s *session) execOne(ctx context.Context, raw string) (engine.Result, string, error) {
	if s.cluster != nil {
		trimmed := strings.TrimSpace(raw)
		if isTxKeyword(trimmed) {
			return engine.Result{}, "", &execError{Code: "UNSUPPORTED", Msg: "session transactions are not supported in cluster mode; send atomic batches instead"}
		}
		timeout := s.limits.QueryTimeout
		if timeout <= 0 {
			timeout = 30 * time.Second
		}
		qctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		res, err := s.cluster.Exec(qctx, raw)
		if err != nil {
			return engine.Result{}, "", mapClusterError(err)
		}
		if lim := s.limits.MaxResultRows; lim > 0 && len(res.Rows) > lim {
			return engine.Result{}, "", &execError{Code: wire.ErrResultTooLarge, Msg: fmt.Sprintf("result row limit exceeded: %d rows (limit %d)", len(res.Rows), lim)}
		}
		return res, "", nil
	}
	sql := strings.TrimSpace(raw)
	if equalFold(sql, "BEGIN") {
		if s.tx != nil {
			return engine.Result{}, "", &execError{Code: "INVALID_REQUEST", Msg: "transaction already active"}
		}
		// A session transaction clones the whole database; reject BEGIN
		// beyond the row budget so one connection cannot pin a huge copy.
		if lim := s.limits.MaxTxDatabaseRows; lim > 0 && s.db.RowCount() > lim {
			return engine.Result{}, "", &execError{Code: wire.ErrBusy, Msg: fmt.Sprintf("transaction rejected: database holds %d rows (limit %d)", s.db.RowCount(), lim), Retryable: true}
		}
		clone, err := s.db.Clone()
		if err != nil {
			return engine.Result{}, "", &execError{Code: "INTERNAL_ERROR", Msg: "begin failed"}
		}
		s.tx = clone
		s.txSQL = nil
		return engine.Result{Message: "BEGIN ok"}, "", nil
	}
	if equalFold(sql, "ROLLBACK") {
		if s.tx == nil {
			return engine.Result{}, "", &execError{Code: "INVALID_REQUEST", Msg: "no transaction active"}
		}
		s.tx = nil
		s.txSQL = nil
		return engine.Result{Message: "ROLLBACK ok"}, "", nil
	}
	if equalFold(sql, "COMMIT") {
		if s.tx == nil {
			return engine.Result{}, "", &execError{Code: "INVALID_REQUEST", Msg: "no transaction active"}
		}
		writes := writeStatements(s.txSQL)
		if len(writes) == 0 {
			s.tx = nil
			s.txSQL = nil
			return engine.Result{Message: "COMMIT ok"}, "", nil
		}
		if err := s.db.CommitFrom(s.tx, func(img *engine.Database) error {
			return persistStatements(s.mode, s.dataDir, writes, img, s.walWriter)
		}); err != nil {
			return engine.Result{}, "", &execError{Code: "QUERY_ERROR", Msg: "commit failed"}
		}
		s.tx = nil
		s.txSQL = nil
		return engine.Result{Message: "COMMIT ok"}, "", nil
	}
	db := s.active()
	res, err := db.ExecWithOptions(ctx, raw, engine.ExecOptions{MaxRows: s.limits.MaxResultRows})
	if err != nil {
		return engine.Result{}, "", mapEngineError(err)
	}
	if s.tx != nil {
		if isWriteSQL(raw) {
			if lim := s.limits.MaxTxStatements; lim > 0 && len(s.txSQL) >= lim {
				// Aborting frees the session clone, the dominant
				// per-connection memory cost of an oversized tx.
				s.tx = nil
				s.txSQL = nil
				return engine.Result{}, "", &execError{Code: "INVALID_REQUEST", Msg: fmt.Sprintf("transaction aborted: exceeded %d buffered statements", lim)}
			}
			s.txSQL = append(s.txSQL, raw)
		}
	} else if isWriteSQL(raw) {
		if err := persistStatements(s.mode, s.dataDir, []string{raw}, s.db, s.walWriter); err != nil {
			return engine.Result{}, "", &execError{Code: "INTERNAL_ERROR", Msg: "persist failed"}
		}
	}
	return res, "", nil
}

// mapEngineError converts engine failures — including context timeouts,
// cancellation, and result-limit violations — to stable wire codes.
func mapEngineError(err error) error {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return &execError{Code: wire.ErrTimeout, Msg: "query timeout exceeded", Retryable: true}
	case errors.Is(err, context.Canceled):
		return &execError{Code: wire.ErrTimeout, Msg: "query canceled", Retryable: true}
	case errors.Is(err, engine.ErrResultTooLarge):
		return &execError{Code: wire.ErrResultTooLarge, Msg: err.Error()}
	default:
		return &execError{Code: queryErrorCode(err), Msg: err.Error()}
	}
}

type execError struct {
	Code       string
	Msg        string
	LeaderAddr string
	Retryable  bool
}

func (e *execError) Error() string { return e.Code + ": " + e.Msg }

func errMissingParams() error {
	return &execError{Code: "INVALID_REQUEST", Msg: "query has placeholders but no params were supplied"}
}

// mapClusterError converts cluster errors to stable wire codes without
// leaking internal details.
func mapClusterError(err error) error {
	if ce, ok := cluster.AsError(err); ok {
		switch ce.Code {
		case cluster.CodeNoLeader, cluster.CodeNoQuorum:
			return &execError{Code: "NOT_LEADER", Msg: "no leader available for write", Retryable: true}
		case cluster.CodeInvalidArgument:
			return &execError{Code: "INVALID_REQUEST", Msg: ce.Message}
		case cluster.CodeNotFound, cluster.CodeRangeNotFound:
			return &execError{Code: "NOT_FOUND", Msg: ce.Message}
		default:
			return &execError{Code: "QUERY_ERROR", Msg: ce.Message}
		}
	}
	msg := err.Error()
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "no leader"):
		return &execError{Code: "NOT_LEADER", Msg: "no leader available for write", Retryable: true}
	case strings.Contains(lower, "table not found"):
		return &execError{Code: "NOT_FOUND", Msg: msg}
	case strings.Contains(lower, "unsupported"):
		return &execError{Code: "UNSUPPORTED", Msg: msg}
	default:
		return &execError{Code: "QUERY_ERROR", Msg: msg}
	}
}

func queryErrorCode(err error) string {
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "table not found"):
		return "NOT_FOUND"
	case strings.Contains(msg, "unsupported"):
		return "UNSUPPORTED"
	default:
		return "QUERY_ERROR"
	}
}

func isTxKeyword(s string) bool {
	return equalFold(s, "BEGIN") || equalFold(s, "COMMIT") || equalFold(s, "ROLLBACK")
}

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

func writeStatements(sqls []string) []string {
	out := sqls[:0]
	for _, sql := range sqls {
		if isWriteSQL(sql) {
			out = append(out, sql)
		}
	}
	return out
}

func persistStatements(mode, data string, sqls []string, db *engine.Database, writer *storage.WALWriter) error {
	if mode == "wal" {
		if writer != nil {
			return writer.Append(sqls)
		}
		return storage.AppendWALBatch(storage.WALPath(data), sqls)
	} else if mode == "snapshot" && len(sqls) > 0 {
		return storage.WriteSnapshot(storage.SnapshotPath(data), db)
	}
	return nil
}
