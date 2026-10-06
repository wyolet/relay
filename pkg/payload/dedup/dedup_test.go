package dedup

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func roundTrip(t *testing.T, body []byte) Split {
	t.Helper()
	s := SplitBody(body)
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
	cases := []struct {
		name      string
		body      string
		wantWhole bool
		wantField map[string]int // field name → piece count
	}{
		{
			name:      "compact messages",
			body:      `{"model":"m","system":[{"type":"text","text":"be brief"}],"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"hello"}],"tools":[],"stream":true}`,
			wantField: map[string]int{"system": 1, "messages": 2, "tools": 0},
		},
		{
			name:      "short string stays in the skeleton",
			body:      `{"system":"you are terse","messages":[{"role":"user","content":"x"}]}`,
			wantField: map[string]int{"messages": 1},
		},
		{
			name:      "every top-level array is split",
			body:      `{"input":[1,2,3],"model":"m","tools":[{"a":"]}"}]}`,
			wantField: map[string]int{"input": 3, "tools": 1},
		},
		{
			name:      "whitespace around elements",
			body:      "{ \"messages\" : [ {\"a\":1},{\"b\":2} ] }",
			wantField: map[string]int{"messages": 2},
		},
		{
			name:      "escaped key",
			body:      `{"messages":[{"a":1}]}`,
			wantField: map[string]int{"messages": 1},
		},
		{
			name:      "strings containing brackets and escapes",
			body:      `{"messages":[{"t":"a \"]}, [{\" b"},{"t":"\\"}]}`,
			wantField: map[string]int{"messages": 2},
		},
		{name: "pretty-printed separators", body: "{\"messages\":[\n  {\"a\":1},\n  {\"b\":2}\n]}", wantWhole: true},
		{name: "truncated", body: `{"messages":[{"role":"user","content":"hel`, wantWhole: true},
		{name: "top-level array", body: `[1,2]`, wantWhole: true},
		{name: "not json", body: "\x00\x01binary", wantWhole: true},
		{name: "empty", body: "", wantWhole: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := roundTrip(t, []byte(c.body))
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

func TestSplitBodyLargeValueIsPiece(t *testing.T) {
	long := strings.Repeat("a", 2048)
	body := []byte(`{"model":"x","instructions":"` + long + `","input":[1]}`)
	s := roundTrip(t, body)
	got := map[string]bool{}
	for _, f := range s.Fields {
		got[f.Name] = f.Array
	}
	if isArray, ok := got["instructions"]; !ok || isArray {
		t.Fatalf("instructions not a single piece: %+v", s.Fields)
	}
	if _, ok := got["model"]; ok {
		t.Fatalf("model split out: %+v", s.Fields)
	}
	if !bytes.Contains(s.Skeleton, []byte(`"model":"x"`)) || bytes.Contains(s.Skeleton, []byte(long)) {
		t.Fatalf("skeleton = %q", s.Skeleton)
	}
}

// The threshold counts the value's raw bytes, quotes included.
func TestSplitBodyPieceThreshold(t *testing.T) {
	for _, c := range []struct {
		size  int
		piece bool
	}{{minValuePiece - 1, false}, {minValuePiece, true}} {
		v := `"` + strings.Repeat("b", c.size-2) + `"`
		s := roundTrip(t, []byte(`{"v":`+v+`}`))
		if got := len(s.Fields) == 1; got != c.piece {
			t.Fatalf("value of %d bytes: piece = %v, want %v", c.size, got, c.piece)
		}
	}
}

// Consecutive turns of one conversation share every earlier message.
func TestSplitBodySharesPiecesAcrossTurns(t *testing.T) {
	turn1 := SplitBody([]byte(`{"messages":[{"role":"user","content":"a"}]}`))
	turn2 := SplitBody([]byte(`{"messages":[{"role":"user","content":"a"},{"role":"assistant","content":"b"},{"role":"user","content":"c"}]}`))
	h1, h2 := turn1.Fields[0].Hashes, turn2.Fields[0].Hashes
	if CommonPrefix(h1, h2) != 1 {
		t.Fatalf("CommonPrefix = %d, want 1", CommonPrefix(h1, h2))
	}
}

func TestSplitBodyDedupsWithinBody(t *testing.T) {
	s := SplitBody([]byte(`{"messages":[{"a":1},{"a":1},{"a":1}]}`))
	if len(s.Fields[0].Hashes) != 3 || len(s.Pieces) != 1 {
		t.Fatalf("hashes = %d, pieces = %d; want 3, 1", len(s.Fields[0].Hashes), len(s.Pieces))
	}
}

func TestRebuildMissingPiece(t *testing.T) {
	s := SplitBody([]byte(`{"messages":[{"a":1}]}`))
	_, err := Rebuild(s.Skeleton, s.Fields, func(Hash) ([]byte, bool) { return nil, false })
	if !errors.Is(err, ErrMissingPiece) {
		t.Fatalf("err = %v, want ErrMissingPiece", err)
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
	f.Add([]byte(`{"system":"` + strings.Repeat("s", minValuePiece) + `","messages":[{"a":1}]}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		roundTrip(t, body)
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
			s := roundTrip(t, []byte(r.Body))
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
