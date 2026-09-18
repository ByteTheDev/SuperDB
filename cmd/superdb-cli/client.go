package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
)

func collectQueries(args []string) ([]string, error) {
	if len(args) > 0 {
		return []string{strings.Join(args, " ")}, nil
	}
	queries := []string{}
	in := bufio.NewScanner(os.Stdin)
	for in.Scan() {
		if sql := strings.TrimSpace(in.Text()); sql != "" {
			queries = append(queries, sql)
		}
	}
	if err := in.Err(); err != nil {
		return nil, err
	}
	return queries, nil
}

func runClient(addr string, queries []string) error {
	if len(queries) == 0 {
		return nil
	}
	c, err := net.Dial("tcp", addr)
	if err != nil {
		return err
	}
	defer c.Close()
	if tcp, ok := c.(*net.TCPConn); ok {
		_ = tcp.SetNoDelay(true)
		_ = tcp.SetReadBuffer(256 << 10)
		_ = tcp.SetWriteBuffer(256 << 10)
	}
	// Fast path: one query keeps the single-statement frame so responses
	// stay byte-for-byte compatible. Multiple queries go out as one
	// {"sqls": [...]} batch: 1 RTT instead of N, single server persist.
	if len(queries) == 1 {
		return roundTripSingle(c, queries[0])
	}
	return roundTripBatch(c, queries)
}

func roundTripSingle(c net.Conn, sql string) error {
	payload, err := json.Marshal(map[string]string{"sql": sql})
	if err != nil {
		return err
	}
	w := bufio.NewWriterSize(c, 64<<10)
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(payload)))
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	if _, err := w.Write(payload); err != nil {
		return err
	}
	if err := w.Flush(); err != nil {
		return err
	}
	r := bufio.NewReaderSize(c, 64<<10)
	if _, err := readFull(r, header[:]); err != nil {
		return err
	}
	size := binary.BigEndian.Uint32(header[:])
	if size > 64<<20 {
		return fmt.Errorf("response too large: %d bytes", size)
	}
	resp := make([]byte, size)
	if _, err := readFull(r, resp); err != nil {
		return err
	}
	fmt.Println(string(resp))
	return nil
}

func roundTripBatch(c net.Conn, queries []string) error {
	payload, err := json.Marshal(map[string]any{"sqls": queries})
	if err != nil {
		return err
	}
	w := bufio.NewWriterSize(c, 64<<10)
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(payload)))
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	if _, err := w.Write(payload); err != nil {
		return err
	}
	if err := w.Flush(); err != nil {
		return err
	}
	r := bufio.NewReaderSize(c, 256<<10)
	if _, err := readFull(r, header[:]); err != nil {
		return err
	}
	size := binary.BigEndian.Uint32(header[:])
	if size > 64<<20 {
		return fmt.Errorf("response too large: %d bytes", size)
	}
	resp := make([]byte, size)
	if _, err := readFull(r, resp); err != nil {
		return err
	}
	// Server returns a JSON array with one element per query. Print one
	// JSON value per line to preserve the existing CLI output contract.
	var results []json.RawMessage
	if err := json.Unmarshal(resp, &results); err != nil {
		// Single error object (e.g. atomic failure): print as-is.
		fmt.Println(string(resp))
		return nil
	}
	for _, res := range results {
		fmt.Println(string(res))
	}
	return nil
}

func readFull(r *bufio.Reader, b []byte) (int, error) {
	n := 0
	for n < len(b) {
		count, err := r.Read(b[n:])
		n += count
		if err != nil {
			return n, err
		}
	}
	return n, nil
}
