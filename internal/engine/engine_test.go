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
