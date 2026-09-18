package main

import (
	"flag"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:7654", "server address")
	flag.Parse()
	queries, err := collectQueries(flag.Args())
	if err != nil {
		panic(err)
	}
	if err := runClient(*addr, queries); err != nil {
		panic(err)
	}
}
