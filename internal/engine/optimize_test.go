package engine

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// bigTable builds a table large enough to activate the fast row array
// (fastPathThreshold) and returns the database.
func bigTable(t *testing.T, n int, index bool) *Database {
	t.Helper()
	d := New()
	mustExec(t, d, "CREATE TABLE users (id INT PRIMARY KEY, name TEXT, score INT, active BOOL)")
	var b strings.Builder
	for i := 0; i < n; i += 50 {
		b.WriteString("INSERT INTO users VALUES ")
		for j := i; j < i+50 && j < n; j++ {
			if j > i {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, "(%d,'user-%d',%d,%v)", j, j, j%100, j%3 == 0)
		}
		mustExec(t, d, b.String())
		b.Reset()
	}
	if index {
		mustExec(t, d, "CREATE INDEX users_score ON users (score)")
	}
	return d
}

// tableFast reports whether the fast row array is active for table name.
func tableFast(d *Database, name string) bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	t := d.Tables[name]
	return t != nil && len(t.fast) > 0
}

func TestCloneIsExecutionReadyAndIndependent(t *testing.T) {
	d := bigTable(t, 600, true)
	if !tableFast(d, "users") {
		t.Fatal("expected fast path active")
	}
	// Create tombstones so the clone exercises dead counters too.
	for i := 0; i < 100; i += 2 {
		mustExec(t, d, fmt.Sprintf("DELETE FROM users WHERE id = %d", i))
	}
	clone, err := d.Clone()
	if err != nil {
		t.Fatal(err)
	}
	// Identical read results across scan, index, pk, and aggregate paths.
	for _, sql := range []string{
		"SELECT * FROM users ORDER BY score DESC LIMIT 12",
		"SELECT id FROM users WHERE score = 42 ORDER BY id",
		"SELECT * FROM users WHERE id = 301",
		"SELECT COUNT(*) FROM users",
		"SELECT COUNT(*) FROM users WHERE score = 7",
		"SELECT MIN(score) FROM users",
		"SELECT MAX(score) FROM users",
	} {
		got, err := clone.Exec(sql)
		if err != nil {
			t.Fatalf("clone %q: %v", sql, err)
		}
		want, err := d.Exec(sql)
		if err != nil {
			t.Fatalf("source %q: %v", sql, err)
		}
		gj, _ := json.Marshal(got)
		wj, _ := json.Marshal(want)
		if string(gj) != string(wj) {
			t.Fatalf("clone mismatch for %q:\n got %s\nwant %s", sql, gj, wj)
		}
	}
	// Mutating the clone must not leak into the source: row maps, fast
	// value slices, and index member sets are all private copies.
	mustExec(t, clone, "UPDATE users SET name = 'hacked' WHERE id = 1")
	mustExec(t, clone, "INSERT INTO users VALUES (9999, 'extra', 5, true)")
	mustExec(t, clone, "DELETE FROM users WHERE id = 3")
	r, err := d.Exec("SELECT name FROM users WHERE id = 1")
	if err != nil || r.Rows[0][0] != "user-1" {
		t.Fatalf("source saw clone mutation: %+v %v", r, err)
	}
	if r, err = d.Exec("SELECT COUNT(*) FROM users"); err != nil || r.Rows[0][0] != 550 {
		t.Fatalf("source row count changed: %+v %v", r, err)
	}
	// And the reverse: source mutations must not reach the clone.
	mustExec(t, d, "UPDATE users SET score = 12345 WHERE id = 5")
	r, err = clone.Exec("SELECT score FROM users WHERE id = 5")
	if err != nil || r.Rows[0][0] != int64(5) {
		t.Fatalf("clone saw source mutation: %+v %v", r, err)
	}
}

func TestCommitFromPublishesClone(t *testing.T) {
	d := bigTable(t, 300, false)
	clone, err := d.Clone()
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, clone, "INSERT INTO users VALUES (7777, 'new', 1, true)")
	mustExec(t, clone, "DELETE FROM users WHERE id = 0")
	if err := d.CommitFrom(clone, nil); err != nil {
		t.Fatal(err)
	}
	r, err := d.Exec("SELECT * FROM users WHERE id = 7777")
	if err != nil || len(r.Rows) != 1 {
		t.Fatalf("committed insert missing: %+v %v", r, err)
	}
	if r, err = d.Exec("SELECT COUNT(*) FROM users"); err != nil || r.Rows[0][0] != 300 {
		t.Fatalf("count after commit: %+v %v", r, err)
	}
}

func TestSnapshotJSONParity(t *testing.T) {
	d := New()
	mustExec(t, d, "CREATE TABLE a (id INT PRIMARY KEY, name TEXT, score FLOAT, active BOOL)")
	mustExec(t, d, "INSERT INTO a VALUES (1,'x<>&',1.5,true),(2,'it''s',2.25,false),(3,'uni é',0,true)")
	mustExec(t, d, "CREATE INDEX a_name ON a (name)")
	mustExec(t, d, "CREATE TABLE b (v TEXT)")
	mustExec(t, d, "INSERT INTO b VALUES ('p'),('q')")
	mustExec(t, d, "DELETE FROM a WHERE id = 2")
	mustExec(t, d, "CREATE TABLE empty (id INT PRIMARY KEY)")
	got, err := d.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	// The reference output is what the previous implementation produced:
	// a deep copy marshaled with encoding/json.
	clone, err := d.Clone()
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(databaseImage{Tables: clone.Tables})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("snapshot JSON differs from reflection path\ngot:  %s\nwant: %s", got, want)
	}
	// And it must still round-trip through UnmarshalJSON.
	restored := New()
	if err := json.Unmarshal(got, restored); err != nil {
		t.Fatal(err)
	}
	r, err := restored.Exec("SELECT COUNT(*) FROM a")
	if err != nil || r.Rows[0][0] != 2 {
		t.Fatalf("restored count: %+v %v", r, err)
	}
}

func TestOrderByLimitMatchesFullSort(t *testing.T) {
	d := bigTable(t, 800, false) // fast path active; scores collide (i%100)
	// Deterministic shuffle of insert order via updates is unnecessary:
	// ties on score exercise the stability tiebreaker already.
	full, err := d.Exec("SELECT id, score FROM users ORDER BY score DESC")
	if err != nil {
		t.Fatal(err)
	}
	limited, err := d.Exec("SELECT id, score FROM users ORDER BY score DESC LIMIT 17")
	if err != nil {
		t.Fatal(err)
	}
	if len(limited.Rows) != 17 {
		t.Fatalf("LIMIT 17 returned %d rows", len(limited.Rows))
	}
	for i := range limited.Rows {
		if fmt.Sprint(limited.Rows[i][0]) != fmt.Sprint(full.Rows[i][0]) {
			t.Fatalf("top-K row %d = %v, full sort = %v", i, limited.Rows[i], full.Rows[i])
		}
	}
	// Ascending + TEXT column + LIMIT boundary equal to a tie run.
	fullA, err := d.Exec("SELECT id, name FROM users ORDER BY name ASC")
	if err != nil {
		t.Fatal(err)
	}
	limA, err := d.Exec("SELECT id, name FROM users ORDER BY name ASC LIMIT 5")
	if err != nil || len(limA.Rows) != 5 {
		t.Fatal(err)
	}
	for i := range limA.Rows {
		if fmt.Sprint(limA.Rows[i][0]) != fmt.Sprint(fullA.Rows[i][0]) {
			t.Fatalf("asc top-K row %d = %v, full sort = %v", i, limA.Rows[i], fullA.Rows[i])
		}
	}
	// LIMIT larger than the set and no LIMIT must still work.
	all, err := d.Exec("SELECT id FROM users ORDER BY id DESC LIMIT 5000")
	if err != nil || len(all.Rows) != 800 {
		t.Fatalf("oversized limit: %d rows %v", len(all.Rows), err)
	}
}

func TestOrderBySmallTableUnchanged(t *testing.T) {
	d := New()
	mustExec(t, d, "CREATE TABLE s (id INT PRIMARY KEY, v INT, w TEXT)")
	mustExec(t, d, "INSERT INTO s VALUES (1,9,'b'),(2,1,'a'),(3,9,'c'),(4,1,'d')")
	r, err := d.Exec("SELECT id FROM s ORDER BY v ASC")
	if err != nil {
		t.Fatal(err)
	}
	ids := []int64{2, 4, 1, 3} // v ties keep insertion order
	for i, row := range r.Rows {
		if row[0] != ids[i] {
			t.Fatalf("row %d = %v, want id %d", i, row, ids[i])
		}
	}
	r, err = d.Exec("SELECT id FROM s ORDER BY v DESC LIMIT 2")
	if err != nil || len(r.Rows) != 2 || r.Rows[0][0] != int64(1) || r.Rows[1][0] != int64(3) {
		t.Fatalf("desc top-k: %+v %v", r, err)
	}
}

func TestCountStarFastPaths(t *testing.T) {
	d := bigTable(t, 600, true)
	// Tombstones: deleted rows must not be counted.
	mustExec(t, d, "DELETE FROM users WHERE id >= 550")
	r, err := d.Exec("SELECT COUNT(*) FROM users")
	if err != nil || r.Rows[0][0] != 550 {
		t.Fatalf("no-where count: %+v %v", r, err)
	}
	// Primary-key equality.
	r, err = d.Exec("SELECT COUNT(*) FROM users WHERE id = 42")
	if err != nil || r.Rows[0][0] != 1 {
		t.Fatalf("pk count: %+v %v", r, err)
	}
	r, err = d.Exec("SELECT COUNT(*) FROM users WHERE id = 424242")
	if err != nil || r.Rows[0][0] != 0 {
		t.Fatalf("pk miss count: %+v %v", r, err)
	}
	// Secondary-index equality: score = 7 matches ids 7,107,...,547 → 6.
	r, err = d.Exec("SELECT COUNT(*) FROM users WHERE score = 7")
	if err != nil || r.Rows[0][0] != 6 {
		t.Fatalf("indexed count: %+v %v", r, err)
	}
	// Index counts must agree with what SELECT returns.
	sel, err := d.Exec("SELECT id FROM users WHERE score = 7")
	if err != nil || len(sel.Rows) != 6 {
		t.Fatalf("indexed select: %+v %v", sel, err)
	}
	// Non-equality predicates still scan and stay correct.
	r, err = d.Exec("SELECT COUNT(*) FROM users WHERE score > 95")
	if err != nil {
		t.Fatal(err)
	}
	sel, err = d.Exec("SELECT id FROM users WHERE score > 95")
	if err != nil || r.Rows[0][0] != len(sel.Rows) {
		t.Fatalf("range count %v != select %d", r.Rows[0][0], len(sel.Rows))
	}
	// COUNT(column) still excludes NULLs.
	mustExec(t, d, "ALTER TABLE users ADD COLUMN nick TEXT")
	mustExec(t, d, "UPDATE users SET nick = 'n' WHERE id = 1")
	r, err = d.Exec("SELECT COUNT(nick) FROM users")
	if err != nil || r.Rows[0][0] != 1 {
		t.Fatalf("count(column): %+v %v", r, err)
	}
	// The aggregate column label keeps the historical lowercase form.
	if r.Columns[0] != "count(nick)" {
		t.Fatalf("aggregate column label: %v", r.Columns)
	}
}

func TestSmallTableCloneAndCount(t *testing.T) {
	// Under fastPathThreshold there is no fast array; clone + counts must
	// still be exact.
	d := New()
	mustExec(t, d, "CREATE TABLE small (id INT PRIMARY KEY, v TEXT)")
	mustExec(t, d, "INSERT INTO small VALUES (1,'a'),(2,'b'),(3,'c')")
	clone, err := d.Clone()
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, clone, "DELETE FROM small WHERE id = 1")
	if r, _ := d.Exec("SELECT COUNT(*) FROM small"); r.Rows[0][0] != 3 {
		t.Fatalf("source affected by clone delete")
	}
	if r, _ := clone.Exec("SELECT COUNT(*) FROM small WHERE v = 'b'"); r.Rows[0][0] != 1 {
		t.Fatalf("clone count wrong")
	}
}
