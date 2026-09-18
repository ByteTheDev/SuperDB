package server

import "unicode/utf8"

// Fast, allocation-light parser for the narrow request shapes clients send:
// {"sql":"..."}, {"sqls":[...]}, and {"sqls":[...],"atomic":bool} with
// arbitrary whitespace, key order, and unknown fields.
//
// It accepts exactly the payloads encoding/json would decode into request
// without error and returns ok=false for anything else (wrong types,
// malformed JSON, trailing data). Callers fall back to encoding/json on
// ok=false, so the standard library stays authoritative for validation and
// error messages: the fast path can only decline work, never diverge.

func parseRequestFast(payload []byte) (q request, ok bool) {
	p := fastParser{b: payload}
	p.skipSpace()
	if !p.consume('{') {
		return request{}, false
	}
	var out request
	if p.peek() == '}' {
		p.pos++
		p.skipSpace()
		if p.pos != len(p.b) {
			return request{}, false
		}
		return out, true
	}
	for {
		p.skipSpace()
		key, keyOK := p.string()
		if !keyOK {
			return request{}, false
		}
		p.skipSpace()
		if !p.consume(':') {
			return request{}, false
		}
		p.skipSpace()
		switch key {
		case "sql":
			s, sOK := p.string()
			if !sOK {
				return request{}, false
			}
			out.SQL = s
		case "sqls":
			list, lOK := p.stringArray()
			if !lOK {
				return request{}, false
			}
			out.SQLs = list
		case "atomic":
			v, vOK := p.literal()
			if !vOK {
				return request{}, false
			}
			switch v {
			case "true":
				out.Atomic = true
			case "false":
				out.Atomic = false
			case "null":
			default:
				return request{}, false
			}
		default:
			if !p.skipValue() {
				return request{}, false
			}
		}
		p.skipSpace()
		if p.consume(',') {
			continue
		}
		if p.consume('}') {
			p.skipSpace()
			if p.pos != len(p.b) {
				return request{}, false
			}
			return out, true
		}
		return request{}, false
	}
}

type fastParser struct {
	b   []byte
	pos int
}

func (p *fastParser) peek() byte {
	if p.pos < len(p.b) {
		return p.b[p.pos]
	}
	return 0
}

func (p *fastParser) consume(c byte) bool {
	if p.peek() == c {
		p.pos++
		return true
	}
	return false
}

func (p *fastParser) skipSpace() {
	for p.pos < len(p.b) {
		switch p.b[p.pos] {
		case ' ', '\t', '\n', '\r':
			p.pos++
		default:
			return
		}
	}
}

// string parses a JSON string starting at the current position (which must
// be the opening quote) and returns its decoded value.
func (p *fastParser) string() (string, bool) {
	if !p.consume('"') {
		return "", false
	}
	start := p.pos
	for p.pos < len(p.b) {
		c := p.b[p.pos]
		switch {
		case c == '"':
			s := string(p.b[start:p.pos])
			p.pos++
			return s, true
		case c == '\\':
			goto escaped
		case c < 0x20:
			return "", false
		case c < 0x80:
			p.pos++
		default:
			// Multibyte runes pass through like encoding/json; invalid
			// bytes decline to the fallback, which replaces them with
			// U+FFFD exactly like the standard library.
			_, size := utf8.DecodeRune(p.b[p.pos:])
			if size <= 1 && p.b[p.pos] >= 0x80 {
				return "", false
			}
			p.pos += size
		}
	}
	return "", false
escaped:
	out := append([]byte(nil), p.b[start:p.pos]...)
	for {
		if p.pos >= len(p.b) {
			return "", false
		}
		c := p.b[p.pos]
		switch {
		case c == '"':
			p.pos++
			return string(out), true
		case c == '\\':
			r, n, ok := p.escapeAt(p.pos)
			if !ok {
				return "", false
			}
			out = append(out, r...)
			p.pos += n
		case c < 0x20:
			return "", false
		case c < 0x80:
			out = append(out, c)
			p.pos++
		default:
			_, size := utf8.DecodeRune(p.b[p.pos:])
			if size <= 1 && p.b[p.pos] >= 0x80 {
				return "", false
			}
			out = append(out, p.b[p.pos:p.pos+size]...)
			p.pos += size
		}
	}
}

// escapeAt decodes the escape sequence starting at b[i] (the backslash) and
// returns its UTF-8 bytes and total length. It mirrors encoding/json,
// including surrogate pairs; malformed sequences fail.
func (p *fastParser) escapeAt(i int) ([]byte, int, bool) {
	if i+1 >= len(p.b) {
		return nil, 0, false
	}
	switch e := p.b[i+1]; e {
	case '"', '\\', '/':
		return []byte{e}, 2, true
	case 'b':
		return []byte{'\b'}, 2, true
	case 'f':
		return []byte{'\f'}, 2, true
	case 'n':
		return []byte{'\n'}, 2, true
	case 'r':
		return []byte{'\r'}, 2, true
	case 't':
		return []byte{'\t'}, 2, true
	case 'u':
		r, ok := decodeU(p.b, i+2)
		if !ok {
			return nil, 0, false
		}
		consumed := 6
		// Surrogate pair, exactly like encoding/json.
		if r >= 0xD800 && r <= 0xDBFF && i+12 <= len(p.b) && p.b[i+6] == '\\' && p.b[i+7] == 'u' {
			lo, ok := decodeU(p.b, i+8)
			if !ok || lo < 0xDC00 || lo > 0xDFFF {
				return nil, 0, false
			}
			r = 0x10000 + (r-0xD800)<<10 + (lo - 0xDC00)
			consumed = 12
		} else if r >= 0xD800 && r <= 0xDFFF {
			return nil, 0, false
		}
		var buf [4]byte
		size := encodeRune(buf[:], r)
		return buf[:size], consumed, true
	default:
		return nil, 0, false
	}
}

// decodeU reads \uXXXX hex digits starting at b[i] and returns the value.
func decodeU(b []byte, i int) (rune, bool) {
	if i+4 > len(b) {
		return 0, false
	}
	var r rune
	for _, c := range b[i : i+4] {
		var v byte
		switch {
		case '0' <= c && c <= '9':
			v = c - '0'
		case 'a' <= c && c <= 'f':
			v = c - 'a' + 10
		case 'A' <= c && c <= 'F':
			v = c - 'A' + 10
		default:
			return 0, false
		}
		r = r*16 + rune(v)
	}
	return r, true
}

// encodeRune encodes r as UTF-8 into buf (len >= 4) and returns its length.
func encodeRune(buf []byte, r rune) int {
	switch {
	case r < 0x80:
		buf[0] = byte(r)
		return 1
	case r < 0x800:
		buf[0] = 0xC0 | byte(r>>6)
		buf[1] = 0x80 | byte(r&0x3F)
		return 2
	case r < 0x10000:
		buf[0] = 0xE0 | byte(r>>12)
		buf[1] = 0x80 | byte((r>>6)&0x3F)
		buf[2] = 0x80 | byte(r&0x3F)
		return 3
	default:
		buf[0] = 0xF0 | byte(r>>18)
		buf[1] = 0x80 | byte((r>>12)&0x3F)
		buf[2] = 0x80 | byte((r>>6)&0x3F)
		buf[3] = 0x80 | byte(r&0x3F)
		return 4
	}
}

func (p *fastParser) stringArray() ([]string, bool) {
	if !p.consume('[') {
		return nil, false
	}
	var out []string
	p.skipSpace()
	if p.peek() == ']' {
		p.pos++
		return []string{}, true
	}
	for {
		p.skipSpace()
		s, ok := p.string()
		if !ok {
			return nil, false
		}
		p.skipSpace()
		out = append(out, s)
		if p.consume(',') {
			continue
		}
		if p.consume(']') {
			return out, true
		}
		return nil, false
	}
}

// literal reads true, false, or null starting at the current position.
func (p *fastParser) literal() (string, bool) {
	for _, lit := range []string{"true", "false", "null"} {
		if len(p.b)-p.pos >= len(lit) && string(p.b[p.pos:p.pos+len(lit)]) == lit {
			p.pos += len(lit)
			return lit, true
		}
	}
	return "", false
}

// skipValue skips one JSON value of any type, validating strictly so the
// fast path never accepts what encoding/json would reject.
func (p *fastParser) skipValue() bool {
	if p.pos >= len(p.b) {
		return false
	}
	switch p.b[p.pos] {
	case '"':
		return p.skipString()
	case '{':
		p.pos++
		p.skipSpace()
		if p.peek() == '}' {
			p.pos++
			return true
		}
		for {
			p.skipSpace()
			if !p.skipString() {
				return false
			}
			p.skipSpace()
			if !p.consume(':') {
				return false
			}
			p.skipSpace()
			if !p.skipValue() {
				return false
			}
			p.skipSpace()
			if p.consume(',') {
				continue
			}
			if p.consume('}') {
				return true
			}
			return false
		}
	case '[':
		p.pos++
		p.skipSpace()
		if p.peek() == ']' {
			p.pos++
			return true
		}
		for {
			p.skipSpace()
			if !p.skipValue() {
				return false
			}
			p.skipSpace()
			if p.consume(',') {
				continue
			}
			if p.consume(']') {
				return true
			}
			return false
		}
	case 't':
		return p.skipLiteral("true")
	case 'f':
		return p.skipLiteral("false")
	case 'n':
		return p.skipLiteral("null")
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		return p.skipNumber()
	default:
		return false
	}
}

func (p *fastParser) skipLiteral(lit string) bool {
	if len(p.b)-p.pos >= len(lit) && string(p.b[p.pos:p.pos+len(lit)]) == lit {
		p.pos += len(lit)
		return true
	}
	return false
}

// skipString validates a JSON string without decoding it.
func (p *fastParser) skipString() bool {
	if !p.consume('"') {
		return false
	}
	for {
		if p.pos >= len(p.b) {
			return false
		}
		switch c := p.b[p.pos]; {
		case c == '"':
			p.pos++
			return true
		case c == '\\':
			if p.pos+1 >= len(p.b) {
				return false
			}
			switch e := p.b[p.pos+1]; e {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
				p.pos += 2
			case 'u':
				if _, ok := decodeU(p.b, p.pos+2); !ok {
					return false
				}
				p.pos += 6
			default:
				return false
			}
		case c < 0x20:
			return false
		default:
			p.pos++
		}
	}
}

// skipNumber validates a JSON number with the exact grammar encoding/json
// enforces (no leading zeros, no trailing dot, no bare plus).
func (p *fastParser) skipNumber() bool {
	i := p.pos
	if i < len(p.b) && p.b[i] == '-' {
		i++
	}
	if i >= len(p.b) {
		return false
	}
	if p.b[i] == '0' {
		i++
	} else if '1' <= p.b[i] && p.b[i] <= '9' {
		for i < len(p.b) && '0' <= p.b[i] && p.b[i] <= '9' {
			i++
		}
	} else {
		return false
	}
	if i < len(p.b) && p.b[i] == '.' {
		i++
		if i >= len(p.b) || p.b[i] < '0' || p.b[i] > '9' {
			return false
		}
		for i < len(p.b) && '0' <= p.b[i] && p.b[i] <= '9' {
			i++
		}
	}
	if i < len(p.b) && (p.b[i] == 'e' || p.b[i] == 'E') {
		i++
		if i < len(p.b) && (p.b[i] == '+' || p.b[i] == '-') {
			i++
		}
		if i >= len(p.b) || p.b[i] < '0' || p.b[i] > '9' {
			return false
		}
		for i < len(p.b) && '0' <= p.b[i] && p.b[i] <= '9' {
			i++
		}
	}
	p.pos = i
	return true
}
