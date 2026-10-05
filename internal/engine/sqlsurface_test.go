package engine

import (
	"fmt"
	"strings"
	"testing"
)

func TestDropTableAndIndex(t *testing.T) {
	d := New()
	for _, q := range []string{
		"CREATE TABLE users (id INT PRIMARY KEY, name TEXT)",
		"INSERT INTO users VALUES (1, 'Ada'), (2, 'Grace')",
		"CREATE INDEX users_name ON users (name)",
	} {
		mustExec(t, d, q)
	}
	// Index drop resolves the CREATE INDEX name and keeps the table usable.
	if _, err := d.Exec("DROP INDEX users_name ON users"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec("DROP INDEX users_name ON users"); err == nil {
		t.Fatal("expected error dropping index twice")
	}
	if _, err := d.Exec("DROP INDEX missing ON users"); err == nil {
		t.Fatal("expected error for unknown index")
	}
	r, err := d.Exec("SELECT id FROM users WHERE name = 'Grace'")
	if err != nil || len(r.Rows) != 1 || r.Rows[0][0] != int64(2) {
		t.Fatalf("select after DROP INDEX: %+v %v", r, err)
	}
	// DROP TABLE removes the table; IF EXISTS softens a repeated drop.
	if _, err = d.Exec("DROP TABLE users"); err != nil {
		t.Fatal(err)
	}
	if _, err = d.Exec("SELECT * FROM users"); err == nil {
		t.Fatal("expected error selecting dropped table")
	}
	if _, err = d.Exec("DROP TABLE users"); err == nil {
		t.Fatal("expected error dropping table twice")
	}
	if _, err = d.Exec("DROP TABLE IF EXISTS users"); err != nil {
		t.Fatal(err)
	}
	if _, err = d.Exec("DROP TABLE users extra"); err == nil {
		t.Fatal("expected error for trailing tokens")
	}
}

func TestAlterDropColumn(t *testing.T) {
	d := New()
	for _, q := range []string{
		"CREATE TABLE t (id INT PRIMARY KEY, name TEXT, score INT)",
		"INSERT INTO t VALUES (1, 'a', 5), (2, 'b', 7)",
	} {
		mustExec(t, d, q)
	}
	if _, err := d.Exec("ALTER TABLE t DROP COLUMN score"); err != nil {
		t.Fatal(err)
	}
	r, err := d.Exec("SELECT * FROM t ORDER BY id")
	if err != nil || len(r.Columns) != 2 || r.Rows[0][1] != "a" {
		t.Fatalf("after drop: %+v %v", r, err)
	}
	// COLUMN keyword is optional.
	if _, err = d.Exec("ALTER TABLE t DROP name"); err != nil {
		t.Fatal(err)
	}
	r, err = d.Exec("SELECT * FROM t")
	if err != nil || len(r.Columns) != 1 || r.Columns[0] != "id" {
		t.Fatalf("after second drop: %+v %v", r, err)
	}
	if _, err = d.Exec("ALTER TABLE t DROP COLUMN id"); err == nil {
		t.Fatal("expected error dropping the only column")
	}
	if _, err = d.Exec("ALTER TABLE t DROP COLUMN missing"); err == nil {
		t.Fatal("expected error dropping unknown column")
	}
	// Bare ALTER TABLE t DROP must error, not panic.
	if _, err = d.Exec("ALTER TABLE t DROP"); err == nil {
		t.Fatal("expected error for bare DROP")
	}
}

func TestAlterDropPrimaryColumnBecomesKeyless(t *testing.T) {
	d := New()
	for _, q := range []string{
		"CREATE TABLE t (id INT PRIMARY KEY, name TEXT)",
		"INSERT INTO t VALUES (1, 'a'), (2, 'b')",
		"ALTER TABLE t DROP COLUMN id",
	} {
		mustExec(t, d, q)
	}
	// Rows keep their old keys; new inserts get synthetic keys.
	if _, err := d.Exec("INSERT INTO t VALUES ('c')"); err != nil {
		t.Fatal(err)
	}
	r, err := d.Exec("SELECT * FROM t ORDER BY name")
	if err != nil || len(r.Rows) != 3 || r.Rows[0][0] != "a" || r.Rows[2][0] != "c" {
		t.Fatalf("keyless after PK drop: %+v %v", r, err)
	}
}

func TestShowTablesAndDescribe(t *testing.T) {
	d := New()
	mustExec(t, d, "CREATE TABLE users (id INT PRIMARY KEY, name TEXT, active BOOL)")
	mustExec(t, d, "CREATE TABLE accounts (id INT PRIMARY KEY)")
	mustExec(t, d, "CREATE INDEX users_name ON users (name)")
	r, err := d.Exec("SHOW TABLES")
	if err != nil || len(r.Rows) != 2 || r.Rows[0][0] != "accounts" || r.Rows[1][0] != "users" {
		t.Fatalf("SHOW TABLES: %+v %v", r, err)
	}
	for _, stmt := range []string{"DESCRIBE users", "DESC users", "SHOW COLUMNS FROM users"} {
		r, err = d.Exec(stmt)
		if err != nil || len(r.Rows) != 3 || r.Rows[0][0] != "id" || r.Rows[0][1] != "INT" || r.Rows[0][2] != true || r.Rows[2][2] != false {
			t.Fatalf("%s: %+v %v", stmt, r, err)
		}
	}
	for _, stmt := range []string{"SHOW INDEX FROM users", "SHOW INDEXES FROM users"} {
		r, err = d.Exec(stmt)
		if err != nil || len(r.Rows) != 1 || r.Rows[0][0] != "users_name" || r.Rows[0][1] != "name" {
			t.Fatalf("%s: %+v %v", stmt, r, err)
		}
	}
	if _, err = d.Exec("SHOW COLUMNS FROM missing"); err == nil {
		t.Fatal("expected error describing missing table")
	}
	if _, err = d.Exec("SHOW NONSENSE"); err == nil {
		t.Fatal("expected error for unknown SHOW")
	}
	if _, err = d.Exec("DESCRIBE"); err == nil {
		t.Fatal("expected error for bare DESCRIBE")
	}
	if _, err = d.Exec("DESCT users"); err == nil {
		t.Fatal("expected error for DESCT")
	}
}

func TestTruncate(t *testing.T) {
	d := New()
	for _, q := range []string{
		"CREATE TABLE t (id INT PRIMARY KEY, name TEXT)",
		"CREATE INDEX t_name ON t (name)",
		"INSERT INTO t VALUES (1, 'a'), (2, 'b')",
	} {
		mustExec(t, d, q)
	}
	if _, err := d.Exec("TRUNCATE t"); err != nil {
		t.Fatal(err)
	}
	r, err := d.Exec("SELECT * FROM t")
	if err != nil || len(r.Rows) != 0 {
		t.Fatalf("after truncate: %+v %v", r, err)
	}
	// Schema and index definitions survive; fresh rows index cleanly.
	mustExec(t, d, "INSERT INTO t VALUES (3, 'c')")
	r, err = d.Exec("SELECT id FROM t WHERE name = 'c'")
	if err != nil || len(r.Rows) != 1 || r.Rows[0][0] != int64(3) {
		t.Fatalf("indexed insert after truncate: %+v %v", r, err)
	}
	r, err = d.Exec("SHOW INDEX FROM t")
	if err != nil || len(r.Rows) != 1 || r.Rows[0][0] != "t_name" {
		t.Fatalf("index survives truncate: %+v %v", r, err)
	}
}

func TestLimitOffset(t *testing.T) {
	d := New()
	mustExec(t, d, "CREATE TABLE t (id INT PRIMARY KEY, v INT)")
	var vals []string
	for i := 1; i <= 10; i++ {
		vals = append(vals, fmt.Sprintf("(%d, %d)", i, i*10))
	}
	mustExec(t, d, "INSERT INTO t VALUES "+strings.Join(vals, ", "))
	r, err := d.Exec("SELECT id FROM t ORDER BY id LIMIT 3 OFFSET 2")
	if err != nil || len(r.Rows) != 3 || r.Rows[0][0] != int64(3) || r.Rows[2][0] != int64(5) {
		t.Fatalf("limit+offset: %+v %v", r, err)
	}
	r, err = d.Exec("SELECT id FROM t ORDER BY id LIMIT 2 OFFSET 9")
	if err != nil || len(r.Rows) != 1 {
		t.Fatalf("offset tail: %+v %v", r, err)
	}
	r, err = d.Exec("SELECT id FROM t ORDER BY id LIMIT 2 OFFSET 20")
	if err != nil || len(r.Rows) != 0 {
		t.Fatalf("offset past end: %+v %v", r, err)
	}
	r, err = d.Exec("SELECT id FROM t ORDER BY id OFFSET 8")
	if err != nil || len(r.Rows) != 2 {
		t.Fatalf("offset without limit: %+v %v", r, err)
	}
	for _, bad := range []string{
		"SELECT id FROM t LIMIT 2 OFFSET -1",
		"SELECT id FROM t LIMIT x OFFSET 2",
		"SELECT id FROM t OFFSET 2 LIMIT 1", // order is LIMIT then OFFSET
	} {
		if _, err := d.Exec(bad); err == nil {
			t.Fatalf("expected error: %s", bad)
		}
	}
}

func TestWhereInBetweenLike(t *testing.T) {
	d := New()
	mustExec(t, d, "CREATE TABLE t (id INT PRIMARY KEY, name TEXT, score FLOAT)")
	mustExec(t, d, "INSERT INTO t VALUES (1, 'ada', 1.5), (2, 'bob', 2.5), (3, 'cy', 3.5), (4, 'dee', 4.5)")
	// IN on numeric and text columns.
	r, err := d.Exec("SELECT id FROM t WHERE id IN (1, 3) ORDER BY id")
	if err != nil || len(r.Rows) != 2 || r.Rows[0][0] != int64(1) || r.Rows[1][0] != int64(3) {
		t.Fatalf("IN ints: %+v %v", r, err)
	}
	r, err = d.Exec("SELECT id FROM t WHERE name IN ('bob', 'dee', 'nobody') ORDER BY id")
	if err != nil || len(r.Rows) != 2 || r.Rows[0][0] != int64(2) || r.Rows[1][0] != int64(4) {
		t.Fatalf("IN strings: %+v %v", r, err)
	}
	// BETWEEN is inclusive on both bounds.
	r, err = d.Exec("SELECT id FROM t WHERE score BETWEEN 2.5 AND 4.5 ORDER BY id")
	if err != nil || len(r.Rows) != 3 || r.Rows[0][0] != int64(2) || r.Rows[2][0] != int64(4) {
		t.Fatalf("BETWEEN: %+v %v", r, err)
	}
	// BETWEEN combined with AND/OR keeps its operand AND private.
	r, err = d.Exec("SELECT id FROM t WHERE score BETWEEN 1.0 AND 2.0 OR id = 4 ORDER BY id")
	if err != nil || len(r.Rows) != 2 || r.Rows[0][0] != int64(1) || r.Rows[1][0] != int64(4) {
		t.Fatalf("BETWEEN+OR: %+v %v", r, err)
	}
	r, err = d.Exec("SELECT id FROM t WHERE id BETWEEN 1 AND 2 AND name LIKE 'a%'")
	if err != nil || len(r.Rows) != 1 || r.Rows[0][0] != int64(1) {
		t.Fatalf("BETWEEN+AND+LIKE: %+v %v", r, err)
	}
	// LIKE: % is a run wildcard, _ a single character.
	r, err = d.Exec("SELECT id FROM t WHERE name LIKE 'b_b'")
	if err != nil || len(r.Rows) != 1 || r.Rows[0][0] != int64(2) {
		t.Fatalf("LIKE _: %+v %v", r, err)
	}
	r, err = d.Exec("SELECT id FROM t WHERE name LIKE '%e%' ORDER BY id")
	if err != nil || len(r.Rows) != 1 || r.Rows[0][0] != int64(4) {
		t.Fatalf("LIKE %%: %+v %v", r, err)
	}
	for _, bad := range []string{
		"SELECT id FROM t WHERE id IN 1",
		"SELECT id FROM t WHERE id IN ()",
		"SELECT id FROM t WHERE id BETWEEN 1",
		"SELECT id FROM t WHERE id BETWEEN 1 AND 2 AND 3",
		"SELECT id FROM t WHERE id BETWEEN 1 OR 2 AND 3",
		"SELECT id FROM t WHERE name LIKE",
	} {
		if _, err := d.Exec(bad); err == nil {
			t.Fatalf("expected error: %s", bad)
		}
	}
}

func TestMutationWithNewOperators(t *testing.T) {
	d := New()
	mustExec(t, d, "CREATE TABLE t (id INT PRIMARY KEY, name TEXT, score INT)")
	mustExec(t, d, "INSERT INTO t VALUES (1, 'ada', 5), (2, 'bob', 7), (3, 'cy', 9), (4, 'dee', 11)")
	r, err := d.Exec("UPDATE t SET score = 0 WHERE id IN (1, 3)")
	if err != nil || r.Affected != 2 {
		t.Fatalf("UPDATE IN: %+v %v", r, err)
	}
	r, err = d.Exec("DELETE FROM t WHERE score BETWEEN 6 AND 10")
	if err != nil || r.Affected != 1 {
		t.Fatalf("DELETE BETWEEN: %+v %v", r, err)
	}
	r, err = d.Exec("DELETE FROM t WHERE name LIKE 'd%'")
	if err != nil || r.Affected != 1 {
		t.Fatalf("DELETE LIKE: %+v %v", r, err)
	}
	// Rows 1 and 3 survive: UPDATE zeroed their scores (below BETWEEN's
	// range) and neither name matches 'd%'.
	r, err = d.Exec("SELECT COUNT(*) FROM t")
	if err != nil || r.Rows[0][0] != 2 {
		t.Fatalf("remaining: %+v %v", r, err)
	}
	// Mutations still reject multi-term predicates other than BETWEEN's.
	if _, err = d.Exec("DELETE FROM t WHERE id = 1 AND id = 2"); err == nil {
		t.Fatal("expected error for two-term DELETE")
	}
}

func TestDistinct(t *testing.T) {
	d := New()
	mustExec(t, d, "CREATE TABLE t (id INT PRIMARY KEY, name TEXT)")
	mustExec(t, d, "INSERT INTO t VALUES (1, 'a'), (2, 'b'), (3, 'a'), (4, 'a'), (5, 'b')")
	r, err := d.Exec("SELECT DISTINCT name FROM t ORDER BY name")
	if err != nil || len(r.Rows) != 2 || r.Rows[0][0] != "a" || r.Rows[1][0] != "b" {
		t.Fatalf("DISTINCT: %+v %v", r, err)
	}
	r, err = d.Exec("SELECT DISTINCT name FROM t ORDER BY name LIMIT 1 OFFSET 1")
	if err != nil || len(r.Rows) != 1 || r.Rows[0][0] != "b" {
		t.Fatalf("DISTINCT+OFFSET: %+v %v", r, err)
	}
	// DISTINCT dedups projected rows: two columns keep duplicates of one.
	r, err = d.Exec("SELECT DISTINCT id, name FROM t WHERE name = 'a' ORDER BY id")
	if err != nil || len(r.Rows) != 3 {
		t.Fatalf("DISTINCT 2 cols: %+v %v", r, err)
	}
	if _, err = d.Exec("SELECT DISTINCT FROM t"); err == nil {
		t.Fatal("expected error for empty DISTINCT projection")
	}
}

func TestDistinctFastPath(t *testing.T) {
	d := bigTable(t, 300, false)
	// users.name is unique (user-N); project score%10 to force duplicates.
	r, err := d.Exec("SELECT DISTINCT score FROM t2_missing")
	if err == nil {
		t.Fatal("expected missing table error")
	}
	r, err = d.Exec("SELECT COUNT(*) FROM users")
	if err != nil || r.Rows[0][0] != 300 {
		t.Fatalf("seed: %+v %v", r, err)
	}
	// score values are j%100 → 100 distinct values across 300 rows.
	r, err = d.Exec("SELECT DISTINCT score FROM users")
	if err != nil || len(r.Rows) != 100 {
		t.Fatalf("fast DISTINCT: got %d rows", len(r.Rows))
	}
	// With WHERE: scores 0-9 each appear 3x across 300 rows → 10 unique.
	r, err = d.Exec("SELECT DISTINCT score FROM users WHERE score < 10")
	if err != nil || len(r.Rows) != 10 {
		t.Fatalf("fast DISTINCT+WHERE: got %d rows", len(r.Rows))
	}
}

func TestGroupByHaving(t *testing.T) {
	d := New()
	mustExec(t, d, "CREATE TABLE t (id INT PRIMARY KEY, dept TEXT, salary INT)")
	mustExec(t, d, "INSERT INTO t VALUES "+
		"(1, 'eng', 100), (2, 'eng', 200), (3, 'ops', 50), (4, 'ops', NULL), (5, 'hr', 10)")
	r, err := d.Exec("SELECT dept, COUNT(*) FROM t GROUP BY dept ORDER BY dept")
	if err != nil || len(r.Rows) != 3 {
		t.Fatalf("GROUP BY: %+v %v", r, err)
	}
	if r.Rows[0][0] != "eng" || r.Rows[0][1] != 2 || r.Rows[1][0] != "hr" || r.Rows[2][0] != "ops" {
		t.Fatalf("GROUP BY rows: %+v", r.Rows)
	}
	// COUNT(col) skips NULLs; SUM/AVG likewise.
	r, err = d.Exec("SELECT dept, COUNT(salary), SUM(salary), AVG(salary) FROM t GROUP BY dept ORDER BY dept")
	if err != nil || len(r.Rows) != 3 {
		t.Fatalf("grouped aggs: %+v %v", r, err)
	}
	if r.Rows[0][1] != 2 || r.Rows[0][2] != 300.0 || r.Rows[0][3] != 150.0 {
		t.Fatalf("eng row: %+v", r.Rows[0])
	}
	if r.Rows[2][1] != 1 || r.Rows[2][2] != 50.0 {
		t.Fatalf("ops row (NULL skipped): %+v", r.Rows[2])
	}
	// HAVING filters groups; ORDER BY resolves the aggregate name.
	r, err = d.Exec("SELECT dept, COUNT(*) FROM t GROUP BY dept HAVING COUNT(*) > 1 ORDER BY COUNT(*) DESC")
	if err != nil || len(r.Rows) != 2 || r.Rows[0][0] != "eng" || r.Rows[1][0] != "ops" {
		t.Fatalf("HAVING: %+v %v", r, err)
	}
	// HAVING on a grouping column.
	r, err = d.Exec("SELECT dept, COUNT(*) FROM t GROUP BY dept HAVING dept != 'hr' ORDER BY dept")
	if err != nil || len(r.Rows) != 2 {
		t.Fatalf("HAVING on column: %+v %v", r, err)
	}
	// WHERE filters before grouping.
	r, err = d.Exec("SELECT dept, COUNT(*) FROM t WHERE id <= 3 GROUP BY dept ORDER BY dept")
	if err != nil || len(r.Rows) != 2 || r.Rows[0][1] != 2 || r.Rows[1][1] != 1 {
		t.Fatalf("WHERE+GROUP: %+v %v", r, err)
	}
	// GROUP BY without aggregates behaves like DISTINCT.
	r, err = d.Exec("SELECT dept FROM t GROUP BY dept ORDER BY dept")
	if err != nil || len(r.Rows) != 3 {
		t.Fatalf("bare GROUP BY: %+v %v", r, err)
	}
	for _, bad := range []string{
		"SELECT name FROM t GROUP BY dept",                  // name not in GROUP BY
		"SELECT dept FROM t GROUP BY missing",               // unknown column
		"SELECT dept, COUNT(*) FROM t HAVING COUNT(*) > 1",  // HAVING w/o GROUP BY
		"SELECT dept, COUNT(*) FROM t GROUP BY dept HAVING", // empty HAVING
		"SELECT dept, COUNT(*) FROM t ORDER BY nosuch",      // bad ORDER BY
		"SELECT dept, SUM(*) FROM t GROUP BY dept",          // SUM(*) invalid
		"SELECT dept, COUNT(nosuch) FROM t GROUP BY dept",   // bad agg arg
		"SELECT * FROM t GROUP BY dept",                     // * with GROUP BY
	} {
		if _, err := d.Exec(bad); err == nil {
			t.Fatalf("expected error: %s", bad)
		}
	}
}

func TestAvg(t *testing.T) {
	d := New()
	mustExec(t, d, "CREATE TABLE t (id INT PRIMARY KEY, v INT, f FLOAT)")
	mustExec(t, d, "INSERT INTO t VALUES (1, 10, 1.5), (2, 20, 2.5), (3, NULL, NULL)")
	r, err := d.Exec("SELECT AVG(v) FROM t")
	if err != nil || r.Rows[0][0] != 15.0 {
		t.Fatalf("AVG int: %+v %v", r, err)
	}
	r, err = d.Exec("SELECT AVG(f) FROM t WHERE f > 1.0")
	if err != nil || r.Rows[0][0] != 2.0 {
		t.Fatalf("AVG float: %+v %v", r, err)
	}
	// All-NULL input yields NULL.
	r, err = d.Exec("SELECT AVG(v) FROM t WHERE id = 3")
	if err != nil || r.Rows[0][0] != nil {
		t.Fatalf("AVG null: %+v %v", r, err)
	}
	if _, err = d.Exec("SELECT AVG(name) FROM t"); err == nil {
		t.Fatal("expected error for unknown column")
	}
}

func TestInsertIntoSelect(t *testing.T) {
	d := New()
	for _, q := range []string{
		"CREATE TABLE src (id INT PRIMARY KEY, name TEXT, v INT)",
		"INSERT INTO src VALUES (1, 'a', 10), (2, 'b', 20), (3, 'c', 30)",
		"CREATE TABLE dst (id INT PRIMARY KEY, name TEXT, v INT)",
	} {
		mustExec(t, d, q)
	}
	r, err := d.Exec("INSERT INTO dst SELECT * FROM src WHERE v >= 20")
	if err != nil || r.Affected != 2 {
		t.Fatalf("INSERT SELECT: %+v %v", r, err)
	}
	r, err = d.Exec("SELECT id, name, v FROM dst ORDER BY id")
	if err != nil || len(r.Rows) != 2 || r.Rows[0][1] != "b" || r.Rows[1][2] != int64(30) {
		t.Fatalf("copied rows: %+v %v", r, err)
	}
	// Self-insert doubles rows under new... same keys → duplicates error.
	if _, err = d.Exec("INSERT INTO src SELECT * FROM src"); err == nil {
		t.Fatal("expected duplicate PK error on self-insert")
	}
	// An unsupported expression projects NULL keys, which collide as
	// batch duplicates — it must error rather than silently overwrite.
	if _, err = d.Exec("INSERT INTO dst SELECT id + 100, name, v FROM src"); err == nil {
		t.Fatal("expected error for expression in projection")
	}
	if _, err = d.Exec("INSERT INTO dst SELECT id, name FROM src"); err == nil {
		t.Fatal("expected column count mismatch")
	}
	if _, err = d.Exec("INSERT INTO missing SELECT * FROM src"); err == nil {
		t.Fatal("expected missing table error")
	}
}

func TestInsertOrReplace(t *testing.T) {
	d := New()
	for _, q := range []string{
		"CREATE TABLE t (id INT PRIMARY KEY, name TEXT)",
		"CREATE INDEX t_name ON t (name)",
		"INSERT INTO t VALUES (1, 'a'), (2, 'b'), (3, 'c')",
	} {
		mustExec(t, d, q)
	}
	// Replace an existing key and append a new one in one statement.
	r, err := d.Exec("INSERT OR REPLACE INTO t VALUES (2, 'bee'), (4, 'd')")
	if err != nil || r.Affected != 2 {
		t.Fatalf("OR REPLACE: %+v %v", r, err)
	}
	// The replaced row keeps its position; the new row appends.
	r, err = d.Exec("SELECT id, name FROM t ORDER BY id")
	if err != nil || len(r.Rows) != 4 {
		t.Fatalf("after replace: %+v %v", r, err)
	}
	if r.Rows[1][1] != "bee" || r.Rows[3][0] != int64(4) {
		t.Fatalf("replace rows: %+v", r.Rows)
	}
	// Secondary index reflects the replaced value and drops the old one.
	r, err = d.Exec("SELECT id FROM t WHERE name = 'bee'")
	if err != nil || len(r.Rows) != 1 || r.Rows[0][0] != int64(2) {
		t.Fatalf("index after replace: %+v %v", r, err)
	}
	r, err = d.Exec("SELECT id FROM t WHERE name = 'b'")
	if err != nil || len(r.Rows) != 0 {
		t.Fatalf("stale index entry: %+v %v", r, err)
	}
	// Later duplicate keys in one batch win.
	if _, err = d.Exec("INSERT OR REPLACE INTO t VALUES (5, 'x'), (5, 'y')"); err != nil {
		t.Fatal(err)
	}
	r, err = d.Exec("SELECT name FROM t WHERE id = 5")
	if err != nil || r.Rows[0][0] != "y" {
		t.Fatalf("last wins: %+v %v", r, err)
	}
	// Plain INSERT still rejects within-batch and existing duplicates.
	if _, err = d.Exec("INSERT INTO t VALUES (5, 'z')"); err == nil {
		t.Fatal("expected existing-PK error")
	}
	if _, err = d.Exec("INSERT INTO t VALUES (6, 'x'), (6, 'x2')"); err == nil {
		t.Fatal("expected batch dup error")
	}
}

func TestInsertOrReplaceFastPath(t *testing.T) {
	d := bigTable(t, 300, false)
	// Replace one row, append one row, verify the fast array stays aligned.
	r, err := d.Exec("INSERT OR REPLACE INTO users VALUES (5, 'replaced', 99, true), (300, 'new', 1, false)")
	if err != nil || r.Affected != 2 {
		t.Fatalf("fast OR REPLACE: %+v %v", r, err)
	}
	r, err = d.Exec("SELECT name FROM users WHERE id = 5")
	if err != nil || len(r.Rows) != 1 || r.Rows[0][0] != "replaced" {
		t.Fatalf("fast replace: %+v %v", r, err)
	}
	r, err = d.Exec("SELECT name FROM users WHERE id = 300")
	if err != nil || len(r.Rows) != 1 || r.Rows[0][0] != "new" {
		t.Fatalf("fast append: %+v %v", r, err)
	}
	// Order is preserved for the replaced key: id=5 stays at position 6.
	r, err = d.Exec("SELECT id FROM users LIMIT 6 OFFSET 5")
	if err != nil || r.Rows[0][0] != int64(5) {
		t.Fatalf("order preserved: %+v %v", r, err)
	}
}

func TestSelectHeadRejectsJunk(t *testing.T) {
	d := New()
	mustExec(t, d, "CREATE TABLE t (id INT PRIMARY KEY)")
	mustExec(t, d, "INSERT INTO t VALUES (1)")
	for _, bad := range []string{
		"SELECT * FROM t u WHERE id = 1",        // alias unsupported
		"SELECT * FROM t, u WHERE id = 1",       // second table unsupported
		"SELECT * FROM t JOIN u ON t.id = u.id", // JOIN unsupported
		"SELECT * FROM t LIMIT 1 WHERE id = 1",  // misordered clauses
		"SELECT * FROM t OFFSET 1 WHERE id = 1",
		"SELECT * FROM t WHERE id = 1 LIMIT 1 ORDER BY id",
		"SELECT * FROM t WHERE",
		"SELECT * FROM t GROUP BY",
		"SELECT * FROM t ORDER BY",
		"SELECT * FROM t LIMIT",
		"SELECT * FROM t OFFSET",
		"SELECT * FROM t LIMIT 1.5",
		"SELECT * FROM t ORDER BY id SIDEWAYS",
	} {
		if _, err := d.Exec(bad); err == nil {
			t.Fatalf("expected error: %s", bad)
		}
	}
}

func TestIsReadOnly(t *testing.T) {
	for _, read := range []string{
		"SELECT * FROM t", "select 1", " SHOW TABLES", "SHOW COLUMNS FROM t",
		"DESCRIBE t", "DESC t",
	} {
		if !IsReadOnly(read) {
			t.Fatalf("read-only misclassified: %s", read)
		}
	}
	for _, write := range []string{
		"INSERT INTO t VALUES (1)", "INSERT OR REPLACE INTO t VALUES (1)",
		"DROP TABLE t", "DROP INDEX i ON t", "TRUNCATE t",
		"ALTER TABLE t DROP COLUMN c", "UPDATE t SET x = 1", "DELETE FROM t",
		"CREATE TABLE t (id INT)", "CREATE INDEX i ON t (id)", "garbage",
	} {
		if IsReadOnly(write) {
			t.Fatalf("write misclassified: %s", write)
		}
	}
}

func TestIndexNamesSurviveSnapshot(t *testing.T) {
	d := New()
	mustExec(t, d, "CREATE TABLE t (id INT PRIMARY KEY, name TEXT)")
	mustExec(t, d, "INSERT INTO t VALUES (1, 'a')")
	mustExec(t, d, "CREATE INDEX my_idx ON t (name)")
	snap, err := d.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	d2 := New()
	if err := d2.UnmarshalJSON(snap); err != nil {
		t.Fatal(err)
	}
	// The index name round-trips: drop by name still works after restore.
	if _, err := d2.Exec("DROP INDEX my_idx ON t"); err != nil {
		t.Fatalf("drop restored index by name: %v", err)
	}
	// A snapshot lacking IndexNames (legacy shape) decodes and the index
	// remains droppable by its column name.
	legacy := `{"Tables":{"t":{"Name":"t","Columns":[{"Name":"id","Type":"INT","Primary":true},{"Name":"name","Type":"TEXT","Primary":false}],"Rows":{"1":{"id":1,"name":"a"}},"Order":["1"],"Indexes":{"name":{"a":{}}}}}}`
	d3 := New()
	if err := d3.UnmarshalJSON([]byte(legacy)); err != nil {
		t.Fatal(err)
	}
	if _, err := d3.Exec("DROP INDEX name ON t"); err != nil {
		t.Fatalf("drop legacy index by column: %v", err)
	}
	r, err := d3.Exec("SHOW INDEX FROM t")
	if err != nil || len(r.Rows) != 0 {
		t.Fatalf("legacy drop verified: %+v %v", r, err)
	}
}
