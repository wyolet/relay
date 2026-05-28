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
