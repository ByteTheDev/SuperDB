package server

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"sync"

	"superdb/internal/engine"
)

const maxRequestSize = 16 << 20

// payloadPool recycles request/response buffers up to 64 KiB, the common
// case for OLTP statements. Larger payloads bypass the pool to avoid
// holding onto oversized backing arrays.
var payloadPool = sync.Pool{New: func() any { return make([]byte, 0, 64<<10) }}

// bufferPool recycles JSON encode buffers for responses.
var bufferPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

type request struct {
	SQL  string   `json:"sql,omitempty"`
	SQLs []string `json:"sqls,omitempty"`
	// Atomic commits SQLs as one all-or-nothing unit. Honored in cluster
	// mode (single Raft entry); in local mode the batch still stops at the
	// first error but earlier statements are NOT rolled back.
	Atomic bool `json:"atomic,omitempty"`
}

func readRequest(r *bufio.Reader) (request, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return request{}, err
	}
	size := binary.BigEndian.Uint32(header[:])
	if size > maxRequestSize {
		return request{}, errors.New("request too large")
	}
	var payload []byte
	var pooled bool
	if size <= 64<<10 {
		if buf, ok := payloadPool.Get().([]byte); ok {
			if cap(buf) >= int(size) {
				payload = buf[:size]
				pooled = true
			}
		}
	}
	if payload == nil {
		payload = make([]byte, size)
	}
	if _, err := io.ReadFull(r, payload); err != nil {
		if pooled {
			payloadPool.Put(payload[:0])
		}
		return request{}, err
	}
	var q request
	if fast, ok := parseRequestFast(payload); ok {
		q = fast
	} else if err := json.Unmarshal(payload, &q); err != nil {
		if pooled {
			payloadPool.Put(payload[:0])
		}
		return request{}, err
	}
	if pooled {
		payloadPool.Put(payload[:0])
	}
	if q.SQL == "" && len(q.SQLs) == 0 {
		return request{}, errors.New("request must contain sql or sqls")
	}
	return q, nil
}

func writeResponse(w *bufio.Writer, value any) error {
	buf, ok := bufferPool.Get().(*bytes.Buffer)
	if !ok {
		buf = new(bytes.Buffer)
	}
	buf.Reset()
	payload, err := appendResponseValue(buf.Bytes()[:0], value)
	if err != nil {
		buf.Reset()
		bufferPool.Put(buf)
		return err
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(payload)))
	if _, err := w.Write(header[:]); err != nil {
		buf.Reset()
		bufferPool.Put(buf)
		return err
	}
	if _, err := w.Write(payload); err != nil {
		buf.Reset()
		bufferPool.Put(buf)
		return err
	}
	buf.Reset()
	bufferPool.Put(buf)
	return w.Flush()
}

// appendResponseValue encodes a response into buf without per-result
// allocations or reflection. engine.Result uses its custom marshaler
// (byte-identical to encoding/json); anything else falls back to the
// standard library so output and errors stay exact.
func appendResponseValue(buf []byte, value any) ([]byte, error) {
	switch v := value.(type) {
	case engine.Result:
		return engine.AppendResultJSON(buf, v)
	case []any:
		buf = append(buf, '[')
		for i, item := range v {
			if i > 0 {
				buf = append(buf, ',')
			}
			var err error
			buf, err = appendResponseValue(buf, item)
			if err != nil {
				return nil, err
			}
		}
		return append(buf, ']'), nil
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		return append(buf, b...), nil
	}
}
