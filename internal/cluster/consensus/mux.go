package consensus

import (
	"bufio"
	"net"
	"sync"
	"time"

	"github.com/hashicorp/raft"
)

// MagicRiver byte prefixes every SuperDB internal-RPC frame. HashiCorp Raft
// connections start with an RPC type byte < 16, so one peeked byte cleanly
// separates cluster traffic from Raft traffic on a single shared port.
const MagicRiver = byte('S')

// prefixedConn replays one already-read byte to the consumer.
type prefixedConn struct {
	net.Conn
	first byte
	once  sync.Mutex
	done  bool
}

func (c *prefixedConn) Read(b []byte) (int, error) {
	c.once.Lock()
	defer c.once.Unlock()
	if !c.done {
		c.done = true
		if len(b) == 0 {
			return 0, nil
		}
		b[0] = c.first
		return 1, nil
	}
	return c.Conn.Read(b)
}

// Mux splits one TCP listener into SuperDB RPC connections and Raft
// connections by peeking the first byte.
type Mux struct {
	ln     net.Listener
	river  chan net.Conn
	raftCh chan net.Conn
	closed chan struct{}
	once   sync.Once
}

// NewMux wraps ln and starts classifying connections.
func NewMux(ln net.Listener) *Mux {
	m := &Mux{ln: ln, river: make(chan net.Conn, 64), raftCh: make(chan net.Conn, 64), closed: make(chan struct{})}
	go m.loop()
	return m
}

func (m *Mux) loop() {
	for {
		c, err := m.ln.Accept()
		if err != nil {
			select {
			case <-m.closed:
				return
			default:
			}
			continue
		}
		go m.classify(c)
	}
}

func (m *Mux) classify(c net.Conn) {
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	var first [1]byte
	n, err := c.Read(first[:])
	_ = c.SetReadDeadline(time.Time{})
	if err != nil || n == 0 {
		_ = c.Close()
		return
	}
	wrapped := &prefixedConn{Conn: c, first: first[0]}
	if first[0] == MagicRiver {
		select {
		case m.river <- wrapped:
		case <-m.closed:
			_ = c.Close()
		}
		return
	}
	select {
	case m.raftCh <- wrapped:
	case <-m.closed:
		_ = c.Close()
	}
}

// River returns the SuperDB-RPC side as a net.Listener.
func (m *Mux) River() net.Listener { return &chanListener{ch: m.river, closed: m.closed} }

// RaftCh delivers Raft-classified connections to the Raft stream layer.
func (m *Mux) RaftCh() <-chan net.Conn { return m.raftCh }

// Close stops classification and closes the underlying listener.
func (m *Mux) Close() error {
	var err error
	m.once.Do(func() {
		close(m.closed)
		err = m.ln.Close()
	})
	return err
}

type chanListener struct {
	ch     chan net.Conn
	closed chan struct{}
}

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *chanListener) Close() error   { return nil }
func (l *chanListener) Addr() net.Addr { return &net.TCPAddr{} }

// RaftStreamLayer adapts classified Raft connections to hashicorp/raft.
type RaftStreamLayer struct {
	ch   <-chan net.Conn
	addr net.Addr
}

// NewRaftStreamLayer builds a StreamLayer over the mux Raft channel.
func NewRaftStreamLayer(ch <-chan net.Conn, addr net.Addr) *RaftStreamLayer {
	return &RaftStreamLayer{ch: ch, addr: addr}
}

// Dial opens a direct TCP connection to a Raft peer.
func (s *RaftStreamLayer) Dial(address raft.ServerAddress, timeout time.Duration) (net.Conn, error) {
	return net.DialTimeout("tcp", string(address), timeout)
}

// Accept returns the next Raft-classified inbound connection.
func (s *RaftStreamLayer) Accept() (net.Conn, error) {
	c, ok := <-s.ch
	if !ok {
		return nil, net.ErrClosed
	}
	return c, nil
}

// Close is a no-op: the mux owns the socket lifecycle.
func (s *RaftStreamLayer) Close() error { return nil }

// Addr returns the advertised address.
func (s *RaftStreamLayer) Addr() net.Addr { return s.addr }

// PeekReader helps tests classify frames.
func PeekReader(c net.Conn) *bufio.Reader { return bufio.NewReader(c) }
