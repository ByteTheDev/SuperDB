package engine

import (
	"fmt"
	"testing"
)

func seedCompareTable(t *testing.T, rows int) *Database {
	t.Helper()
	d := New()
	mustExec(t, d, "CREATE TABLE t (id INT PRIMARY KEY, score INT, name TEXT)")
	var vals string
	for i := 1; i <= rows; i++ {
		score := i * 5
		if i > 1 {
			vals += ", "
		}
		vals += fmt.Sprintf("(%d, %d, 'n%d')", i, score, i)
	}
	mustExec(t, d, "INSERT INTO t VALUES "+vals)
	return d
}

func TestWhereComparisonOperators(t *testing.T) {
	// seedCompareTable gives row i: id=i, score=i*5, name='n'+i.
	cases := []struct {
		where string
		match func(id int) bool
	}{
		{"score = 10", func(id int) bool { return id*5 == 10 }},
		{"score != 10", func(id int) bool { return id*5 != 10 }},
		{"score <> 10", func(id int) bool { return id*5 != 10 }},
		{"score < 10", func(id int) bool { return id*5 < 10 }},
		{"score <= 10", func(id int) bool { return id*5 <= 10 }},
		{"score > 10", func(id int) bool { return id*5 > 10 }},
		{"score >= 10", func(id int) bool { return id*5 >= 10 }},
		{"id >= 2", func(id int) bool { return id >= 2 }},
		{"score >= 10 AND score <= 15", func(id int) bool { return id*5 >= 10 && id*5 <= 15 }},
		{"score < 5 OR score > 10", func(id int) bool { return id*5 < 5 || id*5 > 10 }},
		{"name >= 'n2'", func(id int) bool { return fmt.Sprintf("n%d", id) >= "n2" }},
	}
	for _, rows := range []int{3, 300} {
		t.Run(fmt.Sprint(rows), func(t *testing.T) {
			d := seedCompareTable(t, rows)
			for _, tc := range cases {
				var want []int64
				for id := 1; id <= rows; id++ {
					if tc.match(id) {
						want = append(want, int64(id))
					}
				}
				r, err := d.Exec("SELECT id FROM t WHERE " + tc.where + " ORDER BY id")
				if err != nil {
					t.Fatalf("%s: %v", tc.where, err)
				}
				if len(r.Rows) != len(want) {
					t.Fatalf("%s: got %d rows want %d", tc.where, len(r.Rows), len(want))
				}
				for i, row := range r.Rows {
					if row[0] != want[i] {
						t.Fatalf("%s: row %d = %v want id %d", tc.where, i, row, want[i])
					}
				}
			}
		})
	}
}

func TestWhereComparisonMutations(t *testing.T) {
	d := seedCompareTable(t, 3)
	r, err := d.Exec("DELETE FROM t WHERE score >= 10")
	if err != nil || r.Affected != 2 {
		t.Fatalf("delete: %v %+v", err, r)
	}
	d = seedCompareTable(t, 3)
	r, err = d.Exec("UPDATE t SET score = 99 WHERE id != 1")
	if err != nil || r.Affected != 2 {
		t.Fatalf("update: %v %+v", err, r)
	}
	r, _ = d.Exec("SELECT id FROM t WHERE score = 99 ORDER BY id")
	if len(r.Rows) != 2 || r.Rows[0][0] != int64(2) || r.Rows[1][0] != int64(3) {
		t.Fatalf("update rows: %+v", r.Rows)
	}
	r, err = d.Exec("SELECT COUNT(*) FROM t WHERE score >= 99")
	if err != nil || r.Rows[0][0] != 2 {
		t.Fatalf("aggregate: %v %+v", err, r)
	}
}

func TestWhereMissingColumnRejected(t *testing.T) {
	d := seedCompareTable(t, 3)
	for _, q := range []string{
		"SELECT * FROM t WHERE nope = 1",
		"SELECT * FROM t WHERE nope != 1",
		"SELECT COUNT(*) FROM t WHERE nope >= 1",
		"UPDATE t SET score = 1 WHERE nope = 1",
		"DELETE FROM t WHERE nope = 1",
	} {
		if _, err := d.Exec(q); err == nil {
			t.Fatalf("%s: missing column silently accepted", q)
		}
	}
	r, _ := d.Exec("SELECT COUNT(*) FROM t")
	if r.Rows[0][0] != 3 {
		t.Fatalf("rows changed: %+v", r.Rows)
	}
}

func TestQuotedLiteralEscapes(t *testing.T) {
	d := New()
	mustExec(t, d, "CREATE TABLE u (id INT PRIMARY KEY, name TEXT)")
	mustExec(t, d, "INSERT INTO u VALUES (1, 'it''s'), (2, 'a'',''b'), (3, '(x,y)')")
	r, err := d.Exec("SELECT id, name FROM u ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"it's", "a','b", "(x,y)"}
	if len(r.Rows) != 3 {
		t.Fatalf("rows: %+v", r.Rows)
	}
	for i, row := range r.Rows {
		if row[1] != want[i] {
			t.Fatalf("row %d: got %v want %v", i, row[1], want[i])
		}
	}
	// WHERE on the decoded value must match.
	r, err = d.Exec("SELECT id FROM u WHERE name = 'it''s'")
	if err != nil || len(r.Rows) != 1 || r.Rows[0][0] != int64(1) {
		t.Fatalf("escaped where: %v %+v", err, r)
	}
	// Comma and paren inside literal must not break row splitting.
	r, err = d.Exec("SELECT id FROM u WHERE name = '(x,y)'")
	if err != nil || len(r.Rows) != 1 || r.Rows[0][0] != int64(3) {
		t.Fatalf("paren where: %v %+v", err, r)
	}
}

func TestBindParamsApostropheRoundTrip(t *testing.T) {
	d := New()
	mustExec(t, d, "CREATE TABLE u (id INT PRIMARY KEY, name TEXT)")
	for _, value := range []string{"a'',''b", "it's", "plain", "''"} {
		bound, err := BindParams("INSERT INTO u VALUES (?, ?)", []any{len(value), value})
		if err != nil {
			t.Fatal(err)
		}
		mustExec(t, d, bound)
	}
	// ids are len(value): 7, 4, 5, 2 — ORDER BY id yields '' (2), it's (4),
	// plain (5), a'',''b (7).
	r, err := d.Exec("SELECT name FROM u ORDER BY id")
	if err != nil || len(r.Rows) != 4 {
		t.Fatalf("select: %v %+v", err, r)
	}
	want := []string{"''", "it's", "plain", "a'',''b"}
	for i, row := range r.Rows {
		if row[0] != want[i] {
			t.Fatalf("row %d round trip corrupted: got %v want %v", i, row[0], want[i])
		}
	}
}

func TestOrderByValidation(t *testing.T) {
	d := seedCompareTable(t, 3)
	if _, err := d.Exec("SELECT * FROM t ORDER BY nope"); err == nil {
		t.Fatal("ORDER BY unknown column accepted")
	}
	// Multiple spaces between ORDER and BY must still parse.
	r, err := d.Exec("SELECT id FROM t ORDER  BY id DESC")
	if err != nil || len(r.Rows) != 3 || r.Rows[0][0] != int64(3) {
		t.Fatalf("multi-space ORDER BY: %v %+v", err, r)
	}
}

func TestAggregateMissingColumnRejected(t *testing.T) {
	for _, rows := range []int{3, 300} {
		d := seedCompareTable(t, rows)
		for _, q := range []string{
			"SELECT COUNT(nope) FROM t",
			"SELECT MIN(nope) FROM t",
			"SELECT MAX(nope) FROM t",
		} {
			if _, err := d.Exec(q); err == nil {
				t.Fatalf("rows=%d %s: missing column accepted", rows, q)
			}
		}
	}
}

func TestTableWithoutPrimaryKey(t *testing.T) {
	d := New()
	mustExec(t, d, "CREATE TABLE logs (msg TEXT, lvl INT)")
	mustExec(t, d, "INSERT INTO logs VALUES ('a', 1), ('b', 2)")
	mustExec(t, d, "INSERT INTO logs VALUES ('c', 3)")
	r, err := d.Exec("SELECT msg FROM logs ORDER BY lvl")
	if err != nil || len(r.Rows) != 3 {
		t.Fatalf("select: %v %+v", err, r)
	}
	if r.Rows[0][0] != "a" || r.Rows[2][0] != "c" {
		t.Fatalf("order wrong: %+v", r.Rows)
	}
	// INSERT order must be deterministic without ORDER BY.
	r, _ = d.Exec("SELECT msg FROM logs")
	if r.Rows[0][0] != "a" || r.Rows[1][0] != "b" || r.Rows[2][0] != "c" {
		t.Fatalf("insertion order lost: %+v", r.Rows)
	}
	// Update and delete still work via synthetic keys.
	r, err = d.Exec("UPDATE logs SET lvl = 9 WHERE msg = 'b'")
	if err != nil || r.Affected != 1 {
		t.Fatalf("update: %v %+v", err, r)
	}
	r, err = d.Exec("DELETE FROM logs WHERE lvl >= 3")
	if err != nil || r.Affected != 2 {
		t.Fatalf("delete: %v %+v", err, r)
	}
	// Synthetic keys must survive a snapshot clone.
	c, err := d.Clone()
	if err != nil {
		t.Fatal(err)
	}
	r, _ = c.Exec("SELECT msg FROM logs")
	if len(r.Rows) != 1 || r.Rows[0][0] != "a" {
		t.Fatalf("clone lost rows: %+v", r.Rows)
	}
	mustExec(t, c, "INSERT INTO logs VALUES ('d', 4)")
	r, _ = c.Exec("SELECT COUNT(*) FROM logs")
	if r.Rows[0][0] != 2 {
		t.Fatalf("post-clone insert collided: %+v", r.Rows)
	}
}

func TestCreateIndexMultibyteNames(t *testing.T) {
	d := New()
	// 'ſ' uppercases to 'S' with a different byte length; the old
	// strings.ToUpper offset math corrupted the ON/table split.
	mustExec(t, d, "CREATE TABLE ſ_table (id INT PRIMARY KEY, v INT)")
	mustExec(t, d, "INSERT INTO ſ_table VALUES (1, 5), (2, 9)")
	mustExec(t, d, "CREATE INDEX ſ_idx ON ſ_table (v)")
	r, err := d.Exec("SELECT id FROM ſ_table WHERE v = 9")
	if err != nil || len(r.Rows) != 1 || r.Rows[0][0] != int64(2) {
		t.Fatalf("indexed select: %v %+v", err, r)
	}
}
