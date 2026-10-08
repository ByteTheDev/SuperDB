package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// stdout/stderr are variables so tests can capture output.
var (
	stdout io.Writer = os.Stdout
	stderr io.Writer = os.Stderr
)

const shellHelp = `Commands:
  .help              show this help
  .exit, .quit       leave the shell (exit and quit also work)
  .format table|json change output format
  .timing            toggle per-statement timing
Statements run when a line ends with ';' — multi-line input continues
with a '...>' prompt until the terminator arrives.
`

// runShell is the interactive SQL REPL: one persistent connection, one
// statement per submission, results rendered in the chosen format.
func runShell(c *conn, addr, authKey, format string, timing bool) error {
	fmt.Fprintf(stdout, "superdb-cli %s connected to %s\n", version, addr)
	fmt.Fprintln(stdout, "Type .help for help, .exit to quit.")
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	var pending strings.Builder
	for {
		if pending.Len() == 0 {
			fmt.Fprint(stdout, "superdb> ")
		} else {
			fmt.Fprint(stdout, "   ...> ")
		}
		if !scanner.Scan() {
			fmt.Fprintln(stdout)
			return scanner.Err()
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" && pending.Len() == 0 {
			continue
		}
		if pending.Len() == 0 {
			lower := strings.ToLower(strings.TrimSuffix(line, ";"))
			if strings.HasPrefix(lower, ".") {
				if runShellCommand(lower, &format, &timing) {
					return nil
				}
				continue
			}
			if lower == "exit" || lower == "quit" {
				return nil
			}
		}
		pending.WriteString(line)
		pending.WriteByte(' ')
		stmts, rest := splitStatements(pending.String(), false)
		if len(stmts) == 0 {
			continue
		}
		pending.Reset()
		pending.WriteString(rest)
		for _, sql := range stmts {
			started := time.Now()
			resp, err := c.query(authKey, sql, nil)
			if err != nil {
				fmt.Fprintf(stderr, "connection lost: %v\n", err)
				return err
			}
			printResult(resp, format, elapsed(timing, started))
		}
	}
}

// runShellCommand handles a dot-command; it reports true when the shell
// should exit.
func runShellCommand(cmd string, format *string, timing *bool) bool {
	fields := strings.Fields(cmd)
	switch fields[0] {
	case ".exit", ".quit":
		return true
	case ".help":
		fmt.Fprint(stdout, shellHelp)
	case ".format":
		if len(fields) == 2 && (fields[1] == "table" || fields[1] == "json") {
			*format = fields[1]
			fmt.Fprintf(stdout, "format: %s\n", *format)
		} else {
			fmt.Fprintf(stdout, "format is %s (usage: .format table|json)\n", *format)
		}
	case ".timing":
		*timing = !*timing
		fmt.Fprintf(stdout, "timing: %v\n", *timing)
	default:
		fmt.Fprintf(stderr, "unknown command %q — try .help\n", fields[0])
	}
	return false
}
