package remote

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"superdb/internal/engine"
	"superdb/internal/server"
	"superdb/internal/storage"
	"superdb/internal/wire"
)

const maxAuthFailures = 5

// Server is the hosted SuperDB TCP server speaking the SDB1 wire protocol.
// It calls the existing engine (and optionally Raft via ClusterExec) — it
// never duplicates storage or query logic.
type Server struct {
	cfg     Config
	db      *engine.Database
	auth    Authenticator
	metrics *Metrics

	cluster  ClusterExec
	shutdown func()

	mu       sync.Mutex
	listener net.Listener
	conns    map[net.Conn]struct{}
	wg       sync.WaitGroup
	closed   chan struct{}
	ready    atomicBool

	walWriter *storage.WALWriter
	health    *healthServer
}

type atomicBool struct {
	v  int32
	mu sync.RWMutex
	b  bool
}

func (a *atomicBool) Store(v bool) { a.mu.Lock(); a.b = v; a.mu.Unlock() }
func (a *atomicBool) Load() bool   { a.mu.RLock(); defer a.mu.RUnlock(); return a.b }

// New creates a server around an existing database handle.
func New(db *engine.Database, cfg Config, cluster ClusterExec) *Server {
	if cfg.MaxConnections <= 0 {
		cfg.MaxConnections = 128
	}
	m := &Metrics{startedAt: time.Now()}
	return &Server{
		cfg:     cfg,
		db:      db,
		auth:    NewAuthenticator(cfg.Username, cfg.Password),
		metrics: m,
		cluster: cluster,
		conns:   make(map[net.Conn]struct{}),
		closed:  make(chan struct{}),
	}
}

// Metrics returns a snapshot of server counters.
func (s *Server) Metrics() Snapshot { return s.metrics.Snapshot() }

// Addr returns the bound address.
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}

// Serve starts listening (plain TCP or TLS when cert/key are set) and
// blocks until ctx is cancelled or Shutdown is called.
func (s *Server) Serve(ctx context.Context) error {
	if s.cfg.Mode != "memory" && s.cfg.Mode != "wal" && s.cfg.Mode != "snapshot" {
		return fmt.Errorf("invalid mode %q: want memory|wal|snapshot", s.cfg.Mode)
	}
	if (s.cfg.TLSCertFile != "") != (s.cfg.TLSKeyFile != "") {
		return errors.New("tls-cert and tls-key must be set together")
	}
	// The server owns durability wiring: load snapshot + replay WAL into
	// the live engine before accepting traffic, mirroring the local
	// server entrypoint so restarts recover the same state.
	if s.cfg.Mode == "wal" || s.cfg.Mode == "snapshot" {
		if err := server.LoadSnapshot(s.cfg.DataDir, s.db); err != nil {
			return fmt.Errorf("load snapshot: %w", err)
		}
	}
	if s.cfg.Mode == "wal" {
		if err := server.ReplayWAL(s.cfg.DataDir, s.db); err != nil {
			return fmt.Errorf("replay wal: %w", err)
		}
		w, err := storage.OpenWALWriter(storage.WALPath(s.cfg.DataDir))
		if err != nil {
			return fmt.Errorf("open wal: %w", err)
		}
		s.walWriter = w
		defer w.Close()
	}
	addr := fmt.Sprintf("%s:%d", s.cfg.Host, s.cfg.Port)
	var ln net.Listener
	var err error
	if s.cfg.TLSCertFile != "" {
		cert, err := tls.LoadX509KeyPair(s.cfg.TLSCertFile, s.cfg.TLSKeyFile)
		if err != nil {
			return fmt.Errorf("load tls: %w", err)
		}
		ln, err = tls.Listen("tcp", addr, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	} else {
		var lc net.ListenConfig
		ln, err = lc.Listen(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	s.mu.Lock()
	s.listener = ln
	s.mu.Unlock()

	if s.cfg.HealthAddr != "" {
		s.health = newHealthServer(s.cfg.HealthAddr, s)
		go s.health.serve(ctx)
	}

	tlsNote := "plaintext"
	if s.cfg.TLSCertFile != "" {
		tlsNote = "tls"
	}
	log.Printf("SuperDB Server\nListening: %s\nProtocol: superdb (SDB1)\nDatabase: %s\nTLS: %s\nReady for connections",
		ln.Addr().String(), s.cfg.DataDir, tlsNote)
	s.ready.Store(true)

	sem := make(chan struct{}, s.cfg.MaxConnections)
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		c, err := ln.Accept()
		if err != nil {
			select {
			case <-s.closed:
				return nil
			default:
			}
			if ctx.Err() != nil {
				return nil
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			// Closed listener during shutdown.
			if strings.Contains(err.Error(), "closed") {
				return nil
			}
			continue
		}
		select {
		case sem <- struct{}{}:
		default:
			s.metrics.rejectedConns.Add(1)
			_ = c.Close()
			continue
		}
		s.metrics.totalConns.Add(1)
		s.metrics.activeConns.Add(1)
		s.wg.Add(1)
		go func(conn net.Conn) {
			defer func() {
				s.metrics.activeConns.Add(-1)
				s.wg.Done()
				<-sem
			}()
			s.trackConn(conn)
			s.handleConn(conn)
			s.untrackConn(conn)
		}(c)
	}
}

// Shutdown stops accepting new connections and waits for active ones.
func (s *Server) Shutdown() {
	select {
	case <-s.closed:
		return
	default:
		close(s.closed)
	}
	s.ready.Store(false)
	s.mu.Lock()
	if s.listener != nil {
		_ = s.listener.Close()
	}
	// Snapshot conns so we can close idle ones without holding the lock.
	conns := make([]net.Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	// Wake idle readers promptly so they drain without waiting out the
	// full idle deadline; connections mid-request are unaffected because
	// only the read deadline is shortened.
	for _, c := range conns {
		_ = c.SetReadDeadline(time.Now().Add(time.Second))
	}
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	timeout := s.cfg.ShutdownTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	select {
	case <-done:
	case <-time.After(timeout):
		for _, c := range conns {
			_ = c.Close()
		}
		<-done
	}
}

func (s *Server) trackConn(c net.Conn) {
	s.mu.Lock()
	s.conns[c] = struct{}{}
	s.mu.Unlock()
}

func (s *Server) untrackConn(c net.Conn) {
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
}

// Ready reports whether the server accepts database traffic.
func (s *Server) Ready() bool { return s.ready.Load() }

// handleConn runs the per-connection frame loop. One goroutine owns the
// connection: reads are sequential, writes are therefore serialized, and
// a malformed frame from this client can never affect another connection.
func (s *Server) handleConn(c net.Conn) {
	defer c.Close()
	if tcp, ok := c.(*net.TCPConn); ok {
		_ = tcp.SetNoDelay(true)
	}
	r := bufio.NewReaderSize(c, 64<<10)
	w := bufio.NewWriterSize(c, 64<<10)
	sess := &session{db: s.db, walWriter: s.walWriter, cluster: s.cluster, mode: s.cfg.Mode, dataDir: s.cfg.DataDir}
	if !s.auth.Required() {
		sess.authed = true
	}
	for {
		if s.cfg.IdleTimeout > 0 {
			_ = c.SetReadDeadline(time.Now().Add(s.cfg.IdleTimeout))
		} else if s.cfg.ReadTimeout > 0 {
			_ = c.SetReadDeadline(time.Now().Add(s.cfg.ReadTimeout))
		}
		hdr, payload, err := wire.ReadFrame(r)
		if err != nil {
			return // disconnect, timeout, or malformed header: drop only this conn
		}
		s.metrics.bytesRx.Add(uint64(wire.HeaderLen) + uint64(len(payload)))
		start := time.Now()
		respType, respID, respPayload := s.dispatch(sess, hdr, payload, c)
		s.metrics.queries.Add(1)
		s.metrics.latencyNanos.Add(uint64(time.Since(start).Nanoseconds()))
		if s.cfg.WriteTimeout > 0 {
			_ = c.SetWriteDeadline(time.Now().Add(s.cfg.WriteTimeout))
		}
		if len(respPayload) > wire.MaxFrameSize {
			respPayload = wire.ErrorResponse(respID, wire.ErrInternal, "response too large")
		}
		frame := wire.EncodeFrame(respType, respID, respPayload)
		if _, err := w.Write(frame); err != nil {
			return
		}
		if err := w.Flush(); err != nil {
			return
		}
		s.metrics.bytesTx.Add(uint64(len(frame)))
		if hdr.Type == wire.TypeClose {
			return
		}
		if sess.authFails >= maxAuthFailures {
			return
		}
	}
}

// dispatch routes one frame to its handler with panic isolation.
func (s *Server) dispatch(sess *session, hdr wire.Header, payload []byte, c net.Conn) (uint8, uint64, []byte) {
	var out []byte
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				s.metrics.failedQueries.Add(1)
				out = wire.ErrorResponse(hdr.RequestID, wire.ErrInternal, "internal error")
			}
		}()
		out = s.handle(sess, hdr, payload)
	}()
	return wire.TypeResponse, hdr.RequestID, out
}

func (s *Server) handle(sess *session, hdr wire.Header, payload []byte) []byte {
	switch hdr.Type {
	case wire.TypeHello:
		var p wire.HelloPayload
		_ = json.Unmarshal(payload, &p)
		return wire.MustJSON(wire.Response{RequestID: hdr.RequestID, Status: "ok", Message: "SDB1 v1"})
	case wire.TypeAuth:
		var p wire.AuthPayload
		if err := json.Unmarshal(payload, &p); err != nil {
			s.metrics.failedQueries.Add(1)
			return wire.ErrorResponse(hdr.RequestID, wire.ErrInvalidRequest, "invalid auth payload")
		}
		if p.Database != "" {
			sess.database = p.Database
		}
		secret := p.Password
		if p.Mechanism == "token" || (p.Mechanism == "" && p.Token != "" && p.Password == "") {
			// Token path is structurally supported for future API keys:
			// today a token authenticates as the configured password so
			// the mechanism can evolve without a protocol redesign.
			secret = p.Token
		}
		user := p.Username
		if user == "" {
			user = s.cfg.Username
			if user == "" {
				user = "admin"
			}
		}
		if s.auth.Verify(user, secret) {
			sess.authed = true
			return wire.MustJSON(wire.Response{RequestID: hdr.RequestID, Status: "ok", Message: "authenticated"})
		}
		sess.authFails++
		s.metrics.authFailures.Add(1)
		s.metrics.failedQueries.Add(1)
		return wire.ErrorResponse(hdr.RequestID, wire.ErrAuthFailed, "authentication failed")
	case wire.TypePing:
		return wire.MustJSON(wire.Response{RequestID: hdr.RequestID, Status: "ok", Message: "pong"})
	case wire.TypeQuery, wire.TypeExec:
		if s.auth.Required() && !sess.authed {
			s.metrics.failedQueries.Add(1)
			return wire.ErrorResponse(hdr.RequestID, wire.ErrAuthRequired, "authentication required")
		}
		var p wire.QueryPayload
		if err := json.Unmarshal(payload, &p); err != nil {
			s.metrics.failedQueries.Add(1)
			return wire.ErrorResponse(hdr.RequestID, wire.ErrInvalidRequest, "invalid request payload")
		}
		if len(p.SQL) == 0 {
			s.metrics.failedQueries.Add(1)
			return wire.ErrorResponse(hdr.RequestID, wire.ErrInvalidRequest, "sql required")
		}
		if len(p.SQL) > wire.MaxQuerySize {
			s.metrics.failedQueries.Add(1)
			return wire.ErrorResponse(hdr.RequestID, wire.ErrInvalidRequest, "query too large")
		}
		bound, err := bind(p.SQL, p.Params)
		if err != nil {
			s.metrics.failedQueries.Add(1)
			if ee, ok := err.(*execError); ok {
				return wire.ErrorResponse(hdr.RequestID, ee.Code, ee.Msg)
			}
			return wire.ErrorResponse(hdr.RequestID, wire.ErrInvalidRequest, err.Error())
		}
		res, _, execErr := sess.execOne(bound)
		if execErr != nil {
			s.metrics.failedQueries.Add(1)
			if ee, ok := execErr.(*execError); ok {
				r := wire.Response{RequestID: hdr.RequestID, Status: "error", ErrorCode: ee.Code, ErrorMessage: ee.Msg, Retryable: ee.Retryable, LeaderAddr: ee.LeaderAddr}
				return wire.MustJSON(r)
			}
			return wire.ErrorResponse(hdr.RequestID, wire.ErrQueryError, execErr.Error())
		}
		return wire.MustJSON(wire.Response{
			RequestID: hdr.RequestID, Status: "ok",
			Columns: res.Columns, Rows: res.Rows,
			AffectedRows: res.Affected, Message: res.Message,
		})
	case wire.TypeBegin:
		if s.auth.Required() && !sess.authed {
			return wire.ErrorResponse(hdr.RequestID, wire.ErrAuthRequired, "authentication required")
		}
		res, _, err := sess.execOne("BEGIN")
		if err != nil {
			s.metrics.failedQueries.Add(1)
			if ee, ok := err.(*execError); ok {
				return wire.ErrorResponse(hdr.RequestID, ee.Code, ee.Msg)
			}
			return wire.ErrorResponse(hdr.RequestID, wire.ErrQueryError, err.Error())
		}
		return wire.MustJSON(wire.Response{RequestID: hdr.RequestID, Status: "ok", Message: res.Message})
	case wire.TypeCommit:
		if s.auth.Required() && !sess.authed {
			return wire.ErrorResponse(hdr.RequestID, wire.ErrAuthRequired, "authentication required")
		}
		res, _, err := sess.execOne("COMMIT")
		if err != nil {
			s.metrics.failedQueries.Add(1)
			if ee, ok := err.(*execError); ok {
				return wire.ErrorResponse(hdr.RequestID, ee.Code, ee.Msg)
			}
			return wire.ErrorResponse(hdr.RequestID, wire.ErrQueryError, err.Error())
		}
		return wire.MustJSON(wire.Response{RequestID: hdr.RequestID, Status: "ok", Message: res.Message})
	case wire.TypeRollback:
		if s.auth.Required() && !sess.authed {
			return wire.ErrorResponse(hdr.RequestID, wire.ErrAuthRequired, "authentication required")
		}
		res, _, err := sess.execOne("ROLLBACK")
		if err != nil {
			s.metrics.failedQueries.Add(1)
			if ee, ok := err.(*execError); ok {
				return wire.ErrorResponse(hdr.RequestID, ee.Code, ee.Msg)
			}
			return wire.ErrorResponse(hdr.RequestID, wire.ErrQueryError, err.Error())
		}
		return wire.MustJSON(wire.Response{RequestID: hdr.RequestID, Status: "ok", Message: res.Message})
	case wire.TypeClose:
		return wire.MustJSON(wire.Response{RequestID: hdr.RequestID, Status: "ok", Message: "bye"})
	default:
		s.metrics.failedQueries.Add(1)
		return wire.ErrorResponse(hdr.RequestID, wire.ErrUnsupported, "unsupported operation")
	}
}
