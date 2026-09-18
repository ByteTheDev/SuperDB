package engine

import (
	"errors"
	"strconv"
	"strings"
)

func splitVals(s string) []string {
	var out []string
	start := 0
	quote := false
	for i, r := range s {
		if r == '\'' {
			quote = !quote
		}
		if r == ',' && !quote {
			out = append(out, strings.TrimSpace(s[start:i]))
			start = i + 1
		}
	}
	return append(out, strings.TrimSpace(s[start:]))
}

func parseVal(raw string, typ Type) (any, error) {
	raw = strings.TrimSpace(raw)
	if strings.EqualFold(raw, "NULL") {
		return nil, nil
	}
	if typ == Text {
		return strings.Trim(raw, "'"), nil
	}
	switch typ {
	case Int:
		return strconv.ParseInt(raw, 10, 64)
	case Float:
		return strconv.ParseFloat(raw, 64)
	case Bool:
		return strconv.ParseBool(raw)
	}
	return nil, errors.New("unknown type")
}

func filter(s string) (string, string) {
	u := strings.ToUpper(s)
	i := strings.Index(u, "WHERE")
	if i < 0 {
		return "", ""
	}
	p := strings.SplitN(strings.TrimSpace(s[i+5:]), "=", 2)
	if len(p) != 2 {
		return "", ""
	}
	return strings.ToLower(strings.TrimSpace(p[0])), strings.Trim(strings.TrimSpace(p[1]), "'")
}
