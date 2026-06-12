// Package slug derives DNS-1123-compatible identifiers from free-text
// display names, with deterministic collision suffixes.
package slug

import (
	"fmt"
	"regexp"
	"strings"
)

const MaxLen = 63

var validRe = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

// Valid reports whether s is a legal slug (DNS-1123 label, ≤63 chars).
func Valid(s string) bool { return validRe.MatchString(s) }

// From slugifies a free-text display name. Lowercases, replaces runs of
// non-[a-z0-9] with a single dash, trims leading/trailing dashes, truncates
// to MaxLen. Returns "" when the input has no usable characters; callers
// should treat empty as an error.
func From(displayName string) string {
	var b strings.Builder
	b.Grow(len(displayName))
	prevDash := true // suppress leading dashes
	for _, r := range strings.ToLower(displayName) {
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
	s := strings.TrimRight(b.String(), "-")
	if len(s) > MaxLen {
		s = strings.TrimRight(s[:MaxLen], "-")
	}
	return s
}

// FromPrefix normalizes the literal prefix of a wildcard pattern. Unlike
// From it keeps the boundary dash when the raw prefix ends mid-separator
// ("claude-fable-5[" → "claude-fable-5-"), so prefix matching against
// From-normalized keys stays boundary-accurate ("claude-fable-5[*]" must
// not match "claude-fable-50-foo"). No MaxLen truncation — patterns are
// validated, not minted.
func FromPrefix(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	prevDash := true // suppress leading dashes
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

// FromSuffix normalizes the literal suffix of a wildcard pattern: the
// mirror of FromPrefix — keeps a leading boundary dash (":acme" → "-acme"),
// trims trailing separators ("]" → "").
func FromSuffix(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 1)
	prevDash := false // a leading separator run becomes one boundary dash
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

// WithSuffix appends "-N" to base, ensuring the result fits in MaxLen by
// trimming base. N ≥ 2 (callers iterate from 2).
func WithSuffix(base string, n int) string {
	suffix := fmt.Sprintf("-%d", n)
	maxBase := MaxLen - len(suffix)
	if len(base) > maxBase {
		base = strings.TrimRight(base[:maxBase], "-")
	}
	return base + suffix
}

// Unique returns base if exists(base) is false; otherwise base-2, base-3, …
// until exists returns false. Callers own the uniqueness scope (per-kind index).
func Unique(base string, exists func(candidate string) bool) string {
	if !exists(base) {
		return base
	}
	for n := 2; ; n++ {
		c := WithSuffix(base, n)
		if !exists(c) {
			return c
		}
	}
}
