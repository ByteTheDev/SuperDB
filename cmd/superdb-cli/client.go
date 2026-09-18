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
	for _, sql := range queries {
		payload, err := json.Marshal(map[string]string{"sql": sql})
		if err != nil {
			return err
		}
		var header [4]byte
		binary.BigEndian.PutUint32(header[:], uint32(len(payload)))
		frame := make([]byte, len(header)+len(payload))
		copy(frame, header[:])
		copy(frame[len(header):], payload)
		if _, err := c.Write(frame); err != nil {
			return err
		}
	}
	for range queries {
		var header [4]byte
		if _, err := readFull(c, header[:]); err != nil {
			return err
		}
		payload := make([]byte, binary.BigEndian.Uint32(header[:]))
		if _, err := readFull(c, payload); err != nil {
			return err
		}
		fmt.Println(string(payload))
	}
	return nil
}

func readFull(c net.Conn, b []byte) (int, error) {
	n := 0
	for n < len(b) {
		count, err := c.Read(b[n:])
		n += count
		if err != nil {
			return n, err
		}
	}
	return n, nil
}
