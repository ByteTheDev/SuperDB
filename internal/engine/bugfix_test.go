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
