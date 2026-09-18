package engine

import (
	"strings"
	"unicode"
)

// This file holds allocation-free ASCII case-folding helpers for SQL keyword
// matching. SQL keywords are ASCII, so folding only 'a'-'z' is sufficient and
// avoids the full-string strings.ToUpper copies on the hot path. Table and
// column names keep their exact existing case handling (strings.ToLower on
// the small name token only).

// hasPrefixFold reports whether s starts with prefix, comparing ASCII
// letters case-insensitively without allocating.
func hasPrefixFold(s, prefix string) bool {
	if len(s) < len(prefix) {
		return false
	}
	for i := 0; i < len(prefix); i++ {
		c := s[i]
		if 'a' <= c && c <= 'z' {
			c -= 'a' - 'A'
		}
		if c != prefix[i] {
			return false
		}
	}
	return true
}

// foldEqualAt reports whether s starts with needle under ASCII folding.
// needle must already be uppercase.
func foldEqualAt(s, needle string) bool {
	if len(s) < len(needle) {
		return false
	}
	for i := 0; i < len(needle); i++ {
		c := s[i]
		if 'a' <= c && c <= 'z' {
			c -= 'a' - 'A'
		}
		if c != needle[i] {
			return false
		}
	}
	return true
}

// indexFold returns the index of the first case-insensitive occurrence of sub
// in s, or -1. sub must already be uppercase. It mirrors
// strings.Index(strings.ToUpper(s), sub) for ASCII text without allocating.
func indexFold(s, sub string) int {
	if len(sub) == 0 {
		return 0
	}
	if len(sub) > len(s) {
		return -1
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if foldEqualAt(s[i:], sub) {
			return i
		}
	}
	return -1
}

// firstField returns the first whitespace-delimited token of s, mirroring
// strings.Fields(s)[0] without splitting the whole string.
func firstField(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	if i := strings.IndexFunc(s, unicode.IsSpace); i >= 0 {
		return s[:i], true
	}
	return s, true
}
