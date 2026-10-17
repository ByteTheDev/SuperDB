package engine

import (
	"testing"
)

func TestSelectUnknownProjectionErrors(t *testing.T) {
	d := New()
	mustExec(t, d, "CREATE TABLE t (id INT PRIMARY KEY, name TEXT)")
	mustExec(t, d, "INSERT INTO t VALUES (1, 'a')")
	for _, q := range []string{
		"SELECT nosuch FROM t",
		"SELECT id, nosuch FROM t",
		"SELECT DISTINCT nosuch FROM t",
	} {
		if _, err := d.Exec(q); err == nil {
			t.Fatalf("expected column error: %s", q)
		}
	}
	// Valid projections still work.
	r, err := d.Exec("SELECT id, name FROM t ORDER BY id")
	if err != nil || len(r.Rows) != 1 {
		t.Fatalf("valid projection failed: %+v %v", r, err)
	}
}

func TestAggregateStarRejectedExceptCount(t *testing.T) {
	d := New()
	mustExec(t, d, "CREATE TABLE t (id INT PRIMARY KEY, v INT)")
	mustExec(t, d, "INSERT INTO t VALUES (1, 10), (2, 20)")
	if _, err := d.Exec("SELECT COUNT(*) FROM t"); err != nil {
		t.Fatalf("COUNT(*) must work: %v", err)
	}
	for _, q := range []string{
		"SELECT SUM(*) FROM t",
		"SELECT AVG(*) FROM t",
		"SELECT MIN(*) FROM t",
		"SELECT MAX(*) FROM t",
	} {
		if _, err := d.Exec(q); err == nil {
			t.Fatalf("expected * rejection: %s", q)
		}
	}
}

func TestUpdateUnknownColumnErrors(t *testing.T) {
	d := New()
	mustExec(t, d, "CREATE TABLE t (id INT PRIMARY KEY, v TEXT)")
	mustExec(t, d, "INSERT INTO t VALUES (1, 'a')")
	for _, q := range []string{
		"UPDATE t SET nosuch = 'x' WHERE id = 1",
		"UPDATE t SET nosuch = NULL WHERE id = 1",
	} {
		if _, err := d.Exec(q); err == nil {
			t.Fatalf("expected column error: %s", q)
		}
	}
	// Row must not gain a new key.
	r, err := d.Exec("SELECT id, v FROM t")
	if err != nil || len(r.Rows) != 1 || len(r.Rows[0]) != 2 {
		t.Fatalf("row polluted: %+v %v", r, err)
	}
}

func TestCreateTableValidation(t *testing.T) {
	d := New()
	for _, q := range []string{
		"CREATE TABLE t (id FOO)",
		"CREATE TABLE t (id INT, id TEXT)",
		"CREATE TABLE t (a INT PRIMARY KEY, b INT PRIMARY KEY)",
	} {
		if _, err := d.Exec(q); err == nil {
			t.Fatalf("expected create error: %s", q)
		}
	}
	if _, err := d.Exec("CREATE TABLE ok (id INT PRIMARY KEY, v TEXT)"); err != nil {
		t.Fatalf("valid create failed: %v", err)
	}
}

// The compiled WHERE path resolves literals against the column type, so
// `id = 01`, `v = 1.50`, and `k = TRUE` all match stored values. The
// primary-key and index lookup shortcuts must use the same normalized key
// or a semantically equivalent predicate silently returns no rows.
func TestSelectEqualityNormalizesLiteralKey(t *testing.T) {
	d := New()
	mustExec(t, d, "CREATE TABLE t (id INT PRIMARY KEY, name TEXT)")
	mustExec(t, d, "INSERT INTO t VALUES (1, 'a'), (20, 'b')")
	for _, q := range []string{
		"SELECT name FROM t WHERE id = 01",
		"SELECT name FROM t WHERE id = +1",
		"SELECT name FROM t WHERE id = '1'",
	} {
		r, err := d.Exec(q)
		if err != nil || len(r.Rows) != 1 || r.Rows[0][0] != "a" {
			t.Fatalf("%s returned %+v err=%v; the row must match", q, r, err)
		}
	}
	if r, err := d.Exec("SELECT COUNT(*) FROM t WHERE id = 01"); err != nil || r.Rows[0][0] != 1 {
		t.Fatalf("COUNT(*) with non-canonical literal: %+v %v", r, err)
	}
	// Values that cannot convert to the column type still match nothing,
	// exactly like the compiled path.
	if r, err := d.Exec("SELECT name FROM t WHERE id = 'abc'"); err != nil || len(r.Rows) != 0 {
		t.Fatalf("non-convertible literal must miss: %+v %v", r, err)
	}
}

func TestSelectEqualityNormalizesFloatAndBoolKeys(t *testing.T) {
	d := New()
	mustExec(t, d, "CREATE TABLE f (v FLOAT PRIMARY KEY, tag TEXT)")
	mustExec(t, d, "INSERT INTO f VALUES (1.5, 'x'), (1000, 'y')")
	for _, q := range []string{
		"SELECT tag FROM f WHERE v = 1.50",
		"SELECT tag FROM f WHERE v = 1.5e0",
	} {
		r, err := d.Exec(q)
		if err != nil || len(r.Rows) != 1 || r.Rows[0][0] != "x" {
			t.Fatalf("%s returned %+v err=%v; the row must match", q, r, err)
		}
	}
	if r, err := d.Exec("SELECT tag FROM f WHERE v = 1e3"); err != nil || len(r.Rows) != 1 || r.Rows[0][0] != "y" {
		t.Fatalf("float exponent literal must match stored 1000: %+v %v", r, err)
	}
	mustExec(t, d, "CREATE TABLE b (k BOOL PRIMARY KEY)")
	mustExec(t, d, "INSERT INTO b VALUES (true), (false)")
	for _, q := range []string{
		"SELECT k FROM b WHERE k = TRUE",
		"SELECT k FROM b WHERE k = 1",
	} {
		r, err := d.Exec(q)
		if err != nil || len(r.Rows) != 1 || r.Rows[0][0] != true {
			t.Fatalf("%s returned %+v err=%v; the true row must match", q, r, err)
		}
	}
}

func TestSelectEqualityNormalizesIndexKey(t *testing.T) {
	d := New()
	mustExec(t, d, "CREATE TABLE ix (id INT PRIMARY KEY, score FLOAT, n INT)")
	mustExec(t, d, "INSERT INTO ix VALUES (1, 2.5, 42), (2, 3.5, 7)")
	mustExec(t, d, "CREATE INDEX ix_score ON ix (score)")
	mustExec(t, d, "CREATE INDEX ix_n ON ix (n)")
	for _, q := range []string{
		"SELECT id FROM ix WHERE score = 2.50",
		"SELECT id FROM ix WHERE score = 2.5e0",
		"SELECT id FROM ix WHERE n = 042",
		"SELECT id FROM ix WHERE n = +42",
	} {
		r, err := d.Exec(q)
		if err != nil || len(r.Rows) != 1 || r.Rows[0][0] != int64(1) {
			t.Fatalf("%s returned %+v err=%v; the indexed row must match", q, r, err)
		}
	}
	for _, q := range []string{
		"SELECT COUNT(*) FROM ix WHERE score = 2.50",
		"SELECT COUNT(*) FROM ix WHERE n = 042",
	} {
		r, err := d.Exec(q)
		if err != nil || r.Rows[0][0] != 1 {
			t.Fatalf("%s returned %+v err=%v; count must be 1", q, r, err)
		}
	}
}

func TestSelectEqualityNormalizesLiteralKeyOnFastPath(t *testing.T) {
	d := New()
	mustExec(t, d, "CREATE TABLE big (id INT PRIMARY KEY, v INT)")
	for i := 0; i < 300; i++ {
		mustExec(t, d, "INSERT INTO big VALUES ("+itoa(i)+", "+itoa(i)+")")
	}
	for _, q := range []string{
		"SELECT v FROM big WHERE id = 007",
		"SELECT v FROM big WHERE id = +7",
	} {
		r, err := d.Exec(q)
		if err != nil || len(r.Rows) != 1 || r.Rows[0][0] != int64(7) {
			t.Fatalf("%s returned %+v err=%v; the row must match on the fast path", q, r, err)
		}
	}
	if r, err := d.Exec("SELECT COUNT(*) FROM big WHERE id = 007"); err != nil || r.Rows[0][0] != 1 {
		t.Fatalf("COUNT(*) fast path with non-canonical literal: %+v %v", r, err)
	}
}
