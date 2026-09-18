package engine

import (
	"encoding/json"
	"reflect"
	"testing"
)

// plainResult has Result's fields but no MarshalJSON, so encoding/json uses
// reflection: exactly the old encoding path. Any byte difference fails.
type plainResult Result

func TestResultMarshalParity(t *testing.T) {
	cases := []Result{
		{},
		{Message: "table created"},
		{Affected: 1},
		{Columns: []string{"id", "name"}, Rows: [][]any{{int64(1), "Ada"}}},
		{Columns: []string{"count(*)"}, Rows: [][]any{{2}}},
		{Message: "ok", Affected: 3, Columns: []string{"a"}, Rows: [][]any{{nil}}},
		{Columns: []string{"t"}, Rows: [][]any{{"<a href=\"x\">&'\"\n\r\t\x01\x7f"}}},
		{Columns: []string{"u"}, Rows: [][]any{{"héllo 世界 🎉"}}},
		{Columns: []string{"f"}, Rows: [][]any{{10.5}, {1e21}, {1e-7}, {0.0}, {-0.0}}},
		{Columns: []string{"b"}, Rows: [][]any{{true}, {false}}},
		{Columns: []string{"m"}, Rows: [][]any{{int64(-42), int(7), "x", true, nil, 3.25}}},
		{Columns: []string{"bad"}, Rows: [][]any{{"ok\xffbad", "\xc0\xaf", "\xed\xa0\x80"}}},
		{Columns: []string{}, Rows: [][]any{}},
	}
	for i, r := range cases {
		got, gotErr := json.Marshal(r)
		want, wantErr := json.Marshal(plainResult(r))
		if (gotErr != nil) != (wantErr != nil) {
			t.Fatalf("case %d: error mismatch got=%v want=%v", i, gotErr, wantErr)
		}
		if gotErr == nil && string(got) != string(want) {
			t.Fatalf("case %d: byte mismatch\ngot:  %s\nwant: %s", i, got, want)
		}
	}
}

func TestResultMarshalRoundTrip(t *testing.T) {
	d := New()
	mustExec(t, d, "CREATE TABLE users (id INT PRIMARY KEY, name TEXT, score FLOAT, active BOOL)")
	mustExec(t, d, "INSERT INTO users VALUES (1, 'Ada <&>', 10.5, true), (2, 'Gr\"ace', 9.25, false)")
	for _, q := range []string{
		"SELECT * FROM users",
		"SELECT COUNT(*) FROM users WHERE active = true",
		"SELECT name FROM users WHERE id = 2",
	} {
		r, err := d.Exec(q)
		if err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		var back Result
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(back.Columns, r.Columns) || !reflect.DeepEqual(back.Rows, normalizeRows(r.Rows)) {
			t.Fatalf("round trip mismatch for %q: %+v vs %+v", q, back, r)
		}
	}
}

// JSON numbers decode as float64; normalize int64/int rows for comparison.
func normalizeRows(rows [][]any) [][]any {
	out := make([][]any, len(rows))
	for i, row := range rows {
		out[i] = make([]any, len(row))
		for j, v := range row {
			switch n := v.(type) {
			case int64:
				out[i][j] = float64(n)
			case int:
				out[i][j] = float64(n)
			default:
				out[i][j] = v
			}
		}
	}
	return out
}

func mustExec(t *testing.T, d *Database, sql string) {
	t.Helper()
	if _, err := d.Exec(sql); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func TestLazyOrderDeleteStaysCorrect(t *testing.T) {
	d := New()
	mustExec(t, d, "CREATE TABLE t (id INT PRIMARY KEY, v TEXT)")
	for i := 0; i < 600; i++ {
		mustExec(t, d, "INSERT INTO t VALUES ("+itoa(i)+", 'v')")
	}
	for i := 0; i < 400; i += 2 {
		mustExec(t, d, "DELETE FROM t WHERE id = "+itoa(i))
	}
	r, err := d.Exec("SELECT COUNT(*) FROM t")
	if err != nil || r.Rows[0][0] != 400 {
		t.Fatalf("count after deletes: %+v %v", r, err)
	}
	r, err = d.Exec("SELECT * FROM t WHERE id = 3")
	if err != nil || len(r.Rows) != 1 {
		t.Fatalf("point lookup after deletes: %+v %v", r, err)
	}
	r, err = d.Exec("SELECT * FROM t WHERE id = 2")
	if err != nil || len(r.Rows) != 0 {
		t.Fatalf("deleted key must stay gone: %+v %v", r, err)
	}
	// Churn past several compaction cycles: Order must stay bounded and
	// every live row must remain visible.
	for round := 0; round < 10; round++ {
		for i := 600 + round*100; i < 700+round*100; i++ {
			mustExec(t, d, "INSERT INTO t VALUES ("+itoa(i)+", 'v')")
		}
		for i := 600 + round*100; i < 700+round*100; i += 2 {
			mustExec(t, d, "DELETE FROM t WHERE id = "+itoa(i))
		}
	}
	tbl := d.Tables["t"]
	tbl.mu.RLock()
	orderLen := len(tbl.Order)
	liveCount := len(tbl.Rows)
	tbl.mu.RUnlock()
	if orderLen > liveCount+liveCount/3+16 {
		t.Fatalf("Order not compacted: len(Order)=%d live=%d", orderLen, liveCount)
	}
	r, err = d.Exec("SELECT COUNT(*) FROM t")
	if err != nil || r.Rows[0][0] != liveCount {
		t.Fatalf("count mismatch after churn: %+v live=%d err=%v", r, liveCount, err)
	}
	// Deleted keys must be re-insertable.
	mustExec(t, d, "INSERT INTO t VALUES (0, 'back')")
	r, err = d.Exec("SELECT * FROM t WHERE id = 0")
	if err != nil || len(r.Rows) != 1 || r.Rows[0][1] != "back" {
		t.Fatalf("reinsert after delete failed: %+v %v", r, err)
	}
}
