package server

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"superdb/internal/engine"
	"superdb/internal/storage"
)

type request struct {
	SQL string `json:"sql"`
}

func ReplayWAL(dir string, db *engine.Database) error {
	apply := func(sql string) error {
		if _, err := db.Exec(sql); err != nil {
			return fmt.Errorf("replay WAL: %w", err)
		}
		return nil
	}
	path := storage.WALPath(dir)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		path = storage.LegacyWALPath(dir)
	}
	return storage.ReplayWAL(path, apply)
}

func LoadSnapshot(dir string, db *engine.Database) error {
	loaded, err := storage.ReadSnapshot(storage.SnapshotPath(dir), db)
	if err != nil {
		return err
	}
	if !loaded {
		_, err = storage.ReadSnapshot(storage.LegacySnapshotPath(dir), db)
	}
	return err
}

func Handle(c net.Conn, db *engine.Database, mode, data string) {
	defer c.Close()
	r := bufio.NewReader(c)
	for {
		h := make([]byte, 4)
		if _, e := io.ReadFull(r, h); e != nil {
			return
		}
		n := binary.BigEndian.Uint32(h)
		if n > 16<<20 {
			return
		}
		p := make([]byte, n)
		if _, e := io.ReadFull(r, p); e != nil {
			return
		}
		var q request
		if json.Unmarshal(p, &q) != nil {
			return
		}
		var out any
		sql := strings.TrimSpace(strings.ToUpper(q.SQL))
		if sql == "BEGIN" || sql == "COMMIT" || sql == "ROLLBACK" {
			out = map[string]string{"message": sql + " ok"}
		} else {
			res, e := db.Exec(q.SQL)
			if e != nil {
				out = map[string]string{"error": e.Error()}
			} else {
				out = res
				if mode == "wal" {
					if e := appendWAL(data, q.SQL); e != nil {
						out = map[string]string{"error": "persist WAL: " + e.Error()}
					}
				} else if mode == "snapshot" {
					if e := saveSnapshot(data, db); e != nil {
						out = map[string]string{"error": "save snapshot: " + e.Error()}
					}
				}
			}
		}
		b, _ := json.Marshal(out)
		binary.BigEndian.PutUint32(h, uint32(len(b)))
		c.Write(h)
		c.Write(b)
	}
}
func appendWAL(dir, sql string) error {
	return storage.AppendWAL(storage.WALPath(dir), sql)
}

func saveSnapshot(dir string, db *engine.Database) error {
	return storage.WriteSnapshot(storage.SnapshotPath(dir), db)
}
