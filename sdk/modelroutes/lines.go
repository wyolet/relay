package modelroutes

import (
	"strings"

	"github.com/BurntSushi/toml"
)

type lineKind int

const (
	lineBlank  lineKind = iota // blank or comment only
	lineHeader                 // [table] or [[array]]
	lineKey                    // key = value, complete on its own line
	lineOther                  // part of a multi-line value, or not TOML on its own
)

// srcLine is one physical line. Each line is classified by decoding it alone with the real TOML parser, so key quoting and escapes never need a hand-written lexer; anything that does not decode alone is lineOther and blocks edits near it.
type srcLine struct {
	text  string
	eol   string // "\n", "\r\n", or "" for an unterminated last line
	kind  lineKind
	table []string // the table in force; for a header, the table it opens
	key   string   // first key segment of a key line
	value any      // decoded value of a key line
}

func splitLines(data []byte) []srcLine {
	var lines []srcLine
	rest := string(data)
	for rest != "" {
		text, after, found := strings.Cut(rest, "\n")
		l := srcLine{text: text}
		if found {
			l.eol = "\n"
			if t, ok := strings.CutSuffix(text, "\r"); ok {
				l.text, l.eol = t, "\r\n"
			}
		}
		lines = append(lines, l)
		rest = after
	}
	return lines
}

func scanLines(data []byte) []srcLine {
	lines := splitLines(data)
	var table []string
	openString := "" // closing delimiter of a multi-line string still open
	for i := range lines {
		l := &lines[i]
		l.table = table
		if openString != "" {
			l.kind = lineOther
			if strings.Count(l.text, openString)%2 == 1 {
				openString = ""
			}
			continue
		}
		trimmed := strings.TrimSpace(l.text)
		if trimmed == "" || trimmed[0] == '#' {
			l.kind = lineBlank
			continue
		}
		var doc map[string]any
		if _, err := toml.Decode(l.text, &doc); err != nil {
			l.kind = lineOther
			for _, delim := range []string{`"""`, `'''`} {
				if strings.Count(l.text, delim)%2 == 1 {
					openString = delim
					break
				}
			}
			continue
		}
		if trimmed[0] == '[' {
			table = headerPath(doc)
			l.kind, l.table = lineHeader, table
			continue
		}
		if len(doc) != 1 {
			l.kind = lineOther
			continue
		}
		for k, v := range doc {
			l.kind, l.key, l.value = lineKey, k, v
		}
	}
	return lines
}

// headerPath recovers a header's table path from the document the header line decodes to on its own.
func headerPath(doc map[string]any) []string {
	var path []string
	var cur any = doc
	for {
		m, ok := cur.(map[string]any)
		if !ok || len(m) != 1 {
			return path
		}
		for k, v := range m {
			path = append(path, k)
			cur = v
		}
	}
}

// positions maps top-level keys, routes and models to their first line number.
func positions(lines []srcLine) map[position]int {
	out := map[position]int{}
	set := func(at position, n int) {
		if _, ok := out[at]; !ok {
			out[at] = n
		}
	}
	for i, l := range lines {
		switch {
		case l.kind == lineHeader && len(l.table) == 2 && (l.table[0] == "routes" || l.table[0] == "models"):
			set(position{l.table[0], l.table[1]}, i+1)
		case l.kind == lineKey && len(l.table) == 0:
			set(position{name: l.key}, i+1)
		case l.kind == lineKey && len(l.table) == 1 && (l.table[0] == "routes" || l.table[0] == "models"):
			set(position{l.table[0], l.key}, i+1)
		}
	}
	return out
}

func joinLines(lines []srcLine) []byte {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l.text)
		b.WriteString(l.eol)
	}
	return []byte(b.String())
}
