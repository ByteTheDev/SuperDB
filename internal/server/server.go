package server

import (
	"bufio"
	"net"
	"strings"

	"superdb/internal/engine"
)

func Handle(c net.Conn, db *engine.Database, mode, data string) {
	defer c.Close()
	if tcp, ok := c.(*net.TCPConn); ok {
		_ = tcp.SetNoDelay(true)
		_ = tcp.SetReadBuffer(256 << 10)
		_ = tcp.SetWriteBuffer(256 << 10)
	}
	r := bufio.NewReader(c)
	w := bufio.NewWriterSize(c, 64<<10)
	for {
		q, err := readRequest(r)
		if err != nil {
			return
		}
		out := execute(q, db, mode, data)
		if err := writeResponse(w, out); err != nil {
			return
		}
	}
}

func execute(q request, db *engine.Database, mode, data string) any {
	sql := strings.TrimSpace(strings.ToUpper(q.SQL))
	if sql == "BEGIN" || sql == "COMMIT" || sql == "ROLLBACK" {
		return map[string]string{"message": sql + " ok"}
	}
	res, err := db.Exec(q.SQL)
	if err != nil {
		return map[string]string{"error": err.Error()}
	}
	if mode == "wal" {
		if err := appendWAL(data, q.SQL); err != nil {
			return map[string]string{"error": "persist WAL: " + err.Error()}
		}
	} else if mode == "snapshot" {
		if err := saveSnapshot(data, db); err != nil {
			return map[string]string{"error": "save snapshot: " + err.Error()}
		}
	}
	return res
}
