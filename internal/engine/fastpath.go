package engine

const fastPathThreshold = 256

type fastRow struct {
	key    string
	values []any
}

func (t *Table) rebuildFastPathLocked() {
	if len(t.Order) < fastPathThreshold {
		t.fast = nil
		t.columns = nil
		t.fastPos = nil
		return
	}
	t.columns = make(map[string]int, len(t.Columns))
	for i, column := range t.Columns {
		t.columns[column.Name] = i
	}
	t.fast = make([]fastRow, 0, len(t.Order))
	t.fastPos = make(map[string]int, len(t.Order))
	for _, key := range t.Order {
		row := t.Rows[key]
		values := make([]any, len(t.Columns))
		for i, column := range t.Columns {
			values[i] = row[column.Name]
		}
		t.fastPos[key] = len(t.fast)
		t.fast = append(t.fast, fastRow{key: key, values: values})
	}
}

func (t *Table) appendFastRowLocked(key string, row map[string]any) {
	if len(t.Order) < fastPathThreshold {
		return
	}
	if len(t.fast) == 0 || len(t.fast)+1 != len(t.Order) {
		t.rebuildFastPathLocked()
		return
	}
	values := make([]any, len(t.Columns))
	for i, column := range t.Columns {
		values[i] = row[column.Name]
	}
	t.fast = append(t.fast, fastRow{key: key, values: values})
	t.fastPos[key] = len(t.fast) - 1
}

func (t *Table) removeFastRowLocked(key string) {
	if len(t.fast) == 0 {
		return
	}
	if len(t.Order) < fastPathThreshold {
		t.fast = nil
		t.columns = nil
		t.fastPos = nil
		return
	}
	if index, ok := t.fastPos[key]; ok {
		t.fast[index].key = ""
		t.fast[index].values = nil
		delete(t.fastPos, key)
	}
}

func (t *Table) updateFastRowLocked(key, column string, value any) {
	if len(t.fast) == 0 {
		return
	}
	index, ok := t.columns[column]
	if !ok {
		return
	}
	if rowIndex, ok := t.fastPos[key]; ok {
		t.fast[rowIndex].values[index] = value
	}
}
