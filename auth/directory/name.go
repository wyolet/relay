package directory

import (
	"regexp"
	"strconv"
	"strings"
)

// MaxNameLen is the DNS-1123 label limit a row name obeys.
const MaxNameLen = 63

var nameRe = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

// ValidName reports whether s is a DNS-1123 label of at most MaxNameLen characters.
func ValidName(s string) bool { return nameRe.MatchString(s) }

// Slugify derives a name from free text: lowercased, every run of other characters turned into one dash, dashes trimmed from both ends, cut to MaxNameLen. It returns "" when nothing usable is left.
func Slugify(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	prevDash := true
	for _, r := range strings.ToLower(text) {
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
	if len(s) > MaxNameLen {
		s = strings.TrimRight(s[:MaxNameLen], "-")
	}
	return s
}

// UniqueName returns base when taken reports it free, else the first free of base-2, base-3, …, each cut to fit MaxNameLen.
func UniqueName(base string, taken func(name string) bool) string {
	if !taken(base) {
		return base
	}
	for n := 2; ; n++ {
		suffix := "-" + strconv.Itoa(n)
		stem := base
		if len(stem) > MaxNameLen-len(suffix) {
			stem = strings.TrimRight(stem[:MaxNameLen-len(suffix)], "-")
		}
		if c := stem + suffix; !taken(c) {
			return c
		}
	}
}
