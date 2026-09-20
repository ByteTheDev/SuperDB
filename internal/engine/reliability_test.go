package engine

import (
	"fmt"
	"testing"
)

func TestSnapshotPreservesIntegerPredicates(t *testing.T) {
	d := New()
	for _, q := range []string{"CREATE TABLE t (id INT PRIMARY KEY, v INT)", "INSERT INTO t VALUES (9007199254740993, 7)"} {
		if _, err := d.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	c, err := d.Clone()
	if err != nil {
		t.Fatal(err)
	}
	r, err := c.Exec("SELECT id FROM t WHERE v = 7")
	if err != nil || len(r.Rows) != 1 || r.Rows[0][0] != int64(9007199254740993) {
		t.Fatalf("clone changed integers: %+v %v", r, err)
	}
}

func TestDeletedRowsStayDeleted(t *testing.T) {
	for _, count := range []int{20, 300} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			d := New()
			d.Exec("CREATE TABLE t (id INT PRIMARY KEY)")
			for i := 0; i < count; i++ {
				d.Exec(fmt.Sprintf("INSERT INTO t VALUES (%d)", i))
			}
			d.Exec("DELETE FROM t WHERE id = 1")
			r, err := d.Exec("SELECT * FROM t ORDER BY id")
			if err != nil || len(r.Rows) != count-1 {
				t.Fatalf("deleted row scanned: %d %v", len(r.Rows), err)
			}
			d.Exec("INSERT INTO t VALUES (1)")
			r, err = d.Exec("SELECT COUNT(*) FROM t")
			if err != nil || r.Rows[0][0] != count {
				t.Fatalf("reinsert duplicated: %+v %v", r, err)
			}
		})
	}
}

func TestInvalidMutationDoesNotChangeRows(t *testing.T) {
	for _, q := range []string{"DELETE FROM t WHERE id > 1", "DELETE FROM t WHERE", "UPDATE t SET v = 9 WHERE id > 1", "UPDATE t SET v = 9 WHERE"} {
		t.Run(q, func(t *testing.T) {
			d := New()
			d.Exec("CREATE TABLE t (id INT PRIMARY KEY, v INT)")
			d.Exec("INSERT INTO t VALUES (1, 2)")
			if _, err := d.Exec(q); err == nil {
				t.Fatal("invalid predicate accepted")
			}
			r, _ := d.Exec("SELECT * FROM t")
			if len(r.Rows) != 1 || r.Rows[0][1] != int64(2) {
				t.Fatalf("mutation changed data: %+v", r)
			}
		})
	}
}

func TestPrimaryKeyUpdateRekeysIndexes(t *testing.T) {
	d := New()
	for _, q := range []string{"CREATE TABLE t (id INT PRIMARY KEY, v TEXT)", "INSERT INTO t VALUES (1, 'x'), (2, 'y')", "CREATE INDEX v_idx ON t (v)", "UPDATE t SET id = 3 WHERE id = 1"} {
		if _, err := d.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	r, _ := d.Exec("SELECT * FROM t WHERE id = 3")
	if len(r.Rows) != 1 {
		t.Fatal("new primary key missing")
	}
	if _, err := d.Exec("UPDATE t SET id = 2 WHERE id = 3"); err == nil {
		t.Fatal("duplicate primary key accepted")
	}
	r, _ = d.Exec("SELECT id FROM t WHERE v = 'x'")
	if len(r.Rows) != 1 || r.Rows[0][0] != int64(3) {
		t.Fatalf("index corrupt: %+v", r)
	}
}
