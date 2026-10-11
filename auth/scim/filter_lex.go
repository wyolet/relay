package scim

import (
	"encoding/json"
	"strings"
)

type tokenKind int

const (
	tokEOF tokenKind = iota
	tokWord
	tokString
	tokLParen
	tokRParen
	tokLBracket
	tokRBracket
)

type token struct {
	kind tokenKind
	text string
	// pos is the token's byte offset, for error messages.
	pos int
}

// lex splits a filter or a PATCH path into tokens. A word runs until a space, a bracket, a parenthesis or a quote, so an attribute path with a schema URN ("urn:...:User:userName") or a sub-attribute (".value") stays one word.
func lex(s string) ([]token, error) {
	var out []token
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '(':
			out = append(out, token{tokLParen, "(", i})
			i++
		case c == ')':
			out = append(out, token{tokRParen, ")", i})
			i++
		case c == '[':
			out = append(out, token{tokLBracket, "[", i})
			i++
		case c == ']':
			out = append(out, token{tokRBracket, "]", i})
			i++
		case c == '"':
			end, val, err := lexString(s, i)
			if err != nil {
				return nil, err
			}
			out = append(out, token{tokString, val, i})
			i = end
		default:
			start := i
			for i < len(s) && !strings.ContainsRune(" \t\n\r()[]\"", rune(s[i])) {
				i++
			}
			out = append(out, token{tokWord, s[start:i], start})
		}
	}
	return append(out, token{tokEOF, "", len(s)}), nil
}

// lexString reads the JSON string starting at s[start], returning the offset past its closing quote and its value.
func lexString(s string, start int) (int, string, error) {
	for i := start + 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '"':
			var v string
			if err := json.Unmarshal([]byte(s[start:i+1]), &v); err != nil {
				return 0, "", badRequest(typeInvalidFilter, "malformed string at offset %d", start)
			}
			return i + 1, v, nil
		}
	}
	return 0, "", badRequest(typeInvalidFilter, "unterminated string at offset %d", start)
}
