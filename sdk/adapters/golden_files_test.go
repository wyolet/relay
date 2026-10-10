package adapters_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite the golden files under testdata/ from the current translators")

// checkGolden compares got with the golden file at path, or rewrites the file
// under -update.
func checkGolden(t *testing.T, path string, got []byte) {
	t.Helper()
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Errorf("missing golden %s (run go test -update): %v", path, err)
		return
	}
	if !bytes.Equal(want, got) {
		t.Errorf("%s differs from the translator output (run go test -update to accept):\n%s", path, firstDifference(want, got))
	}
}

// firstDifference shows the first differing line of two goldens with a little context.
func firstDifference(want, got []byte) string {
	w := strings.Split(string(want), "\n")
	g := strings.Split(string(got), "\n")
	for i := 0; i < len(w) || i < len(g); i++ {
		if i < len(w) && i < len(g) && w[i] == g[i] {
			continue
		}
		var b strings.Builder
		for j := max(0, i-3); j < i; j++ {
			fmt.Fprintf(&b, "  %4d   %s\n", j+1, w[j])
		}
		if i < len(w) {
			fmt.Fprintf(&b, "- %4d   %s\n", i+1, w[i])
		}
		if i < len(g) {
			fmt.Fprintf(&b, "+ %4d   %s\n", i+1, g[i])
		}
		return b.String()
	}
	return ""
}

// pretty renders JSON indented, with HTML-safe escapes undone so markers like
// <system> read as written, and volatile values (timestamps and ids minted
// from the clock) replaced by "<now>".
func pretty(raw []byte) ([]byte, error) {
	var b bytes.Buffer
	if err := json.Indent(&b, unescapeHTML(raw), "", "  "); err != nil {
		return nil, fmt.Errorf("indent %s: %w", raw, err)
	}
	out := scrubClock(b.Bytes())
	return append(out, '\n'), nil
}

func prettyValue(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return pretty(raw)
}

// unescapeHTML turns json.Marshal's <, > and & back into the
// characters, leaving every other escape (including an escaped backslash) alone.
func unescapeHTML(raw []byte) []byte {
	repl := map[string]byte{"003c": '<', "003e": '>', "0026": '&'}
	out := make([]byte, 0, len(raw))
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\\' || i+1 >= len(raw) {
			out = append(out, raw[i])
			continue
		}
		if raw[i+1] == 'u' && i+6 <= len(raw) {
			if c, ok := repl[string(raw[i+2:i+6])]; ok {
				out = append(out, c)
				i += 5
				continue
			}
		}
		out = append(out, raw[i], raw[i+1])
		i++
	}
	return out
}

var digitRun = regexp.MustCompile(`\d{10,19}`)

// scrubClock replaces digit runs that read as the current unix time in seconds
// or nanoseconds with <now> — quoted when the run is a whole JSON number, so
// the golden stays valid JSON. Fixture timestamps are years old and survive.
func scrubClock(b []byte) []byte {
	now := time.Now()
	var out []byte
	last := 0
	for _, loc := range digitRun.FindAllIndex(b, -1) {
		if !readsAsNow(b[loc[0]:loc[1]], now) {
			continue
		}
		out = append(out, b[last:loc[0]]...)
		if isNumberValue(b, loc[0]) {
			out = append(out, `"<now>"`...)
		} else {
			out = append(out, "<now>"...)
		}
		last = loc[1]
	}
	return append(out, b[last:]...)
}

func readsAsNow(digits []byte, now time.Time) bool {
	n, err := strconv.ParseInt(string(digits), 10, 64)
	if err != nil {
		return false
	}
	var at time.Time
	switch len(digits) {
	case 10:
		at = time.Unix(n, 0)
	case 19:
		at = time.Unix(0, n)
	default:
		return false
	}
	d := now.Sub(at)
	return d > -time.Hour && d < time.Hour
}

// isNumberValue reports whether the token starting at i follows a JSON
// structural character, i.e. is a bare number rather than part of a string.
func isNumberValue(b []byte, i int) bool {
	for j := i - 1; j >= 0; j-- {
		switch b[j] {
		case ' ', '\n', '\t', '\r':
			continue
		case ':', ',', '[':
			return true
		default:
			return false
		}
	}
	return false
}

// sseFrame is one stream frame as a golden records it; data is inlined as
// JSON when it parses, else kept as a string (e.g. [DONE]).
type sseFrame struct {
	Event string          `json:"event,omitempty"`
	Data  json.RawMessage `json:"data"`
}

// splitFrames cuts a stream at its blank-line separators; each returned frame
// keeps its "\n\n" terminator, the unit a translator stream function takes.
func splitFrames(stream []byte) [][]byte {
	var frames [][]byte
	for _, f := range bytes.Split(stream, []byte("\n\n")) {
		if len(bytes.TrimSpace(f)) == 0 {
			continue
		}
		frames = append(frames, append(append([]byte(nil), f...), '\n', '\n'))
	}
	return frames
}

// recordFrames converts SSE frames to their golden form.
func recordFrames(frames [][]byte) []sseFrame {
	out := make([]sseFrame, 0, len(frames))
	for _, f := range frames {
		var ev string
		var data []string
		for _, line := range strings.Split(strings.TrimRight(string(f), "\n"), "\n") {
			switch {
			case strings.HasPrefix(line, "event:"):
				ev = strings.TrimSpace(line[len("event:"):])
			case strings.HasPrefix(line, "data:"):
				data = append(data, strings.TrimSpace(line[len("data:"):]))
			}
		}
		joined := strings.Join(data, "\n")
		raw := json.RawMessage(joined)
		if !json.Valid(raw) {
			raw, _ = json.Marshal(joined)
		}
		out = append(out, sseFrame{Event: ev, Data: raw})
	}
	return out
}

// errorGolden is the golden form of a direction that refused its input.
func errorGolden(err error) []byte {
	b, _ := prettyValue(map[string]string{"error": err.Error()})
	return b
}

// checkNoStrayFiles fails on files in a case dir that are neither an input
// nor a golden the harness wrote this run, so a removed input cannot leave
// its old goldens behind. Under -update the strays are deleted.
func checkNoStrayFiles(t *testing.T, dir string, known map[string]bool) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if known[e.Name()] {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if *update {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			continue
		}
		t.Errorf("stray file %s: not an input and no direction writes it", path)
	}
}
