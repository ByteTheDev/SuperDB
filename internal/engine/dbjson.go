package engine

import (
	"sort"
	"strconv"
)

// Streaming encoder for the database image. Produces exactly the JSON
// shape json.Marshal(databaseImage{Tables}) emits — exported field order
// (Name, Columns, Rows, Order, Indexes; Name/Type/Primary per column) and
// sorted map keys, which also makes snapshots deterministic for a given
// dataset. The difference is cost: it writes straight from live tables
// instead of deep-copying the whole database and walking it with
// reflection.
//
// Locking: callers hold gate (write) so no statement is in flight and no
// replace() can swap Tables mid-encode, plus each table's read lock while
// that table is encoded — the same guarantees the previous copy-then-
// marshal path relied on.

func appendDatabaseJSON(buf []byte, d *Database) []byte {
	buf = append(buf, `{"Tables":`...)
	if d.Tables == nil {
		return append(buf, "null}"...)
	}
	buf = append(buf, '{')
	names := make([]string, 0, len(d.Tables))
	for name := range d.Tables {
		names = append(names, name)
	}
	sort.Strings(names)
	for i, name := range names {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = appendJSONString(buf, name)
		buf = append(buf, ':')
		t := d.Tables[name]
		t.mu.RLock()
		buf = appendTableJSON(buf, t)
		t.mu.RUnlock()
	}
	return append(buf, '}', '}')
}

func appendTableJSON(buf []byte, t *Table) []byte {
	buf = append(buf, `{"Name":`...)
	buf = appendJSONString(buf, t.Name)
	buf = append(buf, `,"Columns":[`...)
	for i, c := range t.Columns {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = append(buf, `{"Name":`...)
		buf = appendJSONString(buf, c.Name)
		buf = append(buf, `,"Type":`...)
		buf = appendJSONString(buf, string(c.Type))
		buf = append(buf, `,"Primary":`...)
		buf = strconv.AppendBool(buf, c.Primary)
		buf = append(buf, '}')
	}
	buf = append(buf, `],"Rows":`...)
	if t.Rows == nil {
		buf = append(buf, "null"...)
	} else {
		buf = append(buf, '{')
		keys := make([]string, 0, len(t.Rows))
		for key := range t.Rows {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for i, key := range keys {
			if i > 0 {
				buf = append(buf, ',')
			}
			buf = appendJSONString(buf, key)
			buf = append(buf, ':')
			buf = appendRowJSON(buf, t.Rows[key])
		}
		buf = append(buf, '}')
	}
	buf = append(buf, `,"Order":`...)
	if t.Order == nil {
		buf = append(buf, "null"...)
	} else {
		buf = append(buf, '[')
		for i, key := range t.Order {
			if i > 0 {
				buf = append(buf, ',')
			}
			buf = appendJSONString(buf, key)
		}
		buf = append(buf, ']')
	}
	buf = append(buf, `,"Indexes":`...)
	if t.Indexes == nil {
		buf = append(buf, "null"...)
	} else {
		buf = append(buf, '{')
		columns := make([]string, 0, len(t.Indexes))
		for column := range t.Indexes {
			columns = append(columns, column)
		}
		sort.Strings(columns)
		for i, column := range columns {
			if i > 0 {
				buf = append(buf, ',')
			}
			buf = appendJSONString(buf, column)
			buf = append(buf, ':')
			// Member sets serialize as {"rowkey":{}} — the reflection
			// shape map[string]struct{} produces. Restore ignores them
			// and rebuilds membership from rows; the keys are emitted so
			// the byte stream matches the previous encoder exactly.
			values := make([]string, 0, len(t.Indexes[column]))
			for value := range t.Indexes[column] {
				values = append(values, value)
			}
			sort.Strings(values)
			buf = append(buf, '{')
			for j, value := range values {
				if j > 0 {
					buf = append(buf, ',')
				}
				buf = appendJSONString(buf, value)
				buf = append(buf, ':', '{')
				members := make([]string, 0, len(t.Indexes[column][value]))
				for member := range t.Indexes[column][value] {
					members = append(members, member)
				}
				sort.Strings(members)
				for k, member := range members {
					if k > 0 {
						buf = append(buf, ',')
					}
					buf = appendJSONString(buf, member)
					buf = append(buf, ':', '{', '}')
				}
				buf = append(buf, '}')
			}
			buf = append(buf, '}')
		}
		buf = append(buf, '}')
	}
	buf = append(buf, `,"IndexNames":`...)
	if t.IndexNames == nil {
		buf = append(buf, "null}"...)
		return buf
	}
	buf = append(buf, '{')
	names := make([]string, 0, len(t.IndexNames))
	for name := range t.IndexNames {
		names = append(names, name)
	}
	sort.Strings(names)
	for i, name := range names {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = appendJSONString(buf, name)
		buf = append(buf, ':')
		buf = appendJSONString(buf, t.IndexNames[name])
	}
	return append(buf, '}', '}')
}

func appendRowJSON(buf []byte, row map[string]any) []byte {
	if row == nil {
		return append(buf, "null"...)
	}
	keys := make([]string, 0, len(row))
	for key := range row {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	buf = append(buf, '{')
	for i, key := range keys {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = appendJSONString(buf, key)
		buf = append(buf, ':')
		var err error
		buf, err = appendJSONValue(buf, row[key])
		if err != nil {
			// Row values are engine-produced scalars; only a foreign type
			// could fail, and it would have failed the reflection path too.
			// Emit null rather than corrupt the stream.
			buf = append(buf, "null"...)
		}
	}
	return append(buf, '}')
}

// snapshotEstimate guesses an encode buffer size to avoid regrowth on
// large databases. Underestimates are harmless (append grows); it only
// needs to be cheap.
func snapshotEstimate(d *Database) int {
	n := 64
	for _, t := range d.Tables {
		n += len(t.Rows) * (len(t.Columns)*16 + 32)
	}
	return n
}
