package modelroutes

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/wyolet/relay/sdk/catalog"
	"github.com/wyolet/relay/sdk/internal/atomicfile"
)

// Add validates m against the file's routes and ic the way Resolve would, then appends it as the last [models] line, creating the table at the end of the file when there is none. It writes the short form `key = "route"` when Key equals Model (or Model is empty), else `key = { model = ..., route = ... }`.
func Add(path string, ic *catalog.IndexedCatalog, m ModelRoute) error {
	if m.Model == "" {
		m.Model = m.Key
	}
	return editFile(path, func(f *File, lines []srcLine) ([]srcLine, error) {
		if err := checkAdd(f, ic, m); err != nil {
			return nil, err
		}
		t, err := findModelsTable(lines)
		if err != nil {
			return nil, err
		}
		f.Models = append(f.Models, m)
		eol := lineEnding(lines)
		if t.header < 0 {
			if n := len(lines); n > 0 {
				lines[n-1].eol = eol
				if strings.TrimSpace(lines[n-1].text) != "" {
					lines = append(lines, srcLine{eol: eol})
				}
			}
			return append(lines, srcLine{text: "[models]", eol: eol}, srcLine{text: entryText(m, nil), eol: eol}), nil
		}
		var like *srcLine
		if t.last != t.header {
			like = &lines[t.last]
		}
		if lines[t.last].eol == "" {
			lines[t.last].eol = eol
		}
		return slices.Insert(lines, t.last+1, srcLine{text: entryText(m, like), eol: eol}), nil
	})
}

func checkAdd(f *File, ic *catalog.IndexedCatalog, m ModelRoute) error {
	if m.Key == "" {
		return errors.New("modelroutes: empty model key")
	}
	if slices.ContainsFunc(f.Models, func(x ModelRoute) bool { return x.Key == m.Key }) {
		return f.wrap(fmt.Errorf("%s: already listed", describe(m)))
	}
	var r *Route
	for i := range f.Routes {
		if f.Routes[i].Name == m.Route {
			r = &f.Routes[i]
		}
	}
	if r == nil {
		return f.wrap(fmt.Errorf("%s: unknown route %q", describe(m), m.Route))
	}
	var p problems
	for _, err := range routeProblems(*r, ic) {
		p.add(f, position{"routes", r.Name}, "route %q: %v", r.Name, err)
	}
	if err := p.err(); err != nil {
		return err
	}
	if _, err := resolveEntry(ic, m, *r); err != nil {
		return f.wrap(fmt.Errorf("%s: %w", describe(m), err))
	}
	return nil
}

// Remove deletes the [models] line for key. Removing the default is refused, since the file would no longer resolve.
func Remove(path, key string) error {
	return editFile(path, func(f *File, lines []srcLine) ([]srcLine, error) {
		i := slices.IndexFunc(f.Models, func(m ModelRoute) bool { return m.Key == key })
		if i < 0 {
			return nil, f.wrap(fmt.Errorf("model %q is not listed", key))
		}
		if f.Default == key {
			return nil, f.wrap(fmt.Errorf("model %q is the default; change default first", key))
		}
		t, err := findModelsTable(lines)
		if err != nil {
			return nil, err
		}
		line, ok := t.entries[key]
		if !ok {
			return nil, f.wrap(fmt.Errorf("model %q: no [models] line to remove; edit by hand", key))
		}
		f.Models = slices.Delete(f.Models, i, i+1)
		return slices.Delete(lines, line, line+1), nil
	})
}

// SetCatalog replaces the catalog tag's value in place, keeping the rest of its line; a file with no catalog key gets one above its first key or table.
func SetCatalog(path, tag string) error {
	if tag == "" {
		return errors.New("modelroutes: empty catalog tag")
	}
	return editFile(path, func(f *File, lines []srcLine) ([]srcLine, error) {
		f.Catalog = tag
		at := len(lines)
		for i, l := range lines {
			if len(l.table) > 0 {
				at = min(at, i)
				break
			}
			if l.kind == lineBlank {
				continue
			}
			at = min(at, i)
			if l.kind == lineKey && l.key == "catalog" {
				text, err := replaceStringValue(l.text, tag)
				if err != nil {
					return nil, f.wrap(fmt.Errorf("line %d: %w", i+1, err))
				}
				lines[i].text = text
				return lines, nil
			}
		}
		eol := lineEnding(lines)
		if at == len(lines) && at > 0 {
			lines[at-1].eol = eol
		}
		return slices.Insert(lines, at, srcLine{text: "catalog = " + quote(tag), eol: eol}), nil
	})
}

// editFile applies edit to the file's lines and writes the result only if it parses back to exactly the File edit says it intends, so a layout the line scanner misread can never be corrupted silently.
func editFile(path string, edit func(f *File, lines []srcLine) ([]srcLine, error)) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("modelroutes: %w", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("modelroutes: %w", err)
	}
	f, err := parse(data, path)
	if err != nil {
		return err
	}
	lines, err := edit(f, scanLines(data))
	if err != nil {
		return err
	}
	out := joinLines(lines)
	got, err := Parse(out)
	if err != nil || !sameContent(got, f) {
		return f.wrap(errors.New("cannot edit this layout safely; edit by hand"))
	}
	if err := atomicfile.Write(path, out, info.Mode().Perm()); err != nil {
		return fmt.Errorf("modelroutes: %w", err)
	}
	return nil
}

func sameContent(a, b *File) bool {
	return a.Catalog == b.Catalog && a.Default == b.Default && slices.Equal(a.Routes, b.Routes) && slices.Equal(a.Models, b.Models)
}

// modelsTable is where the [models] entries sit; header is -1 when the file has no [models] table.
type modelsTable struct {
	header  int
	last    int // last entry line, or the header when there is none
	entries map[string]int
}

// findModelsTable refuses any [models] layout whose entries are not each one self-contained line, because a line edit there could split an entry.
func findModelsTable(lines []srcLine) (modelsTable, error) {
	t := modelsTable{header: -1, last: -1, entries: map[string]int{}}
	unsafe := func(i int, why string) (modelsTable, error) {
		return modelsTable{}, fmt.Errorf("modelroutes: line %d: %s; edit [models] by hand", i+1, why)
	}
	for i, l := range lines {
		inModels := len(l.table) > 0 && l.table[0] == "models"
		switch {
		case l.kind == lineKey && len(l.table) == 0 && l.key == "models":
			return unsafe(i, "models is written inline, not as a [models] table")
		case l.kind == lineHeader && inModels && len(l.table) > 1:
			return unsafe(i, "[models] entry written as a sub-table")
		case l.kind == lineHeader && inModels:
			t.header, t.last = i, i
		case !inModels || l.kind == lineBlank:
		case l.kind != lineKey:
			return unsafe(i, "[models] entry spans several lines")
		case !singleLineEntry(l.value):
			return unsafe(i, "[models] entry is not a route name or { model, route }")
		default:
			if _, dup := t.entries[l.key]; dup {
				return unsafe(i, fmt.Sprintf("[models] key %q is split across lines", l.key))
			}
			t.entries[l.key], t.last = i, i
		}
	}
	return t, nil
}

func singleLineEntry(v any) bool {
	switch v := v.(type) {
	case string:
		return true
	case map[string]any:
		_, model := v["model"].(string)
		_, route := v["route"].(string)
		return len(v) == 2 && model && route
	}
	return false
}

// entryText renders m, lining its '=' up with like, the entry above it, when that fits.
func entryText(m ModelRoute, like *srcLine) string {
	key, value := quote(m.Key), quote(m.Route)
	if m.Model != m.Key {
		value = "{ model = " + quote(m.Model) + ", route = " + quote(m.Route) + " }"
	}
	indent, pad := "", 1
	if like != nil {
		indent = like.text[:len(like.text)-len(strings.TrimLeft(like.text, " \t"))]
		if eq := strings.Index(like.text, "="); eq > len(indent)+len(key) {
			pad = eq - len(indent) - len(key)
		}
	}
	return indent + key + strings.Repeat(" ", pad) + "= " + value
}

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

func lineEnding(lines []srcLine) string {
	for _, l := range lines {
		if l.eol != "" {
			return l.eol
		}
	}
	return "\n"
}
