package server

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// Ping checks whether a SuperDB TCP server is reachable and answering the
// length-prefixed JSON protocol. It sends {"ping":true} and expects a
// status-ok response; the auth key is optional because ping is answered
// before authentication. A non-SuperDB listener, a refused connection, or
// a timeout all return an error so callers can report "not reachable".
func Ping(addr, authKey string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 800 * time.Millisecond
	}
	c, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(timeout))
	payload, err := json.Marshal(request{Ping: true, Auth: authKey})
	if err != nil {
		return err
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(payload)))
	if _, err := c.Write(header[:]); err != nil {
		return err
	}
	if _, err := c.Write(payload); err != nil {
		return err
	}
	if _, err := io.ReadFull(c, header[:]); err != nil {
		return err
	}
	size := binary.BigEndian.Uint32(header[:])
	if size > 1<<20 {
		return fmt.Errorf("probe response too large: %d bytes", size)
	}
	resp := make([]byte, size)
	if _, err := io.ReadFull(c, resp); err != nil {
		return err
	}
	var decoded struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal(resp, &decoded); err != nil {
		return errors.New("not a SuperDB server (unrecognized response)")
	}
	if decoded.Error != "" {
		return errors.New(decoded.Error)
	}
	if decoded.Status != "ok" {
		return errors.New("unexpected probe response")
	}
	return nil
}
