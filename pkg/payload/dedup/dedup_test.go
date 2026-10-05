package dedup

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func roundTrip(t *testing.T, body []byte, names []string) Split {
	t.Helper()
	s := SplitBody(body, names)
	got, err := Rebuild(s.Skeleton, s.Fields, s.lookup())
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("Rebuild mismatch\n got: %q\nwant: %q", got, body)
	}
	return s
}

func TestSplitBody(t *testing.T) {
	anthropic := []string{"system", "messages", "tools"}
	cases := []struct {
		name      string
		body      string
		names     []string
		wantWhole bool
		wantField map[string]int // field name → piece count
	}{
		{
			name:      "compact messages",
			body:      `{"model":"m","system":[{"type":"text","text":"be brief"}],"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"hello"}],"tools":[],"stream":true}`,
			names:     anthropic,
			wantField: map[string]int{"system": 1, "messages": 2, "tools": 0},
		},
		{
			name:      "string system is one piece",
			body:      `{"system":"you are terse","messages":[{"role":"user","content":"x"}]}`,
			names:     anthropic,
			wantField: map[string]int{"system": 1, "messages": 1},
		},
		{
			name:      "generic split takes every top-level array",
			body:      `{"input":[1,2,3],"model":"m","tools":[{"a":"]}"}]}`,
			wantField: map[string]int{"input": 3, "tools": 1},
		},
		{
			name:      "whitespace around elements",
			body:      "{ \"messages\" : [ {\"a\":1},{\"b\":2} ] }",
			names:     anthropic,
			wantField: map[string]int{"messages": 2},
		},
		{
			name:      "escaped key",
			body:      `{"messages":[{"a":1}]}`,
			names:     anthropic,
			wantField: map[string]int{"messages": 1},
		},
		{
			name:      "strings containing brackets and escapes",
			body:      `{"messages":[{"t":"a \"]}, [{\" b"},{"t":"\\"}]}`,
			names:     anthropic,
			wantField: map[string]int{"messages": 2},
		},
		{name: "pretty-printed separators", body: "{\"messages\":[\n  {\"a\":1},\n  {\"b\":2}\n]}", names: anthropic, wantWhole: true},
		{name: "truncated", body: `{"messages":[{"role":"user","content":"hel`, names: anthropic, wantWhole: true},
		{name: "top-level array", body: `[1,2]`, names: anthropic, wantWhole: true},
		{name: "not json", body: "\x00\x01binary", names: anthropic, wantWhole: true},
		{name: "empty", body: "", names: anthropic, wantWhole: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := roundTrip(t, []byte(c.body), c.names)
			if s.Whole() != c.wantWhole {
				t.Fatalf("Whole = %v, want %v", s.Whole(), c.wantWhole)
			}
			if c.wantWhole {
				return
			}
			got := map[string]int{}
			for _, f := range s.Fields {
				got[f.Name] = len(f.Hashes)
			}
			if len(got) != len(c.wantField) {
				t.Fatalf("fields = %v, want %v", got, c.wantField)
			}
			for k, n := range c.wantField {
				if got[k] != n {
					t.Fatalf("field %q pieces = %d, want %d (all: %v)", k, got[k], n, got)
				}
			}
		})
	}
}

// Consecutive turns of one conversation share every earlier message.
func TestSplitBodySharesPiecesAcrossTurns(t *testing.T) {
	names := []string{"messages"}
	turn1 := SplitBody([]byte(`{"messages":[{"role":"user","content":"a"}]}`), names)
	turn2 := SplitBody([]byte(`{"messages":[{"role":"user","content":"a"},{"role":"assistant","content":"b"},{"role":"user","content":"c"}]}`), names)
	h1, h2 := turn1.Fields[0].Hashes, turn2.Fields[0].Hashes
	if CommonPrefix(h1, h2) != 1 {
		t.Fatalf("CommonPrefix = %d, want 1", CommonPrefix(h1, h2))
	}
}

func TestSplitBodyDedupsWithinBody(t *testing.T) {
	s := SplitBody([]byte(`{"messages":[{"a":1},{"a":1},{"a":1}]}`), []string{"messages"})
	if len(s.Fields[0].Hashes) != 3 || len(s.Pieces) != 1 {
		t.Fatalf("hashes = %d, pieces = %d; want 3, 1", len(s.Fields[0].Hashes), len(s.Pieces))
	}
}

func TestRebuildMissingPiece(t *testing.T) {
	s := SplitBody([]byte(`{"messages":[{"a":1}]}`), []string{"messages"})
	_, err := Rebuild(s.Skeleton, s.Fields, func(Hash) ([]byte, bool) { return nil, false })
	if err == nil {
		t.Fatal("want ErrMissingPiece")
	}
}

func FuzzSplitBody(f *testing.F) {
	for _, s := range []string{
		`{"messages":[{"role":"user","content":"hi"}],"system":"x"}`,
		`{"a":[1,[2,3],{"b":"]"}],"c":{"d":[]}}`,
		"{ \"messages\" : [ 1 , 2 ] }",
		`{"messages":[`,
		`[]`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		for _, names := range [][]string{nil, {"messages", "system", "tools"}} {
			roundTrip(t, body, names)
		}
	})
}

// TestSplitBodySample runs Split over captured request bodies when
// RELAY_DEDUP_SAMPLE_DIR names a directory of JSONL files whose lines hold
// the body in a "body" string field.
func TestSplitBodySample(t *testing.T) {
	dir := os.Getenv("RELAY_DEDUP_SAMPLE_DIR")
	if dir == "" {
		t.Skip("RELAY_DEDUP_SAMPLE_DIR unset")
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no samples in %s: %v", dir, err)
	}
	names := []string{"system", "messages", "tools"}
	var bodies, whole, raw, stored, skeletons, pieces, hashes int
	seen := map[Hash]bool{}
	for _, fn := range files {
		f, err := os.Open(fn)
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64<<20), 64<<20)
		for sc.Scan() {
			var r struct {
				Body string `json:"body"`
			}
			if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
				t.Fatal(err)
			}
			s := roundTrip(t, []byte(r.Body), names)
			bodies++
			raw += len(r.Body)
			skeletons += len(s.Skeleton)
			if s.Whole() {
				whole++
			}
			for _, p := range s.Pieces {
				if !seen[p.Hash] {
					seen[p.Hash] = true
					pieces += len(p.Body)
				}
			}
			for _, fl := range s.Fields {
				hashes += len(fl.Hashes) * len(Hash{})
			}
		}
		f.Close()
	}
	stored = skeletons + pieces + hashes
	t.Logf("bodies=%d stored-whole=%d raw=%d stored=%d (skeletons=%d pieces=%d hashes=%d) ratio=%.1fx",
		bodies, whole, raw, stored, skeletons, pieces, hashes, float64(raw)/float64(stored))
}
