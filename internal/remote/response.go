package remote

import (
	"strconv"

	"superdb/internal/engine"
	"superdb/internal/wire"
)

// Fast encoder for wire.Response. Row-bearing query responses used to go
// through json.Marshal, paying reflection over every cell of every row on
// the hot path. This encoder produces output byte-identical to
// encoding/json — same field order, same omitempty behavior — without it.

// mustResponseJSON encodes a Response and falls back to the standard
// library on the (unreachable for engine values) encode error path.
func mustResponseJSON(r wire.Response) []byte {
	buf, err := appendResponseJSON(make([]byte, 0, 128), r)
	if err != nil {
		return wire.MustJSON(r)
	}
	return buf
}

// appendResponseJSON appends the exact JSON encoding of r to buf:
// field order and omitempty rules match the struct tags in wire.Response.
func appendResponseJSON(buf []byte, r wire.Response) ([]byte, error) {
	out := append(buf, '{')
	out = append(out, `"request_id":`...)
	out = strconv.AppendUint(out, r.RequestID, 10)
	out = append(out, `,"status":`...)
	out = engine.AppendJSONString(out, r.Status)
	if r.ErrorCode != "" {
		out = append(out, `,"error_code":`...)
		out = engine.AppendJSONString(out, r.ErrorCode)
	}
	if r.ErrorMessage != "" {
		out = append(out, `,"error_message":`...)
		out = engine.AppendJSONString(out, r.ErrorMessage)
	}
	if len(r.Columns) > 0 {
		out = append(out, `,"columns":[`...)
		for i, column := range r.Columns {
			if i > 0 {
				out = append(out, ',')
			}
			out = engine.AppendJSONString(out, column)
		}
		out = append(out, ']')
	}
	if len(r.Rows) > 0 {
		out = append(out, `,"rows":[`...)
		for i, row := range r.Rows {
			if i > 0 {
				out = append(out, ',')
			}
			if row == nil {
				out = append(out, "null"...)
				continue
			}
			out = append(out, '[')
			for j, v := range row {
				if j > 0 {
					out = append(out, ',')
				}
				var err error
				out, err = engine.AppendJSONValue(out, v)
				if err != nil {
					return nil, err
				}
			}
			out = append(out, ']')
		}
		out = append(out, ']')
	}
	if r.AffectedRows != 0 {
		out = append(out, `,"affected_rows":`...)
		out = strconv.AppendInt(out, int64(r.AffectedRows), 10)
	}
	if r.Message != "" {
		out = append(out, `,"message":`...)
		out = engine.AppendJSONString(out, r.Message)
	}
	if r.LeaderAddr != "" {
		out = append(out, `,"leader_addr":`...)
		out = engine.AppendJSONString(out, r.LeaderAddr)
	}
	if r.Retryable {
		out = append(out, `,"retryable":true`...)
	}
	return append(out, '}'), nil
}
