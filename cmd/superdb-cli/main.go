package main

import (
	"flag"
	"fmt"
	"os"
)

var version = "dev"

func main() {
	if len(os.Args) == 2 && (os.Args[1] == "--version" || os.Args[1] == "-version") {
		fmt.Printf("superdb-cli %s\n", version)
		return
	}
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
