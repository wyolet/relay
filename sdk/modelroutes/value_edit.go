package modelroutes

import (
	"errors"
	"strings"
)

// replaceStringValue swaps the single-line string after a key line's '=' for value, keeping the key, spacing and any trailing comment.
func replaceStringValue(text, value string) (string, error) {
	eq := strings.Index(text, "=")
	if eq < 0 {
		return "", errors.New("no value to replace")
	}
	start := eq + 1
	for start < len(text) && (text[start] == ' ' || text[start] == '\t') {
		start++
	}
	rest := text[start:]
	end := -1
	switch {
	case strings.HasPrefix(rest, `"""`), strings.HasPrefix(rest, "'''"):
	case strings.HasPrefix(rest, `"`):
		for i := 1; i < len(rest); i++ {
			if rest[i] == '\\' {
				i++
				continue
			}
			if rest[i] == '"' {
				end = i + 1
				break
			}
		}
	case strings.HasPrefix(rest, "'"):
		if i := strings.IndexByte(rest[1:], '\''); i >= 0 {
			end = i + 2
		}
	}
	if end < 0 {
		return "", errors.New("value is not a single-line string; edit by hand")
	}
	return text[:start] + quote(value) + rest[end:], nil
}

// replaceInlineString swaps the string value of key inside the { ... } on a key line, keeping every other byte of the line.
func replaceInlineString(text, key, value string) (string, error) {
	depth, atKey := 0, false
	for i := 0; i < len(text); i++ {
		c := text[i]
		switch {
		case c == '#':
			return "", errors.New("no inline table to edit")
		case c == '{':
			depth++
			atKey = depth == 1
		case c == '}':
			depth--
			atKey = false
		case c == ',':
			atKey = depth == 1
		case c == ' ' || c == '\t':
		case c == '"' || c == '\'':
			end := stringEnd(text, i)
			if end < 0 {
				return "", errors.New("unterminated string; edit by hand")
			}
			if atKey && depth == 1 && text[i+1:end-1] == key {
				return replaceAt(text, i, value)
			}
			atKey, i = false, end-1
		default:
			j := i
			for j < len(text) && isBareKeyChar(text[j]) {
				j++
			}
			if atKey && depth == 1 && text[i:j] == key {
				return replaceAt(text, i, value)
			}
			atKey, i = false, max(i, j-1)
		}
	}
	return "", errors.New("no " + key + " in an inline table; edit by hand")
}

// replaceAt replaces the string value of the key starting at offset at.
func replaceAt(text string, at int, value string) (string, error) {
	rest, err := replaceStringValue(text[at:], value)
	if err != nil {
		return "", err
	}
	return text[:at] + rest, nil
}

// stringEnd is the offset just past the single-line string opening at i, or -1 when it does not close on the line.
func stringEnd(text string, i int) int {
	q := text[i]
	for j := i + 1; j < len(text); j++ {
		switch {
		case q == '"' && text[j] == '\\':
			j++
		case text[j] == q:
			return j + 1
		}
	}
	return -1
}

func isBareKeyChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}
