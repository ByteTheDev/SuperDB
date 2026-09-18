package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"superdb/internal/engine"
	"superdb/internal/server"
)

func main() {
	if len(os.Args) < 2 || os.Args[1] != "server" {
		fmt.Println("usage: superdb server [--addr host:port] [--mode memory|wal|snapshot] [--data dir]")
		return
	}
	fs := flag.NewFlagSet("server", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:7654", "listen address")
	mode := fs.String("mode", "memory", "durability mode")
	data := fs.String("data", "./SUPERDB", "data directory")
	fs.Parse(os.Args[2:])
	db := engine.New()
	if *mode == "wal" || *mode == "snapshot" {
		if e := server.LoadSnapshot(*data, db); e != nil {
			log.Fatal(e)
		}
	}
	if *mode == "wal" {
		if e := server.ReplayWAL(*data, db); e != nil {
			log.Fatal(e)
		}
	}
	log.Printf("SuperDB listening on %s mode=%s data=%s", *addr, *mode, *data)
	ln, e := net.Listen("tcp", *addr)
	if e != nil {
		log.Fatal(e)
	}
	defer ln.Close()
	for {
		c, e := ln.Accept()
		if e != nil {
			log.Print(e)
			continue
		}
		go server.Handle(c, db, *mode, *data)
	}
}
