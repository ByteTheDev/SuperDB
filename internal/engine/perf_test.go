package engine

import (
	"strconv"
	"testing"
)

// seedTable builds a table with n rows for benchmarks and returns it.
func seedTable(b *testing.B, d *Database, n int, withIndex bool) {
	b.Helper()
	mustExec(b, d, "CREATE TABLE users (id INT PRIMARY KEY, name TEXT, score INT)")
	rows := make([]byte, 0, n*40)
	rows = append(rows, "INSERT INTO users VALUES "...)
	for i := 0; i < n; i++ {
		if i > 0 {
			rows = append(rows, ',')
		}
		rows = append(rows, '(')
		rows = strconv.AppendInt(rows, int64(i), 10)
		rows = append(rows, ',', '\'')
		rows = append(rows, "user-"...)
		rows = strconv.AppendInt(rows, int64(i), 10)
		rows = append(rows, '\'', ',')
		rows = strconv.AppendInt(rows, int64(i%100), 10)
		rows = append(rows, ')')
	}
	mustExec(b, d, string(rows))
	if withIndex {
		mustExec(b, d, "CREATE INDEX users_score ON users (score)")
	}
}

func BenchmarkClone(b *testing.B) {
	d := New()
	seedTable(b, d, 10000, false)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		clone, err := d.Clone()
		if err != nil {
			b.Fatal(err)
		}
		_ = clone
	}
}

func BenchmarkSnapshotMarshal(b *testing.B) {
	d := New()
	seedTable(b, d, 10000, false)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := d.Snapshot(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkOrderByLimit(b *testing.B) {
	d := New()
	seedTable(b, d, 20000, false)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := d.Exec("SELECT * FROM users ORDER BY score DESC LIMIT 10"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkOrderByTextLimit(b *testing.B) {
	d := New()
	seedTable(b, d, 20000, false)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := d.Exec("SELECT * FROM users ORDER BY name LIMIT 10"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkOrderByFull(b *testing.B) {
	d := New()
	seedTable(b, d, 20000, false)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := d.Exec("SELECT * FROM users ORDER BY score DESC"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCountStar(b *testing.B) {
	d := New()
	seedTable(b, d, 20000, false)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := d.Exec("SELECT COUNT(*) FROM users"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCountStarIndexedWhere(b *testing.B) {
	d := New()
	seedTable(b, d, 20000, true)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := d.Exec("SELECT COUNT(*) FROM users WHERE score = 42"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAtomicCommitFrom(b *testing.B) {
	d := New()
	seedTable(b, d, 10000, false)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		clone, err := d.Clone()
		if err != nil {
			b.Fatal(err)
		}
		if err := d.CommitFrom(clone, nil); err != nil {
			b.Fatal(err)
		}
	}
}
