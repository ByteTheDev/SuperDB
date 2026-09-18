package cluster

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
	"time"
)

// ProtocolVersion is the internal cluster wire version.
const ProtocolVersion = 1

const (
	maxFrameSize   = 16 << 20
	defaultTimeout = 5 * time.Second
	maxPoolPerPeer = 8
)

// Message kinds on the internal transport. This framing is intentionally
// separate from the public client SQL protocol.
const (
	KindPing       = "ping"
	KindJoin       = "join"
	KindLeave      = "leave"
	KindMetadata   = "metadata"
	KindLookup     = "lookup"
	KindForward    = "forward"
	KindRangeOp    = "range_op"
	KindMoveLeader = "move_leader"
	KindRemove     = "remove"
)

type envelope struct {
	V       int             `json:"v"`
	Kind    string          `json:"kind"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

type response struct {
	V       int             `json:"v"`
	OK      bool            `json:"ok"`
	Payload json.RawMessage `json:"payload,omitempty"`
	Error   string          `json:"error,omitempty"`
	Code    Code            `json:"code,omitempty"`
}

// Handler serves one RPC kind.
type Handler func(ctx context.Context, payload json.RawMessage) (any, error)

// Transport is a length-prefixed JSON node-to-node transport with connection
// reuse. Metadata/health paths only; bulk data replication can adopt a binary
// encoding later without changing this boundary.
type Transport struct {
	mu       sync.Mutex
	handlers map[string]Handler
	pools    map[string]chan net.Conn
	closed   bool
}

// NewTransport creates a transport with no handlers.
func NewTransport() *Transport {
	return &Transport{handlers: make(map[string]Handler), pools: make(map[string]chan net.Conn)}
}

// Handle registers an RPC handler.
func (t *Transport) Handle(kind string, h Handler) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.handlers[kind] = h
}

func (t *Transport) handler(kind string) (Handler, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	h, ok := t.handlers[kind]
	return h, ok
}

// Serve accepts internal connections until ln closes or ctx ends.
func (t *Transport) Serve(ctx context.Context, ln net.Listener) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			continue
		}
		go t.serveConn(ctx, conn)
	}
}

func (t *Transport) serveConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		req, err := readEnvelope(r)
		if err != nil {
			return
		}
		h, ok := t.handler(req.Kind)
		var out response
		out.V = ProtocolVersion
		if req.V != ProtocolVersion {
			out.Error = "unsupported cluster protocol version"
			out.Code = CodeVersionMismatch
		} else if !ok {
			out.Error = "unknown rpc kind " + req.Kind
			out.Code = CodeNotFound
		} else {
			hctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			payload, herr := h(hctx, req.Payload)
			cancel()
			if herr != nil {
				out.Error = herr.Error()
				if ce, ok := AsError(herr); ok {
					out.Code = ce.Code
				} else {
					out.Code = CodeTransport
				}
			} else {
				raw, merr := json.Marshal(payload)
				if merr != nil {
					out.Error = merr.Error()
					out.Code = CodeTransport
				} else {
					out.OK = true
					out.Payload = raw
				}
			}
		}
		_ = conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
		if werr := writeFrame(w, out); werr != nil {
			return
		}
	}
}

// Call performs one RPC with connection reuse, timeout, and cancellation.
func (t *Transport) Call(ctx context.Context, addr, kind string, reqPayload, respPayload any) error {
	conn, err := t.acquire(ctx, addr)
	if err != nil {
		return err
	}
	// Do not return broken connections to the pool.
	failed := true
	defer func() { t.release(addr, conn, failed) }()

	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultTimeout)
		defer cancel()
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	var raw json.RawMessage
	if reqPayload != nil {
		b, err := json.Marshal(reqPayload)
		if err != nil {
			return NewError(CodeTransport, "encode request: "+err.Error())
		}
		raw = b
	}
	w := bufio.NewWriter(conn)
	if err := writeFrame(w, envelope{V: ProtocolVersion, Kind: kind, Payload: raw}); err != nil {
		return NewError(CodeTransport, "send: "+err.Error())
	}
	resp, err := readResponse(bufio.NewReader(conn))
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
			return NewError(CodeTimeout, "rpc "+kind+": "+err.Error())
		}
		return NewError(CodeTransport, "recv: "+err.Error())
	}
	if ctx.Err() != nil {
		return NewError(CodeTimeout, "rpc "+kind+" cancelled: "+ctx.Err().Error())
	}
	if resp.V != ProtocolVersion {
		return NewError(CodeVersionMismatch, "peer uses unsupported protocol version")
	}
	if !resp.OK {
		code := resp.Code
		if code == "" {
			code = CodeTransport
		}
		e := NewError(code, resp.Error)
		if code == CodeTimeout || code == CodeUnavailable {
			e.Retryable = true
		}
		return e
	}
	if respPayload != nil && len(resp.Payload) > 0 {
		if err := json.Unmarshal(resp.Payload, respPayload); err != nil {
			return NewError(CodeTransport, "decode response: "+err.Error())
		}
	}
	failed = false
	return nil
}

func (t *Transport) acquire(ctx context.Context, addr string) (net.Conn, error) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil, NewError(CodeShutdown, "transport closed")
	}
	pool, ok := t.pools[addr]
	if !ok {
		pool = make(chan net.Conn, maxPoolPerPeer)
		t.pools[addr] = pool
	}
	t.mu.Unlock()

	select {
	case c := <-pool:
		if isAlive(c) {
			return c, nil
		}
		_ = c.Close()
	default:
	}
	d := net.Dialer{Timeout: defaultTimeout, KeepAlive: 30 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, &Error{Code: CodeUnavailable, Message: "dial " + addr + ": " + err.Error(), Retryable: true}
	}
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetNoDelay(true)
	}
	return conn, nil
}

func (t *Transport) release(addr string, conn net.Conn, failed bool) {
	if failed {
		_ = conn.Close()
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		_ = conn.Close()
		return
	}
	pool, ok := t.pools[addr]
	if !ok {
		pool = make(chan net.Conn, maxPoolPerPeer)
		t.pools[addr] = pool
	}
	select {
	case pool <- conn:
	default:
		_ = conn.Close()
	}
}

// Close stops reuse and closes pooled connections.
func (t *Transport) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	for _, pool := range t.pools {
		close(pool)
		for c := range pool {
			_ = c.Close()
		}
	}
	t.pools = make(map[string]chan net.Conn)
}

func isAlive(c net.Conn) bool {
	_ = c.SetReadDeadline(time.Now())
	var one [1]byte
	_, err := c.Read(one[:])
	_ = c.SetReadDeadline(time.Now().Add(30 * time.Second))
	if err != nil && !isTimeout(err) {
		return false
	}
	return true
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func readEnvelope(r *bufio.Reader) (envelope, error) {
	magic, err := r.ReadByte()
	if err != nil {
		return envelope{}, err
	}
	if magic != riverMagic {
		return envelope{}, errors.New("not a cluster frame")
	}
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return envelope{}, err
	}
	size := binary.BigEndian.Uint32(header[:])
	if size == 0 || size > maxFrameSize {
		return envelope{}, errors.New("invalid cluster frame size")
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(r, payload); err != nil {
		return envelope{}, err
	}
	var env envelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return envelope{}, err
	}
	return env, nil
}

func readResponse(r *bufio.Reader) (response, error) {
	magic, err := r.ReadByte()
	if err != nil {
		return response{}, err
	}
	if magic != riverMagic {
		return response{}, errors.New("not a cluster frame")
	}
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return response{}, err
	}
	size := binary.BigEndian.Uint32(header[:])
	if size == 0 || size > maxFrameSize {
		return response{}, errors.New("invalid cluster frame size")
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(r, payload); err != nil {
		return response{}, err
	}
	var out response
	if err := json.Unmarshal(payload, &out); err != nil {
		return response{}, err
	}
	return out, nil
}

// riverMagic prefixes every internal frame so the port mux can separate
// cluster RPC from Raft traffic. Public client SQL framing is unchanged.
const riverMagic = byte('S')

func writeFrame(w *bufio.Writer, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err := w.WriteByte(riverMagic); err != nil {
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
