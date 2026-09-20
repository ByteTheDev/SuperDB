package engine

import (
	"fmt"
	"reflect"
	"testing"
)

func TestOrderedFilterAcrossFastPath(t *testing.T) {
	for _, rows := range []int{20, 300} {
		t.Run(fmt.Sprint(rows), func(t *testing.T) {
			d := New()
			mustExec(t, d, "CREATE TABLE t (id INT PRIMARY KEY, score INT)")
			for i := 0; i < rows; i++ {
				mustExec(t, d, fmt.Sprintf("INSERT INTO t VALUES (%d, %d)", i, i%10))
			}
			for _, indexed := range []bool{false, true} {
				if indexed {
					mustExec(t, d, "CREATE INDEX score_idx ON t (score)")
				}
				r, err := d.Exec("SELECT id FROM t WHERE score = 3 ORDER BY id DESC LIMIT 2")
				want := [][]any{{int64(rows - 7)}, {int64(rows - 17)}}
				if err != nil || !reflect.DeepEqual(r.Rows, want) {
					t.Fatalf("indexed=%t: got %+v err=%v want %v", indexed, r, err, want)
				}
			}
		})
	}
}

func TestNumericExtremaAndNullableCount(t *testing.T) {
	for _, rows := range []int{3, 300} {
		t.Run(fmt.Sprint(rows), func(t *testing.T) {
			d := New()
			mustExec(t, d, "CREATE TABLE t (id INT PRIMARY KEY, v INT, f FLOAT)")
			mustExec(t, d, "INSERT INTO t VALUES (0, 10, 10.5), (1, 2, 2.5), (2, NULL, NULL)")
			for i := 3; i < rows; i++ {
				mustExec(t, d, fmt.Sprintf("INSERT INTO t VALUES (%d, NULL, NULL)", i))
			}
			for _, tc := range []struct {
				query string
				want  any
			}{
				{"SELECT MIN(v) FROM t", int64(2)},
				{"SELECT MAX(v) FROM t", int64(10)},
				{"SELECT MIN(f) FROM t", 2.5},
				{"SELECT MAX(f) FROM t", 10.5},
				{"SELECT COUNT(v) FROM t", 2},
				{"SELECT COUNT(*) FROM t", rows},
				{"SELECT COUNT( * ) FROM t", rows},
			} {
				r, err := d.Exec(tc.query)
				if err != nil || len(r.Rows) != 1 || r.Rows[0][0] != tc.want {
					t.Errorf("%s: got %+v err=%v want %v", tc.query, r, err, tc.want)
				}
			}
		})
	}
}

func TestIntegerOrderingPreservesPrecision(t *testing.T) {
	d := New()
	mustExec(t, d, "CREATE TABLE t (id INT PRIMARY KEY)")
	mustExec(t, d, "INSERT INTO t VALUES (9007199254740993), (9007199254740992)")
	r, err := d.Exec("SELECT id FROM t ORDER BY id")
	want := [][]any{{int64(9007199254740992)}, {int64(9007199254740993)}}
	if err != nil || !reflect.DeepEqual(r.Rows, want) {
		t.Fatalf("integer ordering lost precision: %+v err=%v", r, err)
	}
}
