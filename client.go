package superdb

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"superdb/internal/wire"
)

// Rows is a query result.
type Rows struct {
	Columns []string
	Data    [][]any
}

// Result is an exec result.
type Result struct {
	AffectedRows int
	Message      string
}

// Client is a SuperDB hosted-protocol connection.
type Client struct {
	cfg    ConnConfig
	conn   net.Conn
	reader *bufio.Reader
	writer *bufio.Writer

	writeMu sync.Mutex
	nextID  atomic.Uint64

	mu        sync.Mutex
	pending   map[uint64]chan wire.Response
	closed    chan struct{}
	closeOnce sync.Once
	closeTx   sync.Once
}

// Connect dials a hosted SuperDB server, performs HELLO + AUTH, and
// returns a ready client.
//
//	out, err := superdb.Connect("superdb://user:pass@localhost:7432/main")
func Connect(rawurl string) (*Client, error) {
	cfg, err := ParseURL(rawurl)
	if err != nil {
		return nil, err
	}
	return Dial(cfg)
}

// Dial connects from an already-parsed config.
func Dial(cfg ConnConfig) (*Client, error) {
	timeout := cfg.ConnectTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	address := cfg.Address()
	var conn net.Conn
	var err error
	if cfg.TLS {
		tlsCfg := &tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12}
		if cfg.TLSInsecure {
			tlsCfg.InsecureSkipVerify = true //nolint:gosec // explicit opt-in via ?tls=insecure
		}
		d := &tls.Dialer{NetDialer: &net.Dialer{Timeout: timeout}, Config: tlsCfg}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		conn, err = d.DialContext(ctx, "tcp", address)
	} else {
		conn, err = net.DialTimeout("tcp", address, timeout)
	}
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", address, err)
	}
	c := &Client{
		cfg:     cfg,
		conn:    conn,
		reader:  bufio.NewReaderSize(conn, 64<<10),
		writer:  bufio.NewWriterSize(conn, 64<<10),
		pending: make(map[uint64]chan wire.Response),
		closed:  make(chan struct{}),
	}
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetNoDelay(true)
	}
	go c.readLoop()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := c.hello(ctx); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if cfg.Username != "" || cfg.Password != "" {
		if err := c.auth(ctx); err != nil {
			_ = conn.Close()
			return nil, err
		}
	}
	return c, nil
}

func (c *Client) hello(ctx context.Context) error {
	payload := wire.MustJSON(wire.HelloPayload{ClientName: "superdb-go", ClientVersion: "1.0"})
	resp, err := c.roundTrip(ctx, wire.TypeHello, payload)
	if err != nil {
		return err
	}
	if resp.Status != "ok" {
		return toServerError(resp)
	}
	return nil
}

func (c *Client) auth(ctx context.Context) error {
	payload := wire.MustJSON(wire.AuthPayload{Mechanism: "password", Username: c.cfg.Username, Password: c.cfg.Password, Database: c.cfg.Database})
	resp, err := c.roundTrip(ctx, wire.TypeAuth, payload)
	if err != nil {
		return err
	}
	if resp.Status != "ok" {
		return toServerError(resp)
	}
	return nil
}

// Ping checks the connection.
func (c *Client) Ping(ctx context.Context) error {
	resp, err := c.roundTrip(ctx, wire.TypePing, []byte(`{}`))
	if err != nil {
		return err
	}
	if resp.Status != "ok" {
		return toServerError(resp)
	}
	return nil
}

// Query runs a row-returning statement. Params are sent separately and
// bound server-side; values are never interpolated client-side.
func (c *Client) Query(ctx context.Context, sql string, args ...any) (*Rows, error) {
	payload, err := json.Marshal(wire.QueryPayload{SQL: sql, Params: args, Database: c.cfg.Database})
	if err != nil {
		return nil, err
	}
	resp, err := c.roundTrip(ctx, wire.TypeQuery, payload)
	if err != nil {
		return nil, err
	}
	if resp.Status != "ok" {
		return nil, toServerError(resp)
	}
	return &Rows{Columns: resp.Columns, Data: resp.Rows}, nil
}

// Exec runs a statement that returns affected-row metadata.
func (c *Client) Exec(ctx context.Context, sql string, args ...any) (Result, error) {
	payload, err := json.Marshal(wire.ExecPayload{SQL: sql, Params: args, Database: c.cfg.Database})
	if err != nil {
		return Result{}, err
	}
	resp, err := c.roundTrip(ctx, wire.TypeExec, payload)
	if err != nil {
		return Result{}, err
	}
	if resp.Status != "ok" {
		return Result{}, toServerError(resp)
	}
	return Result{AffectedRows: resp.AffectedRows, Message: resp.Message}, nil
}

// Begin starts a server-side session transaction.
func (c *Client) Begin(ctx context.Context) error {
	resp, err := c.roundTrip(ctx, wire.TypeBegin, []byte(`{}`))
	if err != nil {
		return err
	}
	if resp.Status != "ok" {
		return toServerError(resp)
	}
	return nil
}

// Commit commits the session transaction.
func (c *Client) Commit(ctx context.Context) error {
	resp, err := c.roundTrip(ctx, wire.TypeCommit, []byte(`{}`))
	if err != nil {
		return err
	}
	if resp.Status != "ok" {
		return toServerError(resp)
	}
	return nil
}

// Rollback aborts the session transaction.
func (c *Client) Rollback(ctx context.Context) error {
	resp, err := c.roundTrip(ctx, wire.TypeRollback, []byte(`{}`))
	if err != nil {
		return err
	}
	if resp.Status != "ok" {
		return toServerError(resp)
	}
	return nil
}

// Close gracefully closes the connection.
func (c *Client) Close() error {
	var err error
	c.closeTx.Do(func() {
		// Send the close frame before marking the client closed, otherwise
		// roundTrip rejects the request and the server never sees it.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, _ = c.roundTrip(ctx, wire.TypeClose, []byte(`{}`))
		cancel()
		c.markClosed()
		if cerr := c.conn.Close(); cerr != nil && !errors.Is(cerr, net.ErrClosed) {
			err = cerr
		}
	})
	return err
}

// markClosed flips the one-way closed latch exactly once. readLoop failures
// and Close both funnel through it so later calls fail fast.
func (c *Client) markClosed() {
	c.closeOnce.Do(func() { close(c.closed) })
}

func (c *Client) roundTrip(ctx context.Context, msgType uint8, payload []byte) (wire.Response, error) {
	if len(payload) > wire.MaxFrameSize {
		return wire.Response{}, &ServerError{Code: ErrInvalidRequest, Message: "request too large"}
	}
	id := c.nextID.Add(1)
	ch := make(chan wire.Response, 1)
	c.mu.Lock()
	select {
	case <-c.closed:
		c.mu.Unlock()
		return wire.Response{}, errors.New("client closed")
	default:
	}
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()
	frame := wire.EncodeFrame(msgType, id, payload)
	c.writeMu.Lock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	_, werr := c.writer.Write(frame)
	if werr == nil {
		werr = c.writer.Flush()
	}
	c.writeMu.Unlock()
	if werr != nil {
		return wire.Response{}, fmt.Errorf("write: %w", werr)
	}
	select {
	case <-ctx.Done():
		return wire.Response{}, ctx.Err()
	case resp := <-ch:
		// There is no closed case here on purpose: any close path ends in
		// conn.Close, which makes readLoop fail and failAll deliver a real
		// error on this channel — better fidelity than "client closed".
		if resp.RequestID != 0 && resp.RequestID != id {
			return wire.Response{}, &ServerError{Code: ErrProtocol, Message: "request id mismatch"}
		}
		resp.RequestID = id
		return resp, nil
	}
}

// readLoop demultiplexes responses by request ID. Writes are serialized
// by writeMu; reads are owned solely by this goroutine.
func (c *Client) readLoop() {
	for {
		hdr, payload, err := wire.ReadFrame(c.reader)
		if err != nil {
			c.failAll(err)
			return
		}
		if hdr.Type != wire.TypeResponse {
			continue
		}
		var resp wire.Response
		if err := json.Unmarshal(payload, &resp); err != nil {
			continue
		}
		resp.RequestID = hdr.RequestID
		c.mu.Lock()
		ch, ok := c.pending[hdr.RequestID]
		c.mu.Unlock()
		if ok {
			select {
			case ch <- resp:
			default:
			}
		}
	}
}

func (c *Client) failAll(err error) {
	c.mu.Lock()
	for id, ch := range c.pending {
		select {
		case ch <- wire.Response{RequestID: id, Status: "error", ErrorCode: ErrProtocol, ErrorMessage: err.Error()}:
		default:
		}
	}
	c.mu.Unlock()
	// Poison the connection so new calls fail fast instead of writing to a
	// dead socket and hanging until their deadline.
	c.markClosed()
	_ = c.conn.Close()
}

func toServerError(r wire.Response) error {
	return &ServerError{Code: r.ErrorCode, Message: r.ErrorMessage, Retryable: r.Retryable, LeaderAddr: r.LeaderAddr}
}
