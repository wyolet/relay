package modelroutes

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/wyolet/relay/sdk/catalog"
)

// AddRoute writes r as a [routes.<name>] table after the last routes section, or before [models] when the file has no routes yet. r is checked with the rules Resolve applies; ic nil skips the one check that needs a catalog, that r.Host is a catalog host.
func AddRoute(path string, ic *catalog.IndexedCatalog, r Route) error {
	if r.Name == "" {
		return errors.New("modelroutes: empty route name")
	}
	return editFile(path, func(f *File, lines []srcLine) ([]srcLine, error) {
		if slices.ContainsFunc(f.Routes, func(x Route) bool { return x.Name == r.Name }) {
			return nil, f.wrap(fmt.Errorf("route %q already exists", r.Name))
		}
		var p problems
		for _, err := range routeProblems(r, ic) {
			p.add(f, position{}, "route %q: %v", r.Name, err)
		}
		if err := p.err(); err != nil {
			return nil, err
		}
		b, err := findRoutes(lines)
		if err != nil {
			return nil, err
		}
		f.Routes = append(f.Routes, r)
		eol, spaced := lineEnding(lines), tablesSpaced(lines)
		table := routeTable(r, eol)
		blank := srcLine{eol: eol}
		at := len(lines)
		switch {
		case b.lastSection >= 0:
			at = sectionEnd(lines, b.lastSection) + 1
		case slices.ContainsFunc(lines, isModelsHeader):
			at = commentBlockStart(lines, slices.IndexFunc(lines, isModelsHeader))
			if spaced {
				table = append(table, blank)
			}
		}
		if at > 0 {
			lines[at-1].eol = eol
			if spaced && !isEmpty(lines[at-1]) {
				table = append([]srcLine{blank}, table...)
			}
		}
		return slices.Insert(lines, at, table...), nil
	})
}

// RemoveRoute deletes route name: an inline entry's line, or a [routes.<name>] table from its header to its last line, together with a comment block flush on that line or header, which documents this route alone. A comment separated by a blank line stays. Removing a route that [models] entries use is refused.
func RemoveRoute(path, name string) error {
	return editFile(path, func(f *File, lines []srcLine) ([]srcLine, error) {
		i := slices.IndexFunc(f.Routes, func(r Route) bool { return r.Name == name })
		if i < 0 {
			return nil, f.wrap(fmt.Errorf("route %q does not exist", name))
		}
		var users []string
		for _, m := range f.Models {
			if m.Route == name {
				users = append(users, quote(m.Key))
			}
		}
		if len(users) > 0 {
			return nil, f.wrap(fmt.Errorf("route %q is used by %s; remove or move those models first", name, strings.Join(users, ", ")))
		}
		b, err := findRoutes(lines)
		if err != nil {
			return nil, err
		}
		f.Routes = slices.Delete(f.Routes, i, i+1)
		if line, ok := b.inline[name]; ok {
			return slices.Delete(lines, commentBlockStart(lines, line), line+1), nil
		}
		t, ok := b.tables[name]
		if !ok {
			return nil, f.wrap(fmt.Errorf("route %q: no lines to remove; edit by hand", name))
		}
		at := commentBlockStart(lines, t.header)
		lines = slices.Delete(lines, at, t.end+1)
		if at > 0 && isEmpty(lines[at-1]) && (at == len(lines) || isEmpty(lines[at])) {
			lines = slices.Delete(lines, at-1, at)
		}
		return lines, nil
	})
}

// SetRouteDisplayName sets, replaces or, with displayName "", removes the displayName line of a [routes.<name>] table. Routes written as inline entries under [routes] are refused: editing inside an inline table is not a line edit.
func SetRouteDisplayName(path, name, displayName string) error {
	return editFile(path, func(f *File, lines []srcLine) ([]srcLine, error) {
		i := slices.IndexFunc(f.Routes, func(r Route) bool { return r.Name == name })
		if i < 0 {
			return nil, f.wrap(fmt.Errorf("route %q does not exist", name))
		}
		b, err := findRoutes(lines)
		if err != nil {
			return nil, err
		}
		if _, ok := b.inline[name]; ok {
			return nil, f.wrap(fmt.Errorf("route %q is an inline entry under [routes]; move it to a [routes.%s] table or edit by hand", name, tomlKey(name)))
		}
		t, ok := b.tables[name]
		if !ok {
			return nil, f.wrap(fmt.Errorf("route %q: no [routes.%s] table; edit by hand", name, tomlKey(name)))
		}
		f.Routes[i].DisplayName = displayName
		line, has := t.fields["displayName"]
		switch {
		case has && displayName == "":
			return slices.Delete(lines, line, line+1), nil
		case has:
			text, err := replaceStringValue(lines[line].text, displayName)
			if err != nil {
				return nil, f.wrap(fmt.Errorf("line %d: %w", line+1, err))
			}
			lines[line].text = text
			return lines, nil
		case displayName == "":
			return lines, nil
		}
		last := &lines[t.lastField]
		indent := ""
		if t.lastField != t.header {
			indent = last.text[:len(last.text)-len(strings.TrimLeft(last.text, " \t"))]
		}
		eol := lineEnding(lines)
		last.eol = eol
		return slices.Insert(lines, t.lastField+1, srcLine{text: indent + "displayName = " + quote(displayName), eol: eol}), nil
	})
}

// routeTable renders r as a [routes.<name>] table, keys in a fixed order so equal routes render identically.
func routeTable(r Route, eol string) []srcLine {
	out := []srcLine{{text: "[routes." + tomlKey(r.Name) + "]", eol: eol}}
	for _, kv := range [][2]string{{"host", r.Host}, {"url", r.URL}, {"auth", r.Auth}, {"adapter", r.Adapter}} {
		if kv[1] != "" {
			out = append(out, srcLine{text: kv[0] + " = " + quote(kv[1]), eol: eol})
		}
	}
	if r.Passthrough {
		out = append(out, srcLine{text: "passthrough = true", eol: eol})
	}
	for _, kv := range [][2]string{{"account", r.Account}, {"displayName", r.DisplayName}} {
		if kv[1] != "" {
			out = append(out, srcLine{text: kv[0] + " = " + quote(kv[1]), eol: eol})
		}
	}
	return out
}

// tomlKey writes name bare when TOML allows it, else quoted.
func tomlKey(name string) string {
	bare := name != ""
	for _, r := range name {
		bare = bare && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-')
	}
	if !bare {
		return quote(name)
	}
	return name
}

// routesBlock is where routes sit; lastSection is the header of the last [routes] or [routes.*] section, -1 when there is none.
type routesBlock struct {
	lastSection int
	inline      map[string]int // [routes] entry line by route name
	tables      map[string]routeSection
}

// routeSection is one [routes.<name>] table: header through end, its key lines, and the last of them (the header when it has none).
type routeSection struct {
	header, end, lastField int
	fields                 map[string]int
}

// findRoutes refuses any routes layout where a line edit could split or misplace a route: inline `routes = {...}`, [[routes]] arrays, tables nested deeper than [routes.<name>], entries spanning several lines, and one route's keys spread over dotted lines.
func findRoutes(lines []srcLine) (routesBlock, error) {
	b := routesBlock{lastSection: -1, inline: map[string]int{}, tables: map[string]routeSection{}}
	unsafe := func(i int, why string) (routesBlock, error) {
		return routesBlock{}, fmt.Errorf("modelroutes: line %d: %s; edit routes by hand", i+1, why)
	}
	inRoutes, table := false, ""
	for i, l := range lines {
		if l.kind == lineHeader {
			inRoutes, table = len(l.table) > 0 && l.table[0] == "routes", ""
			switch {
			case !inRoutes:
				continue
			case l.array:
				return unsafe(i, "routes written as a [[routes]] array")
			case len(l.table) > 2:
				return unsafe(i, "table nested inside a route")
			case len(l.table) == 2:
				table = l.table[1]
				b.tables[table] = routeSection{header: i, end: sectionEnd(lines, i), lastField: i, fields: map[string]int{}}
			}
			b.lastSection = i
			continue
		}
		if l.kind == lineKey && len(l.table) == 0 && l.key == "routes" {
			return unsafe(i, "routes written inline, not as tables")
		}
		if !inRoutes || l.kind == lineBlank {
			continue
		}
		if l.kind != lineKey {
			return unsafe(i, "route value spans several lines")
		}
		if table == "" {
			if _, ok := l.value.(map[string]any); !ok {
				return unsafe(i, "[routes] entry is not an inline table")
			}
			if _, dup := b.inline[l.key]; dup {
				return unsafe(i, fmt.Sprintf("route %q is split across lines", l.key))
			}
			b.inline[l.key] = i
			continue
		}
		t := b.tables[table]
		if _, dup := t.fields[l.key]; dup {
			return unsafe(i, fmt.Sprintf("key %q of route %q is split across lines", l.key, table))
		}
		t.fields[l.key], t.lastField = i, i
		b.tables[table] = t
	}
	return b, nil
}

// sectionEnd is the last line of the table opened at header. Trailing blank lines, and a comment block sitting directly on the next header, belong to what follows.
func sectionEnd(lines []srcLine, header int) int {
	next := len(lines)
	for i := header + 1; i < len(lines); i++ {
		if lines[i].kind == lineHeader {
			next = commentBlockStart(lines, i)
			break
		}
	}
	end := next - 1
	for end > header && isEmpty(lines[end]) {
		end--
	}
	return end
}

// commentBlockStart is the first line of the comment block directly above line i, or i when there is none.
func commentBlockStart(lines []srcLine, i int) int {
	for i > 0 && lines[i-1].kind == lineBlank && !isEmpty(lines[i-1]) {
		i--
	}
	return i
}

// tablesSpaced reports whether the file separates tables with a blank line; true unless every table header already present sits flush against the line above it.
func tablesSpaced(lines []srcLine) bool {
	spaced, flush := 0, 0
	for i, l := range lines {
		if l.kind != lineHeader {
			continue
		}
		if start := commentBlockStart(lines, i); start > 0 {
			if isEmpty(lines[start-1]) {
				spaced++
			} else {
				flush++
			}
		}
	}
	return spaced > 0 || flush == 0
}

func isEmpty(l srcLine) bool { return strings.TrimSpace(l.text) == "" }

func isModelsHeader(l srcLine) bool {
	return l.kind == lineHeader && !l.array && len(l.table) == 1 && l.table[0] == "models"
}
