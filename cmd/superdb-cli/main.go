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
	authKey := flag.String("auth-key", "", "access key required by the server (or SUPERDB_AUTH_KEY)")
	flag.Parse()
	if *authKey == "" {
		*authKey = os.Getenv("SUPERDB_AUTH_KEY")
	}
	queries, err := collectQueries(flag.Args())
	if err != nil {
		panic(err)
	}
	if err := runClient(*addr, *authKey, queries); err != nil {
		panic(err)
	}
}
