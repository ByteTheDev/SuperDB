package server

import (
	"bufio"
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
	s := &session{db: db, walWriter: options.WALWriter}
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
}

func (s *session) active() *engine.Database {
	if s.tx != nil {
		return s.tx
	}
	return s.db
}

func executeRequest(q request, s *session, mode, data string) any {
	if len(q.SQLs) > 0 {
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
