package storage

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// AppendWAL appends one independently compressed SQL record to a SUPERDB WAL.
func AppendWAL(path, sql string) error {
	return AppendWALBatch(path, []string{sql})
}

// AppendWALBatch appends multiple records and performs one durable sync.
func AppendWALBatch(path string, sqls []string) error {
	writer, err := OpenWALWriter(path)
	if err != nil {
		return err
	}
	defer writer.Close()
	return writer.Append(sqls)
}

// walEncodePool recycles WAL encode buffers. Buffers up to 4 MiB are
// retained; larger batch payloads are left to the GC so a single huge
// request cannot pin an oversized backing array in the pool.
var walEncodePool = sync.Pool{New: func() any { return make([]byte, 0, 64<<10) }}

// WALWriter keeps a WAL file open and serializes durable batch appends.
type WALWriter struct {
	mu   sync.Mutex
	file *os.File
}

func OpenWALWriter(path string) (*WALWriter, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, err
	}
	return &WALWriter{file: file}, nil
}

func (w *WALWriter) Append(sqls []string) error {
	if len(sqls) == 0 {
		return nil
	}
	total := 0
	for _, sql := range sqls {
		if len(sql) > maxPayloadSize {
			return errors.New("payload exceeds SUPERDB size limit")
		}
		total += headerSize + len(sql)
	}
	encoded := walEncodePool.Get().([]byte)[:0]
	if cap(encoded) < total {
		encoded = make([]byte, 0, total)
	}
	defer func() {
		if cap(encoded) <= 4<<20 {
			walEncodePool.Put(encoded[:0])
		}
	}()
	for _, sql := range sqls {
		encoded = append(encoded, magic...)
		encoded = append(encoded, walKind, walVersion, 0, 0)
		var lengths [8]byte
		binary.BigEndian.PutUint32(lengths[0:4], uint32(len(sql)))
		binary.BigEndian.PutUint32(lengths[4:8], uint32(len(sql)))
		encoded = append(encoded, lengths[:]...)
		encoded = append(encoded, sql...)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, err := w.file.Write(encoded); err != nil {
		return err
	}
	return w.file.Sync()
}

func (w *WALWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

// ReplayWAL replays SUPERDB framed records and falls back to legacy line WAL.
func ReplayWAL(path string, apply func(string) error) error {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(b) >= len(magic) && string(b[:len(magic)]) == magic {
		for offset, record := 0, 1; offset < len(b); record++ {
			payload, next, err := decodeFrame(b, offset, walKind)
			if err != nil {
				return fmt.Errorf("WAL record %d: %w", record, err)
			}
			if err := apply(string(payload)); err != nil {
				return fmt.Errorf("replay WAL record %d: %w", record, err)
			}
			offset = next
		}
		return nil
	}
	for lineNo, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if err := apply(line); err != nil {
			return fmt.Errorf("replay legacy WAL line %d: %w", lineNo+1, err)
		}
	}
	return nil
}
