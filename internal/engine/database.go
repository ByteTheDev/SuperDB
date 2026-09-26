package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrResultTooLarge is returned (wrapped) when a SELECT would emit more
// rows than ExecOptions.MaxRows permits. Callers distinguish it with
// errors.Is to map it to a stable error code.
var ErrResultTooLarge = errors.New("result row limit exceeded")

// ExecOptions carries per-statement execution limits. Zero values keep
// the historical unbounded behavior.
type ExecOptions struct {
	// MaxRows caps the number of rows a SELECT may return. A query that
	// would emit more aborts with ErrResultTooLarge instead of
	// materializing an unbounded result. <= 0 means unlimited.
	MaxRows int
}

func New() *Database { return &Database{Tables: make(map[string]*Table)} }

// ExecBatch executes statements in order and returns one result per statement.
// Callers that need atomicity should use a transaction at the server boundary.
func (d *Database) ExecBatch(sqls []string) ([]Result, error) {
	return d.ExecBatchContext(context.Background(), sqls, ExecOptions{})
}

// ExecBatchContext is ExecBatch with cancellation and per-statement limits.
// The context is checked between statements, so a cancelled batch stops at
// the next statement boundary.
func (d *Database) ExecBatchContext(ctx context.Context, sqls []string, opts ExecOptions) ([]Result, error) {
	results := make([]Result, 0, len(sqls))
	for i, sql := range sqls {
		result, err := d.ExecWithOptions(ctx, sql, opts)
		if err != nil {
			return results, fmt.Errorf("statement %d: %w", i+1, err)
		}
		results = append(results, result)
	}
	return results, nil
}

func (d *Database) Exec(sql string) (Result, error) {
	return d.ExecWithOptions(context.Background(), sql, ExecOptions{})
}

// ExecContext is Exec with cancellation. Scan loops poll ctx periodically,
// so a cancelled or timed-out statement aborts mid-scan instead of running
// to completion.
func (d *Database) ExecContext(ctx context.Context, sql string) (Result, error) {
	return d.ExecWithOptions(ctx, sql, ExecOptions{})
}

// ExecWithOptions executes one statement honoring ctx cancellation and
// opts limits. Cancellation while waiting on internal mutexes is not
// preemptible; it takes effect once the lock is acquired and at scan
// boundaries.
func (d *Database) ExecWithOptions(ctx context.Context, sql string, opts ExecOptions) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	write := !hasPrefixFold(strings.TrimSpace(sql), "SELECT")
	if write {
		d.commitMu.Lock()
		defer d.commitMu.Unlock()
	}
	d.gate.RLock()
	defer d.gate.RUnlock()
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	result, err := d.exec(ctx, sql, opts)
	if write && err == nil {
		d.revision.Add(1)
	}
	return result, err
}

func (d *Database) exec(ctx context.Context, sql string, opts ExecOptions) (Result, error) {
	s := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(sql), ";"))
	// Prefix dispatch without upper-casing the whole statement: VALUES
	// payloads can be large and only the leading keyword matters here.
	switch {
	case hasPrefixFold(s, "CREATE TABLE"):
		return d.create(s)
	case hasPrefixFold(s, "CREATE INDEX"):
		return d.createIndex(ctx, s)
	case hasPrefixFold(s, "ALTER TABLE"):
		return d.alterTable(ctx, s)
	case hasPrefixFold(s, "INSERT INTO"):
		return d.insert(ctx, s)
	case hasPrefixFold(s, "SELECT"):
		return d.selectRows(ctx, s, opts.MaxRows)
	case hasPrefixFold(s, "UPDATE"):
		return d.update(ctx, s)
	case hasPrefixFold(s, "DELETE FROM"):
		return d.delete(ctx, s)
	default:
		return Result{}, fmt.Errorf("unsupported SQL")
	}
}

func (d *Database) table(name string) (*Table, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	t := d.Tables[strings.ToLower(name)]
	if t == nil {
		return nil, fmt.Errorf("table not found")
	}
	return t, nil
}

// RowCount returns the total live rows across all tables. It exists to
// bound per-connection memory: a session transaction clones the entire
// database at BEGIN, so servers may reject BEGIN when RowCount exceeds a
// configured cap.
func (d *Database) RowCount() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	n := 0
	for _, t := range d.Tables {
		t.mu.RLock()
		n += len(t.Rows)
		t.mu.RUnlock()
	}
	return n
}
