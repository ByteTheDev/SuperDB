package engine

import "testing"

func TestBindParamsBasic(t *testing.T) {
	out, err := BindParams("SELECT * FROM users WHERE id = ? AND name = ?", []any{int64(42), "Antonio"})
	if err != nil {
		t.Fatal(err)
	}
	want := "SELECT * FROM users WHERE id = 42 AND name = 'Antonio'"
	if out != want {
		t.Fatalf("got %q want %q", out, want)
	}
}

func TestBindParamsEscapesQuotes(t *testing.T) {
	out, err := BindParams("SELECT * FROM u WHERE name = ?", []any{"o'brien"})
	if err != nil {
		t.Fatal(err)
	}
	if out != "SELECT * FROM u WHERE name = 'o''brien'" {
		t.Fatalf("got %q", out)
	}
}

func TestBindParamsIgnoresQuestionInLiteral(t *testing.T) {
	out, err := BindParams("SELECT '?' WHERE id = ?", []any{1})
	if err != nil {
		t.Fatal(err)
	}
	if out != "SELECT '?' WHERE id = 1" {
		t.Fatalf("got %q", out)
	}
}

func TestBindParamsTypes(t *testing.T) {
	out, err := BindParams("INSERT INTO t VALUES (?, ?, ?, ?)", []any{nil, true, 3.5, int(7)})
	if err != nil {
		t.Fatal(err)
	}
	if out != "INSERT INTO t VALUES (NULL, true, 3.5, 7)" {
		t.Fatalf("got %q", out)
	}
}

func TestBindParamsCountMismatch(t *testing.T) {
	if _, err := BindParams("SELECT ? + ?", []any{1}); err == nil {
		t.Fatal("expected too-few error")
	}
	if _, err := BindParams("SELECT 1", []any{1}); err == nil {
		t.Fatal("expected too-many error")
	}
	if _, err := BindParams("SELECT ?", nil); err == nil {
		t.Fatal("expected missing-params error")
	}
}
