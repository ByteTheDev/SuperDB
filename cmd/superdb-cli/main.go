package main

import (
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"superdb/internal/server"
)

var version = "dev"

func main() {
	if len(os.Args) == 2 && (os.Args[1] == "--version" || os.Args[1] == "-version") {
		fmt.Printf("superdb-cli %s\n", version)
		return
	}
	addr := flag.String("addr", "127.0.0.1:7654", "server address")
	authKey := flag.String("auth-key", "", "access key required by the server (or SUPERDB_AUTH_KEY)")
	format := flag.String("format", "auto", "output format: auto, json, or table")
	timing := flag.Bool("timing", false, "print per-query elapsed time")
	file := flag.String("file", "", "execute SQL statements from a file, then exit")
	flag.StringVar(file, "f", "", "alias for --file")
	paramsFlag := flag.String("params", "", "JSON array of values bound to ? placeholders (single query only)")
	tlsOn := flag.Bool("tls", false, "connect over TLS")
	tlsCA := flag.String("tls-ca", "", "PEM file of CA/certificates to trust for TLS (implies --tls)")
	tlsSkip := flag.Bool("tls-skip-verify", false, "skip TLS certificate verification, insecure (implies --tls)")
	ping := flag.Bool("ping", false, "probe the server for reachability, then exit")
	flag.Parse()
	var tlsCfg *tls.Config
	if *tlsOn || *tlsCA != "" || *tlsSkip {
		var err error
		tlsCfg, err = server.ClientTLSConfig(*tlsCA, *tlsSkip)
		if err != nil {
			fmt.Fprintf(stderr, "superdb-cli: %v\n", err)
			os.Exit(2)
		}
	}
	var params []any
	if *paramsFlag != "" {
		if err := json.Unmarshal([]byte(*paramsFlag), &params); err != nil {
			fmt.Fprintf(stderr, "superdb-cli: invalid --params %q: %v\n", *paramsFlag, err)
			os.Exit(2)
		}
	}
	if *authKey == "" {
		*authKey = os.Getenv("SUPERDB_AUTH_KEY")
	}
	if *format != "auto" && *format != "json" && *format != "table" {
		fmt.Fprintf(stderr, "superdb-cli: invalid --format %q (want auto, json, or table)\n", *format)
		os.Exit(2)
	}
	if *ping {
		if err := probeServer(*addr, *authKey, tlsCfg); err != nil {
			fmt.Fprintf(stderr, "superdb-cli: %s not reachable: %v\n", *addr, err)
			os.Exit(1)
		}
		fmt.Printf("%s: ok\n", *addr)
		return
	}
	// auto: table output in the interactive shell, JSON for scripts/pipes.
	outFormat := *format
	interactive := *file == "" && flag.NArg() == 0 && stdinIsTerminal()
	if interactive && outFormat == "auto" {
		outFormat = "table"
	} else if outFormat == "auto" {
		outFormat = "json"
	}
	if interactive {
		c, err := dial(*addr, tlsCfg)
		if err != nil {
			fmt.Fprintf(stderr, "superdb-cli: cannot connect to %s: %v\n", *addr, err)
			os.Exit(1)
		}
		defer c.close()
		if err := runShell(c, *addr, *authKey, outFormat, *timing); err != nil {
			os.Exit(1)
		}
		return
	}
	queries, err := collectQueries(flag.Args(), *file)
	if err != nil {
		fmt.Fprintf(stderr, "superdb-cli: %v\n", err)
		os.Exit(1)
	}
	if err := runBatch(*addr, *authKey, queries, params, outFormat, *timing, tlsCfg); err != nil {
		fmt.Fprintf(stderr, "superdb-cli: %v\n", err)
		os.Exit(1)
	}
}
