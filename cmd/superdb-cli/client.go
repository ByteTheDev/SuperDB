package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

// conn is one TCP connection to a SuperDB server. A single connection is
// reused for every statement in batch, file, and interactive modes so
// sessions (BEGIN/COMMIT) persist across input lines.
type conn struct {
	c net.Conn
	r *bufio.Reader
	w *bufio.Writer
}

func dial(addr string) (*conn, error) {
	c, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return nil, err
	}
	if tcp, ok := c.(*net.TCPConn); ok {
		_ = tcp.SetNoDelay(true)
		_ = tcp.SetReadBuffer(256 << 10)
		_ = tcp.SetWriteBuffer(256 << 10)
	}
	return &conn{
		c: c,
		r: bufio.NewReaderSize(c, 256<<10),
		w: bufio.NewWriterSize(c, 64<<10),
	}, nil
}

func (c *conn) close() { _ = c.c.Close() }

func (c *conn) send(payload []byte) error {
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(payload)))
	if _, err := c.w.Write(header[:]); err != nil {
		return err
	}
	if _, err := c.w.Write(payload); err != nil {
		return err
	}
	return c.w.Flush()
}

// recv reads one framed response.
func (c *conn) recv() ([]byte, error) {
	var header [4]byte
	if _, err := readFull(c.r, header[:]); err != nil {
		return nil, err
	}
	size := binary.BigEndian.Uint32(header[:])
	if size > 64<<20 {
		return nil, fmt.Errorf("response too large: %d bytes", size)
	}
	resp := make([]byte, size)
	if _, err := readFull(c.r, resp); err != nil {
		return nil, err
	}
	return resp, nil
}

// query runs one statement as a {"sql": ...} request and returns the raw
// response payload.
func (c *conn) query(authKey, sql string) ([]byte, error) {
	msg := map[string]string{"sql": sql}
	if authKey != "" {
		msg["auth"] = authKey
	}
	payload, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	if err := c.send(payload); err != nil {
		return nil, err
	}
	return c.recv()
}

// batch sends multiple statements as one {"sqls": [...]} request: a single
// round trip and a single server-side persist for write workloads.
func (c *conn) batch(authKey string, sqls []string) ([][]byte, error) {
	msg := map[string]any{"sqls": sqls}
	if authKey != "" {
		msg["auth"] = authKey
	}
	payload, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	if err := c.send(payload); err != nil {
		return nil, err
	}
	resp, err := c.recv()
	if err != nil {
		return nil, err
	}
	var results []json.RawMessage
	if err := json.Unmarshal(resp, &results); err != nil {
		// Single error object (e.g. a request-level failure): surface it
		// as one result.
		return [][]byte{resp}, nil
	}
	out := make([][]byte, 0, len(results))
	for _, r := range results {
		out = append(out, []byte(r))
	}
	return out, nil
}

// collectQueries gathers statements from positional args, a --file, or
// piped stdin. Semicolon splitting is quote-aware, so multiple statements
// per line and multi-line files both work.
func collectQueries(args []string, file string) ([]string, error) {
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		stmts, _ := splitStatements(string(b), true)
		return stmts, nil
	}
	if len(args) > 0 {
		stmts, _ := splitStatements(strings.Join(args, " "), true)
		return stmts, nil
	}
	var queries []string
	in := bufio.NewScanner(os.Stdin)
	// The server accepts requests up to 16 MiB; allow single-line
	// statements up to that size instead of the 64 KiB scanner default.
	in.Buffer(make([]byte, 64<<10), 16<<20)
	for in.Scan() {
		stmts, _ := splitStatements(in.Text(), true)
		queries = append(queries, stmts...)
	}
	if err := in.Err(); err != nil {
		return nil, err
	}
	return queries, nil
}

// runBatch executes statements non-interactively over one connection and
// prints one response per statement.
func runBatch(addr, authKey string, queries []string, format string, timing bool) error {
	if len(queries) == 0 {
		return nil
	}
	c, err := dial(addr)
	if err != nil {
		return err
	}
	defer c.close()
	// Bound the whole operation so a hung server cannot hang the CLI.
	_ = c.c.SetDeadline(time.Now().Add(2 * time.Minute))
	if len(queries) == 1 {
		started := time.Now()
		resp, err := c.query(authKey, queries[0])
		if err != nil {
			return err
		}
		printResult(resp, format, elapsed(timing, started))
		return nil
	}
	started := time.Now()
	results, err := c.batch(authKey, queries)
	if err != nil {
		return err
	}
	per := ""
	if timing && len(results) > 0 {
		per = fmt.Sprintf("%s total", time.Since(started).Round(time.Microsecond))
	}
	for _, res := range results {
		printResult(res, format, per)
	}
	return nil
}

func elapsed(enabled bool, started time.Time) string {
	if !enabled {
		return ""
	}
	return time.Since(started).Round(time.Microsecond).String()
}

// stdinIsTerminal reports whether stdin is an interactive console. When it
// is (and no query/file was supplied) the client starts the SQL shell.
func stdinIsTerminal() bool {
	info, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// probeServer pings the server and returns an error describing failure.
func probeServer(addr, authKey string) error {
	c, err := dial(addr)
	if err != nil {
		return err
	}
	defer c.close()
	_ = c.c.SetDeadline(time.Now().Add(5 * time.Second))
	msg := map[string]any{"ping": true}
	if authKey != "" {
		msg["auth"] = authKey
	}
	payload, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	if err := c.send(payload); err != nil {
		return err
	}
	resp, err := c.recv()
	if err != nil {
		return err
	}
	var r cliResult
	if err := json.Unmarshal(resp, &r); err != nil {
		return errors.New("not a SuperDB server (unrecognized response)")
	}
	if r.Error != "" {
		return errors.New(r.Error)
	}
	if r.Status != "ok" {
		return errors.New("unexpected probe response")
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
