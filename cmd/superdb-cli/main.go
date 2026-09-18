package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:7654", "server address")
	flag.Parse()
	sql := strings.Join(flag.Args(), " ")
	if sql == "" {
		in := bufio.NewScanner(os.Stdin)
		for in.Scan() {
			sql = in.Text()
			break
		}
	}
	c, e := net.Dial("tcp", *addr)
	if e != nil {
		panic(e)
	}
	defer c.Close()
	b, _ := json.Marshal(map[string]string{"sql": sql})
	var h [4]byte
	binary.BigEndian.PutUint32(h[:], uint32(len(b)))
	c.Write(h[:])
	c.Write(b)
	if _, e = readFull(c, h[:]); e != nil {
		panic(e)
	}
	p := make([]byte, binary.BigEndian.Uint32(h[:]))
	readFull(c, p)
	fmt.Println(string(p))
}
func readFull(c net.Conn, b []byte) (int, error) {
	n := 0
	for n < len(b) {
		x, e := c.Read(b[n:])
		n += x
		if e != nil {
			return n, e
		}
	}
	return n, nil
}
