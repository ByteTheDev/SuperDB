package server

import (
	"reflect"
	"testing"
)

func TestWriteStatementsDoesNotMutateInput(t *testing.T) {
	in := []string{
		"SELECT * FROM t",
		"INSERT INTO t VALUES (1)",
		"SELECT 1",
	}
	orig := append([]string(nil), in...)
	out := writeStatements(in)
	if !reflect.DeepEqual(in, orig) {
		t.Fatalf("input mutated: got %q want %q", in, orig)
	}
	if len(out) != 1 || out[0] != "INSERT INTO t VALUES (1)" {
		t.Fatalf("unexpected filter result: %q", out)
	}
}
