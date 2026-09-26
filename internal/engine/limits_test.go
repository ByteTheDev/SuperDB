package engine

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestExecContextCanceledBeforeExec(t *testing.T) {
	d := New()
	if _, err := d.Exec("CREATE TABLE t (id INT PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := d.ExecContext(ctx, "SELECT * FROM t"); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if _, err := d.ExecContext(ctx, "INSERT INTO t VALUES (1)"); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled for write, got %v", err)
	}
}

func TestExecContextCancelsMidScan(t *testing.T) {
	d := New()
	if _, err := d.Exec("CREATE TABLE t (id INT PRIMARY KEY, v INT)"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 256; i++ {
		if _, err := d.Exec(fmt.Sprintf("INSERT INTO t VALUES (%d, %d)", i, i)); err != nil {
			t.Fatal(err)
		}
	}
	// The first row-tick check happens before the scan does meaningful
	// work, so an already-cancelled context aborts the query.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := d.ExecContext(ctx, "SELECT * FROM t WHERE v = 7"); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled mid-scan, got %v", err)
	}
	if _, err := d.ExecContext(ctx, "SELECT COUNT(*) FROM t"); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled in aggregate scan, got %v", err)
	}
	if _, err := d.ExecContext(ctx, "UPDATE t SET v = 0 WHERE v = 7"); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled in update scan, got %v", err)
	}
	if _, err := d.ExecContext(ctx, "DELETE FROM t WHERE v = 7"); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled in delete scan, got %v", err)
	}
}

func TestExecWithOptionsMaxRows(t *testing.T) {
	d := New()
	if _, err := d.Exec("CREATE TABLE t (id INT PRIMARY KEY, v TEXT)"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err := d.Exec(fmt.Sprintf("INSERT INTO t VALUES (%d, 'x')", i)); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()

	// Full scan exceeds the cap.
	_, err := d.ExecWithOptions(ctx, "SELECT * FROM t", ExecOptions{MaxRows: 3})
	if !errors.Is(err, ErrResultTooLarge) {
		t.Fatalf("expected ErrResultTooLarge, got %v", err)
	}
	// Filtered scan exceeds the cap.
	_, err = d.ExecWithOptions(ctx, "SELECT * FROM t WHERE v = 'x'", ExecOptions{MaxRows: 3})
	if !errors.Is(err, ErrResultTooLarge) {
		t.Fatalf("expected ErrResultTooLarge on filtered path, got %v", err)
	}
	// An explicit LIMIT smaller than the cap still wins.
	r, err := d.ExecWithOptions(ctx, "SELECT * FROM t LIMIT 2", ExecOptions{MaxRows: 3})
	if err != nil || len(r.Rows) != 2 {
		t.Fatalf("LIMIT below cap must succeed: %+v err=%v", r, err)
	}
	// A LIMIT above the cap is not silently truncated: it errors.
	_, err = d.ExecWithOptions(ctx, "SELECT * FROM t LIMIT 5", ExecOptions{MaxRows: 3})
	if !errors.Is(err, ErrResultTooLarge) {
		t.Fatalf("expected ErrResultTooLarge for over-cap LIMIT, got %v", err)
	}
	// Exactly at the cap is allowed.
	r, err = d.ExecWithOptions(ctx, "SELECT * FROM t LIMIT 3", ExecOptions{MaxRows: 3})
	if err != nil || len(r.Rows) != 3 {
		t.Fatalf("at-cap result must succeed: %+v err=%v", r, err)
	}
	// Disabled cap keeps unlimited behavior.
	r, err = d.ExecWithOptions(ctx, "SELECT * FROM t", ExecOptions{})
	if err != nil || len(r.Rows) != 5 {
		t.Fatalf("unlimited result mismatch: %+v err=%v", r, err)
	}
	// The ordered path honors the cap too.
	_, err = d.ExecWithOptions(ctx, "SELECT * FROM t ORDER BY id DESC", ExecOptions{MaxRows: 2})
	if !errors.Is(err, ErrResultTooLarge) {
		t.Fatalf("ordered path must enforce cap, got %v", err)
	}
	// Aggregates return a single row and are unaffected.
	r, err = d.ExecWithOptions(ctx, "SELECT COUNT(*) FROM t", ExecOptions{MaxRows: 1})
	if err != nil || r.Rows[0][0] != 5 {
		t.Fatalf("aggregate mismatch: %+v err=%v", r, err)
	}
}

func TestExecBatchContextCancel(t *testing.T) {
	d := New()
	if _, err := d.Exec("CREATE TABLE t (id INT PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := d.ExecBatchContext(ctx, []string{"INSERT INTO t VALUES (1)", "INSERT INTO t VALUES (2)"}, ExecOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if len(res) != 0 {
		t.Fatalf("cancelled batch must not apply statements: %+v", res)
	}
}

func TestRowTicker(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tk := &rowTicker{}
	if err := tk.tick(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("first tick must observe a canceled context: %v", err)
	}
	tk = &rowTicker{}
	for i := 0; i < 200; i++ {
		if err := tk.tick(context.Background()); err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
	}
}

func TestRowCount(t *testing.T) {
	d := New()
	if d.RowCount() != 0 {
		t.Fatal("empty database must report 0 rows")
	}
	if _, err := d.Exec("CREATE TABLE a (id INT PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec("CREATE TABLE b (id INT PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec("INSERT INTO a VALUES (1), (2)"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec("INSERT INTO b VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	if d.RowCount() != 3 {
		t.Fatalf("RowCount=%d, want 3", d.RowCount())
	}
	if _, err := d.Exec("DELETE FROM b WHERE id = 1"); err != nil {
		t.Fatal(err)
	}
	if d.RowCount() != 2 {
		t.Fatalf("RowCount=%d after delete, want 2", d.RowCount())
	}
}
