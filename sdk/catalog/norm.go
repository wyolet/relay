package catalog

import "strings"

const maxRefLen = 63

// normRef slugifies a model ref the same way pkg/slug.From does so resolution
// matches relay's catalog alias index.
func normRef(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	prevDash := true
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	out := strings.TrimRight(b.String(), "-")
	if len(out) > maxRefLen {
		out = strings.TrimRight(out[:maxRefLen], "-")
	}
	return out
}

// normPrefix normalizes a wildcard pattern's literal prefix, keeping the
// boundary dash ("claude-fable-5[" → "claude-fable-5-") so prefix matching
// against normRef keys stays boundary-accurate. Mirrors pkg/slug.FromPrefix
// (the sdk module cannot import pkg/ — rule 10). No truncation.
func normPrefix(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	prevDash := true
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return b.String()
}

// normSuffix is the mirror of normPrefix: keeps a leading boundary dash,
// trims trailing separators. Mirrors pkg/slug.FromSuffix.
func normSuffix(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 1)
	prevDash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.TrimRight(b.String(), "-")
}
