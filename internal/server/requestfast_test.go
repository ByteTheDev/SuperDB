package server

import (
	"encoding/json"
	"reflect"
	"testing"
)

func stdRequest(t *testing.T, payload string) (request, error) {
	t.Helper()
	var q request
	err := json.Unmarshal([]byte(payload), &q)
	return q, err
}

func TestParseRequestFastCommonShapes(t *testing.T) {
	payloads := []string{
		`{"sql":"SELECT 1"}`,
		`{"sqls":["SELECT 1","SELECT 2"]}`,
		`{"sqls":[],"sql":"SELECT 1"}`,
		`{"sqls":["a"],"atomic":true}`,
		`{"atomic":false,"sqls":["a"]}`,
		`{ "sql" : "SELECT * FROM t WHERE name = 'a\"b\\c'" }`,
		"{\"sql\":\"line1\\nline2\\t\\u00e9\\uD83D\\uDE00\"}",
		`{"sql":"x","unknown":{"nested":[1,2.5,-3e+4,true,null,"s"]}}`,
		`{"sql":"x","n":01}`,
		`{}`,
		`{"sql":"SELECT 1"}   `,
		"\t\r\n {\"sqls\":[\"a\",\"b\"]} \r\n",
		`{"sql":"dup","sql":"winner"}`,
		`{"SQL":"ignored","sql":"used"}`,
		`{"sqls":["a"],"atomic":null}`,
		`{"sql":"caf\u00e9 \u4e2d\u6587"}`,
		`{"sql":"A\uD800B"}`,
		`{"sql":"\uDC00 lone low"}`,
	}
	for _, payload := range payloads[:8] {
		q, ok := parseRequestFast([]byte(payload))
		if !ok {
			t.Fatalf("fast path declined common shape %q", payload)
		}
		want, err := stdRequest(t, payload)
		if err != nil {
			t.Fatalf("std failed on %q: %v", payload, err)
		}
		if !reflect.DeepEqual(q, want) {
			t.Fatalf("mismatch on %q: got %+v want %+v", payload, q, want)
		}
	}
	// The rest must at least agree with std when accepted.
	for _, payload := range payloads[8:] {
		q, ok := parseRequestFast([]byte(payload))
		want, err := stdRequest(t, payload)
		if ok {
			if err != nil || !reflect.DeepEqual(q, want) {
				t.Fatalf("accepted %q with wrong result: got %+v,%v want %+v,%v", payload, q, ok, want, err)
			}
		}
	}
}

func TestParseRequestFastDeclinesMalformed(t *testing.T) {
	malformed := []string{
		``,
		`{`,
		`{"sql":}`,
		`{"sql":'single'}`,
		`{"sql":"unterminated}`,
		`{"sql":"bad\escape"}`,
		`{"sql":"bad\u12"}`,
		`{"sql":"a","sqls":"not-array"}`,
		`{"sqls":[1,2]}`,
		`{"sqls":["a",1]}`,
		`{"atomic":1,"sql":"x"}`,
		`{"atomic":"true","sql":"x"}`,
		`{"sql":123}`,
		`{"sql":null}`,
		`["sql"]`,
		`"sql"`,
		`123`,
		`{"sql":"x"}trailing`,
		`{"sql":"x",}`,
		`{"x":01,"sql":"y"}`,
		`{"x":1.,"sql":"y"}`,
		`{"x":+1,"sql":"y"}`,
		"{\"sql\":\"raw\x01control\"}",
		`{"sql":"ok"," Mum ": tru}`,
	}
	for _, payload := range malformed {
		if q, ok := parseRequestFast([]byte(payload)); ok {
			if _, err := stdRequest(t, payload); err == nil {
				t.Fatalf("fast accepted %q with result %+v but std also accepts: check semantics", payload, q)
			} else {
				t.Fatalf("fast accepted malformed %q (std errors: %v)", payload, err)
			}
		}
	}
}

func TestParseRequestFastStringEscapes(t *testing.T) {
	// Every escape shape must decode exactly like encoding/json.
	values := []string{
		`a"b\c/d`, "e\bf\fg\nh\ri\tj", "é中🎉",
		"quote\"back\\slash/solidus", "\u0000\u001f~",
		"\U0001F600", "tab\there",
	}
	for _, v := range values {
		encoded, err := json.Marshal(map[string]string{"sql": v})
		if err != nil {
			t.Fatal(err)
		}
		q, ok := parseRequestFast(encoded)
		if !ok {
			t.Fatalf("declined encoded %q", v)
		}
		if q.SQL != v {
			t.Fatalf("escape mismatch: got %q want %q", q.SQL, v)
		}
	}
}
