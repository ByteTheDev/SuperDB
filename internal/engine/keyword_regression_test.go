package engine

import "testing"

func TestSelectRejectsMalformedClauseOrder(t *testing.T) {
	d := New()
	mustExec(t, d, "CREATE TABLE t (id INT PRIMARY KEY)")
	mustExec(t, d, "INSERT INTO t VALUES (1)")
	for _, query := range []string{
		"SELECT * FROM t LIMIT 1 WHERE id = 1",
		"SELECT * FROM t ORDER BY id WHERE id = 1",
		"SELECT * FROM t WHERE id = 1 LIMIT 1 ORDER BY id",
		"SELECT * FROM t WHERE",
		"SELECT * FROM t WHERE ORDER BY id",
		"SELECT * FROM t ORDER BY id DESC garbage",
	} {
		t.Run(query, func(t *testing.T) {
			defer func() {
				if value := recover(); value != nil {
					t.Errorf("malformed SQL panicked: %v", value)
				}
			}()
			if _, err := d.Exec(query); err == nil {
				t.Error("malformed SQL accepted")
			}
		})
	}
}

func TestKeywordsInsideIdentifiersAndText(t *testing.T) {
	d := New()
	mustExec(t, d, "CREATE TABLE values_table (id INT PRIMARY KEY, from_value TEXT)")
	queries := []string{
		"INSERT INTO values_table VALUES (1, 'WHERE LIMIT ORDER BY')",
		"SELECT from_value FROM values_table WHERE from_value = 'WHERE LIMIT ORDER BY'",
		"UPDATE values_table SET from_value = 'WHERE AND OR LIMIT' WHERE id = 1",
		"DELETE FROM values_table WHERE from_value = 'WHERE AND OR LIMIT'",
	}
	for _, query := range queries {
		t.Run(query, func(t *testing.T) {
			defer func() {
				if value := recover(); value != nil {
					t.Errorf("valid SQL panicked: %v", value)
				}
			}()
			r, err := d.Exec(query)
			if err != nil {
				t.Fatal(err)
			}
			if r.Affected != 1 && len(r.Rows) != 1 {
				t.Errorf("expected one row: %+v", r)
			}
		})
	}
}
