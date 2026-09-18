package engine

import "testing"

func TestCRUD(t *testing.T) {
	d := New()
	for _, q := range []string{"CREATE TABLE users (id INT PRIMARY KEY, name TEXT, active BOOL)", "INSERT INTO users VALUES (1, 'Ada', true)"} {
		if _, e := d.Exec(q); e != nil {
			t.Fatal(e)
		}
	}
	r, e := d.Exec("SELECT id, name FROM users WHERE id = 1")
	if e != nil || len(r.Rows) != 1 || r.Rows[0][1] != "Ada" {
		t.Fatalf("%+v %v", r, e)
	}
	if _, e = d.Exec("UPDATE users SET active = false WHERE id = 1"); e != nil {
		t.Fatal(e)
	}
	r, e = d.Exec("DELETE FROM users WHERE id = 1")
	if e != nil || r.Affected != 1 {
		t.Fatal(e, r)
	}
}

func TestBatchAndExpandedQueries(t *testing.T) {
	d := New()
	if _, err := d.Exec("CREATE TABLE users (id INT PRIMARY KEY, name TEXT, score INT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec("INSERT INTO users VALUES (1, 'Ada', 9), (2, 'Grace', 7), (3, 'Linus', 10)"); err != nil {
		t.Fatal(err)
	}
	result, err := d.Exec("SELECT name, score FROM users WHERE score = 9 OR score = 10 ORDER BY score DESC LIMIT 1")
	if err != nil || len(result.Rows) != 1 || result.Rows[0][0] != "Linus" {
		t.Fatalf("expanded select mismatch: result=%+v err=%v", result, err)
	}
	result, err = d.Exec("SELECT COUNT(*) FROM users WHERE score = 9 OR score = 10")
	if err != nil || len(result.Rows) != 1 || result.Rows[0][0] != 2 {
		t.Fatalf("aggregate mismatch: result=%+v err=%v", result, err)
	}
	results, err := d.ExecBatch([]string{"UPDATE users SET score = 8 WHERE id = 2", "DELETE FROM users WHERE id = 1"})
	if err != nil || len(results) != 2 || results[0].Affected != 1 || results[1].Affected != 1 {
		t.Fatalf("batch mismatch: results=%+v err=%v", results, err)
	}
}

func TestIndexesAndAlterTable(t *testing.T) {
	d := New()
	for _, query := range []string{
		"CREATE TABLE users (id INT PRIMARY KEY, name TEXT)",
		"INSERT INTO users VALUES (1, 'Ada'), (2, 'Grace')",
		"CREATE INDEX users_name ON users (name)",
		"ALTER TABLE users ADD COLUMN active BOOL",
	} {
		if _, err := d.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	result, err := d.Exec("SELECT id FROM users WHERE name = 'Grace'")
	if err != nil || len(result.Rows) != 1 || result.Rows[0][0] != int64(2) {
		t.Fatalf("indexed query mismatch: result=%+v err=%v", result, err)
	}
	result, err = d.Exec("SELECT active FROM users WHERE id = 1")
	if err != nil || len(result.Rows) != 1 || result.Rows[0][0] != nil {
		t.Fatalf("altered column mismatch: result=%+v err=%v", result, err)
	}
}
func BenchmarkPrimaryKeyLookup(b *testing.B) {
	d := New()
	d.Exec("CREATE TABLE users (id INT PRIMARY KEY, name TEXT)")
	for i := 0; i < 10000; i++ {
		d.Exec("INSERT INTO users VALUES (" + itoa(i) + ", 'user')")
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		d.Exec("SELECT * FROM users WHERE id = 5000")
	}
}
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	s := ""
	for i > 0 {
		s = string(rune('0'+i%10)) + s
		i /= 10
	}
	return s
}
