package server

import (
	"bufio"
	"context"
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
	// Cluster, when set, routes statements through Raft quorum commits.
	// Multi-statement session transactions are rejected in this mode;
	// use atomic batches instead.
	Cluster ClusterExec
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
	r := bufio.NewReader(c)
	w := bufio.NewWriterSize(c, 64<<10)
	s := &session{db: db, walWriter: options.WALWriter, cluster: options.Cluster}
	for {
		q, err := readRequest(r)
		if err != nil {
			return
		}
		out := executeRequest(q, s, mode, data)
		if err := writeResponse(w, out); err != nil {
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
}

func (s *session) active() *engine.Database {
	if s.tx != nil {
		return s.tx
	}
	return s.db
}

func executeRequest(q request, s *session, mode, data string) any {
	if len(q.SQLs) > 0 {
		if q.Atomic && s.cluster != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			results, err := s.cluster.ExecAtomic(ctx, q.SQLs)
			if err != nil {
				return map[string]string{"error": err.Error()}
			}
			out := make([]any, 0, len(results))
			for _, r := range results {
				out = append(out, r)
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

func executeSQL(raw string, s *session, mode, data string) any {
	if s.cluster != nil {
		upper := strings.TrimSpace(strings.ToUpper(raw))
		if upper == "BEGIN" || upper == "COMMIT" || upper == "ROLLBACK" {
			return map[string]string{"error": "session transactions are not supported in cluster mode; send atomic batches instead"}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		res, err := s.cluster.Exec(ctx, raw)
		if err != nil {
			return map[string]string{"error": err.Error()}
		}
		return res
	}
	sql := strings.TrimSpace(strings.ToUpper(raw))
	if sql == "BEGIN" {
		if s.tx != nil {
			return map[string]string{"error": "transaction already active"}
		}
		clone, err := s.db.Clone()
		if err != nil {
			return map[string]string{"error": "begin: " + err.Error()}
		}
		s.tx = clone
		s.txSQL = nil
		return map[string]string{"message": "BEGIN ok"}
	}
	if sql == "ROLLBACK" {
		if s.tx == nil {
			return map[string]string{"error": "no transaction active"}
		}
		s.tx = nil
		s.txSQL = nil
		return map[string]string{"message": "ROLLBACK ok"}
	}
	if sql == "COMMIT" {
		if s.tx == nil {
			return map[string]string{"error": "no transaction active"}
		}
		if err := s.db.ReplaceFrom(s.tx); err != nil {
			return map[string]string{"error": "commit: " + err.Error()}
		}
		if err := persistStatements(mode, data, s.txSQL, s.db, s.walWriter); err != nil {
			return map[string]string{"error": "persist transaction: " + err.Error()}
		}
		s.tx = nil
		s.txSQL = nil
		return map[string]string{"message": "COMMIT ok"}
	}
	db := s.active()
	res, err := db.Exec(raw)
	if err != nil {
		return map[string]string{"error": err.Error()}
	}
	if s.tx != nil {
		s.txSQL = append(s.txSQL, raw)
	} else if s.batch {
		s.pendingPersist = append(s.pendingPersist, raw)
	} else if err := persistStatements(mode, data, []string{raw}, s.db, s.walWriter); err != nil {
		return map[string]string{"error": "persist: " + err.Error()}
	}
	return res
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
