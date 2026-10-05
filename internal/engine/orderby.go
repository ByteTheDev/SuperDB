package engine

import (
	"container/heap"
	"sort"
)

// ORDER BY evaluation. Previously the sort comparator re-read
// t.Rows[key][column] for every comparison — O(n log n) map lookups plus
// interface dispatch, and fmt.Sprint allocations for TEXT columns. The
// order value is now resolved exactly once per candidate row, and when a
// LIMIT is smaller than the candidate set a bounded heap keeps only the
// K best rows instead of sorting all of them.

// orderEntry is one ORDER BY candidate with its resolved sort value and
// original collection position (the stability tiebreaker).
type orderEntry struct {
	key string
	val any
	seq int
}

// orderKeys sorts candidate keys by the ORDER BY column. With a non-
// negative cap smaller than the candidate count it runs a top-K selection
// (O(n log k)) instead of a full sort; output ordering is identical to a
// stable sort over the collected candidates: value order first, original
// collection order for ties.
func orderKeys(t *Table, keys []string, column string, descending bool, cap int) []string {
	entries := make([]orderEntry, len(keys))
	// Resolve the order value once per row. Prefer the fast array (indexed
	// lookup) and fall back to the row map for tombstones/misses.
	colIndex, hasIndex := -1, false
	if len(t.fast) > 0 {
		colIndex, hasIndex = t.columns[column]
	}
	for i, key := range keys {
		var v any
		resolved := false
		if hasIndex {
			if fi, ok := t.fastPos[key]; ok && fi >= 0 && fi < len(t.fast) && t.fast[fi].key != "" && colIndex < len(t.fast[fi].values) {
				v = t.fast[fi].values[colIndex]
				resolved = true
			}
		}
		if !resolved {
			v = t.Rows[key][column]
		}
		entries[i] = orderEntry{key: key, val: v, seq: i}
	}
	if cap >= 0 && cap < len(entries) {
		entries = topKEntries(entries, cap, descending)
	}
	sort.Slice(entries, func(i, j int) bool {
		return rankBefore(entries[i], entries[j], descending)
	})
	out := keys[:0]
	for _, e := range entries {
		out = append(out, e.key)
	}
	return out
}

// rankBefore is the total order for ORDER BY: value ordering (inverted
// for DESC), then insertion order. Because seq is unique this is a strict
// total order, so an unstable sort produces exactly the same output as
// sort.SliceStable did on the candidate slice.
func rankBefore(a, b orderEntry, descending bool) bool {
	if valueLess(a.val, b.val) {
		return !descending
	}
	if valueLess(b.val, a.val) {
		return descending
	}
	return a.seq < b.seq
}

// entryHeap is a bounded selection heap over orderEntry values. The heap
// root is the entry that ranks LAST — the next one to evict — so Less is
// rankBefore inverted.
type entryHeap struct {
	entries    []orderEntry
	descending bool
}

func (h entryHeap) Len() int { return len(h.entries) }
func (h entryHeap) Less(i, j int) bool {
	return rankBefore(h.entries[j], h.entries[i], h.descending)
}
func (h entryHeap) Swap(i, j int) { h.entries[i], h.entries[j] = h.entries[j], h.entries[i] }
func (h *entryHeap) Push(x any)   { h.entries = append(h.entries, x.(orderEntry)) }
func (h *entryHeap) Pop() any {
	old := h.entries
	n := len(old)
	e := old[n-1]
	old[n-1] = orderEntry{}
	h.entries = old[:n-1]
	return e
}

// topKEntries keeps the K best-ranked entries (unordered) using a heap of
// size K. Ties resolve by insertion sequence exactly like the full sort.
func topKEntries(entries []orderEntry, k int, descending bool) []orderEntry {
	h := &entryHeap{descending: descending}
	for _, e := range entries {
		if len(h.entries) < k {
			heap.Push(h, e)
			continue
		}
		if rankBefore(e, h.entries[0], descending) {
			heap.Pop(h)
			heap.Push(h, e)
		}
	}
	return h.entries
}
