package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Aggregate kinds used by grouped queries. COUNT(*) counts rows; the
// column forms count non-NULL values, matching aggregateResult.
const (
	aggCount uint8 = iota
	aggSum
	aggMin
	aggMax
	aggAvg
)

// groupItem is one parsed select-list entry for a GROUP BY query: either
// a plain grouping column or an aggregate expression.
type groupItem struct {
	name   string // output/eval column name (lowercased source text)
	isAgg  bool
	kind   uint8
	arg    string // aggregate argument ("*" for COUNT(*)); the grouping column for plain items
	argIdx int    // resolved argument index into t.Columns, -1 for "*"
}

// groupAccum is the running aggregate state of one group key.
type groupAccum struct {
	gvals []any     // group column values in GROUP BY order
	count []int64   // per aggregate: counted rows / non-NULL values
	total []float64 // per aggregate: SUM/AVG accumulator
	ext   []any     // per aggregate: current MIN or MAX value
}

// groupedResult evaluates SELECT ... GROUP BY. WHERE filters raw rows
// first (the same compiled conditions any scan uses), rows accumulate into
// groups in first-seen order, HAVING then filters groups by evaluating a
// synthetic row that carries each grouping column and each aggregate
// result, so the full operator set works there for free. ORDER BY resolves
// against that same name set; LIMIT/OFFSET and DISTINCT apply last.
// Caller holds t.mu (read).
func groupedResult(ctx context.Context, t *Table, selectExpr string, tail selectTail, distinct bool, maxRows int) (Result, error) {
	colIndex := make(map[string]int, len(t.Columns))
	for i, c := range t.Columns {
		colIndex[c.Name] = i
	}
	groupCol := make(map[string]bool, len(tail.groupBy))
	gIdx := make([]int, 0, len(tail.groupBy))
	for _, g := range tail.groupBy {
		i, ok := colIndex[g]
		if !ok {
			return Result{}, fmt.Errorf("column not found: %s", g)
		}
		gIdx = append(gIdx, i)
		groupCol[g] = true
	}
	// Parse the select list into plain group columns and aggregates.
	parts := splitSelectList(selectExpr)
	items := make([]groupItem, 0, len(parts))
	var aggIdxs []int
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			return Result{}, errors.New("invalid select list")
		}
		if kind, arg, ok := parseAggExpr(p); ok {
			it := groupItem{name: strings.ToLower(p), isAgg: true, kind: kind, arg: arg, argIdx: -1}
			if arg != "*" {
				idx, exists := colIndex[arg]
				if !exists {
					return Result{}, fmt.Errorf("column not found: %s", arg)
				}
				it.argIdx = idx
			}
			items = append(items, it)
			aggIdxs = append(aggIdxs, len(items)-1)
			continue
		}
		col := strings.ToLower(p)
		if !groupCol[col] {
			return Result{}, fmt.Errorf("column must appear in GROUP BY: %s", col)
		}
		items = append(items, groupItem{name: col, arg: col})
	}
	names := make([]string, len(items))
	for i, it := range items {
		names[i] = it.name
	}
	compiled, ok := compileCondition(tail.where, t.Columns)
	if !ok {
		return Result{}, errors.New("invalid WHERE expression")
	}
	// Scan matching rows into groups. Aggregation order follows the
	// table's row order, keeping results deterministic.
	groups := make(map[string]*groupAccum)
	var order []*groupAccum
	accumulate := func(gvals []any, val func(idx int) any) error {
		key := rowProjectionKey(gvals)
		g := groups[key]
		if g == nil {
			g = &groupAccum{
				gvals: gvals,
				count: make([]int64, len(aggIdxs)),
				total: make([]float64, len(aggIdxs)),
				ext:   make([]any, len(aggIdxs)),
			}
			groups[key] = g
			order = append(order, g)
		}
		for ai, itemIdx := range aggIdxs {
			it := items[itemIdx]
			var v any
			if it.argIdx >= 0 {
				v = val(it.argIdx)
			}
			switch it.kind {
			case aggCount:
				if it.argIdx < 0 || v != nil {
					g.count[ai]++
				}
			case aggSum, aggAvg:
				if v == nil {
					continue
				}
				n, ok := numeric(v)
				if !ok {
					return fmt.Errorf("%s requires numeric values", it.name)
				}
				g.count[ai]++
				g.total[ai] += n
			case aggMin:
				if v != nil && (g.ext[ai] == nil || valueLess(v, g.ext[ai])) {
					g.ext[ai] = v
				}
			case aggMax:
				if v != nil && (g.ext[ai] == nil || valueLess(g.ext[ai], v)) {
					g.ext[ai] = v
				}
			}
		}
		return nil
	}
	matchAll := tail.where == ""
	tick := &rowTicker{}
	if len(t.fast) > 0 {
		for _, row := range t.fast {
			if err := tick.tick(ctx); err != nil {
				return Result{}, err
			}
			if row.key == "" {
				continue
			}
			if !matchAll && !compiled.matchesFast(row.values, t.columns) {
				continue
			}
			gvals := make([]any, len(gIdx))
			for i, ci := range gIdx {
				gvals[i] = row.values[ci]
			}
			if err := accumulate(gvals, func(idx int) any { return row.values[idx] }); err != nil {
				return Result{}, err
			}
		}
	} else {
		for _, key := range t.Order {
			if err := tick.tick(ctx); err != nil {
				return Result{}, err
			}
			row, exists := t.Rows[key]
			if !exists {
				continue
			}
			if !matchAll && !compiled.matchesMap(row) {
				continue
			}
			gvals := make([]any, len(gIdx))
			for i, g := range tail.groupBy {
				gvals[i] = row[g]
			}
			if err := accumulate(gvals, func(idx int) any { return row[t.Columns[idx].Name] }); err != nil {
				return Result{}, err
			}
		}
	}
	// HAVING evaluates each group's synthetic row: grouping columns plus
	// every aggregate result, so compileCondition validates and evaluates
	// it exactly like a WHERE clause.
	synthCols := make([]Column, 0, len(tail.groupBy)+len(aggIdxs))
	for _, g := range tail.groupBy {
		synthCols = append(synthCols, Column{Name: g, Type: t.Columns[colIndex[g]].Type})
	}
	for _, itemIdx := range aggIdxs {
		it := items[itemIdx]
		st := Float
		switch it.kind {
		case aggCount:
			st = Int
		case aggMin, aggMax:
			st = t.Columns[it.argIdx].Type
		}
		synthCols = append(synthCols, Column{Name: it.name, Type: st})
	}
	var having compiledCondition
	if tail.having != "" {
		having, ok = compileCondition(tail.having, synthCols)
		if !ok {
			return Result{}, errors.New("invalid HAVING expression")
		}
	}
	type outRow struct {
		eval map[string]any
		line []any
		seq  int
	}
	out := make([]outRow, 0, len(order))
	for seq, g := range order {
		eval := make(map[string]any, len(synthCols))
		for i, gc := range tail.groupBy {
			eval[gc] = g.gvals[i]
		}
		for ai, itemIdx := range aggIdxs {
			it := items[itemIdx]
			var v any
			switch it.kind {
			case aggCount:
				// int matches the historical COUNT(*) result type.
				v = int(g.count[ai])
			case aggSum:
				v = g.total[ai]
			case aggAvg:
				if g.count[ai] > 0 {
					v = g.total[ai] / float64(g.count[ai])
				}
			case aggMin, aggMax:
				v = g.ext[ai]
			}
			eval[it.name] = v
		}
		if tail.having != "" && !having.matchesMap(eval) {
			continue
		}
		line := make([]any, len(items))
		for i, it := range items {
			if it.isAgg {
				line[i] = eval[it.name]
			} else {
				line[i] = eval[it.arg]
			}
		}
		out = append(out, outRow{eval: eval, line: line, seq: seq})
	}
	if tail.orderColumn != "" {
		found := false
		for _, c := range synthCols {
			if c.Name == tail.orderColumn {
				found = true
				break
			}
		}
		if !found {
			return Result{}, fmt.Errorf("ORDER BY column not found: %s", tail.orderColumn)
		}
		sort.Slice(out, func(i, j int) bool {
			a, b := out[i].eval[tail.orderColumn], out[j].eval[tail.orderColumn]
			if valueLess(a, b) {
				return !tail.descending
			}
			if valueLess(b, a) {
				return tail.descending
			}
			return out[i].seq < out[j].seq
		})
	}
	rows := make([][]any, 0, len(out))
	var seen map[string]struct{}
	for _, e := range out {
		if distinct {
			k := rowProjectionKey(e.line)
			if _, dup := seen[k]; dup {
				continue
			}
			if seen == nil {
				seen = make(map[string]struct{})
			}
			seen[k] = struct{}{}
		}
		rows = append(rows, e.line)
	}
	if tail.offset > 0 {
		if tail.offset >= len(rows) {
			rows = rows[:0]
		} else {
			rows = rows[tail.offset:]
		}
	}
	if tail.limit >= 0 && len(rows) > tail.limit {
		rows = rows[:tail.limit]
	}
	r := Result{Columns: names, Rows: rows}
	return r, capResultRows(r, maxRows)
}

// parseAggExpr recognizes `FN(arg)` for the supported aggregates and
// returns its kind and normalized argument. Anything else reports
// ok=false so callers can treat the item as a plain column.
func parseAggExpr(item string) (kind uint8, arg string, ok bool) {
	open := strings.IndexByte(item, '(')
	if open < 0 || !strings.HasSuffix(strings.TrimSpace(item), ")") {
		return 0, "", false
	}
	switch strings.ToUpper(strings.TrimSpace(item[:open])) {
	case "COUNT":
		kind = aggCount
	case "SUM":
		kind = aggSum
	case "MIN":
		kind = aggMin
	case "MAX":
		kind = aggMax
	case "AVG":
		kind = aggAvg
	default:
		return 0, "", false
	}
	arg = strings.ToLower(strings.TrimSpace(item[open+1 : len(item)-1]))
	if arg == "" || (arg == "*" && kind != aggCount) {
		return 0, "", false
	}
	return kind, arg, true
}

// splitSelectList divides a select list on top-level commas, tracking
// parentheses and quoted literals so aggregate arguments stay intact.
func splitSelectList(s string) []string {
	var out []string
	start, depth := 0, 0
	quote := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\'' {
			if ni, skip := skipQuoted(s, i, quote); skip {
				i = ni
				continue
			}
			quote = !quote
			continue
		}
		if quote {
			continue
		}
		switch c {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, strings.TrimSpace(s[start:i]))
				start = i + 1
			}
		}
	}
	return append(out, strings.TrimSpace(s[start:]))
}
