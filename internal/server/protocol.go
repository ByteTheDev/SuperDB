package server

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
)

const maxRequestSize = 16 << 20

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
	payload := make([]byte, size)
	if _, err := io.ReadFull(r, payload); err != nil {
		return request{}, err
	}
	var q request
	if err := json.Unmarshal(payload, &q); err != nil {
		return request{}, err
	}
	if q.SQL == "" && len(q.SQLs) == 0 {
		return request{}, errors.New("request must contain sql or sqls")
	}
	return q, nil
}

func writeResponse(w *bufio.Writer, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(payload)))
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	if _, err := w.Write(payload); err != nil {
		return err
	}
	return w.Flush()
}
