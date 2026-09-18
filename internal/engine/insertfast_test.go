package engine

import (
	"reflect"
	"testing"
)

func TestParseInsertRowsFastTracksEveryDuplicate(t *testing.T) {
	columns := []Column{
		{Name: "id", Type: Int, Primary: true},
		{Name: "name", Type: Text},
	}
	_, err := parseInsertRowsFast("(1, 'one'), (2, 'two'), (2, 'again')", columns)
	if err == nil || err.Error() != "duplicate primary key" {
		t.Fatalf("duplicate row was not rejected: %v", err)
	}
}

func TestParseInsertRowsFastMatchesExistingParser(t *testing.T) {
	columns := []Column{
		{Name: "id", Type: Int, Primary: true},
		{Name: "name", Type: Text},
		{Name: "score", Type: Float},
		{Name: "active", Type: Bool},
	}
	cases := []string{
		"(1, 'Ada', 10.5, true)",
		"(1, 'Ada, Grace', 10.5, true), (2, 'Linus', 8, false)",
		"(1, NULL, 0, false)",
		"(1, 'paren (inside)', 1.25, true)",
		"(1, 'Ada', 10.5)",
		"(1, 'Ada', nope, true)",
		"not a row",
	}
	for _, raw := range cases {
		got, gotErr := parseInsertRowsFast(raw, columns)
		want, wantErr := parseInsertRows(raw, columns)
		if (gotErr != nil) != (wantErr != nil) {
			t.Fatalf("error mismatch for %q: fast=%v existing=%v", raw, gotErr, wantErr)
		}
		if gotErr != nil {
			if gotErr.Error() != wantErr.Error() {
				t.Fatalf("error text mismatch for %q: fast=%q existing=%q", raw, gotErr, wantErr)
			}
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("result mismatch for %q: fast=%#v existing=%#v", raw, got, want)
		}
	}
}

func TestFastIndexedReadStaysCorrectAfterMutation(t *testing.T) {
	d := New()
	mustExec(t, d, "CREATE TABLE users (id INT PRIMARY KEY, score INT)")
	for i := 0; i < 300; i++ {
		mustExec(t, d, "INSERT INTO users VALUES ("+itoa(i)+", "+itoa(i%100)+")")
	}
	mustExec(t, d, "CREATE INDEX users_score ON users (score)")
	r, err := d.Exec("SELECT id FROM users WHERE score = 42")
	if err != nil || len(r.Rows) != 3 {
		t.Fatalf("initial indexed read: %+v %v", r, err)
	}
	mustExec(t, d, "UPDATE users SET score = 7 WHERE id = 42")
	mustExec(t, d, "DELETE FROM users WHERE id = 142")
	r, err = d.Exec("SELECT id FROM users WHERE score = 42")
	if err != nil || len(r.Rows) != 1 || r.Rows[0][0] != int64(242) {
		t.Fatalf("indexed read after mutation: %+v %v", r, err)
	}
}

func BenchmarkInsertSQL(b *testing.B) {
	const rowCount = 10000
	queries := make([]string, rowCount)
	for i := range queries {
		queries[i] = "INSERT INTO users VALUES (" + itoa(i) + ", 'user-', " + itoa(i%100) + ")"
	}
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		d := New()
		if _, err := d.Exec("CREATE TABLE users (id INT PRIMARY KEY, name TEXT, score INT)"); err != nil {
			b.Fatal(err)
		}
		for _, query := range queries {
			if _, err := d.Exec(query); err != nil {
				b.Fatal(err)
			}
		}
	}
}
