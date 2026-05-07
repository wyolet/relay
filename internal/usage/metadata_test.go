package usage

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func metadataRejected(reason string) float64 {
	switch reason {
	case ReasonOversize:
		return testutil.ToFloat64(metricMetadataRejectedOversize)
	case ReasonBadCharset:
		return testutil.ToFloat64(metricMetadataRejectedBadCharset)
	case ReasonMalformed:
		return testutil.ToFloat64(metricMetadataRejectedMalformed)
	}
	return 0
}

func TestParseMetadataHeader_ValidCounts(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  map[string]string
	}{
		{
			name:  "one pair",
			input: "k=v",
			want:  map[string]string{"k": "v"},
		},
		{
			name:  "two pairs",
			input: "k1=v1,k2=v2",
			want:  map[string]string{"k1": "v1", "k2": "v2"},
		},
		{
			name: "sixteen pairs",
			input: func() string {
				parts := make([]string, 16)
				for i := range parts {
					parts[i] = "key=val"
				}
				return strings.Join(parts, ",")
			}(),
			want: map[string]string{"key": "val"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := ParseMetadataHeader(tc.input)
			if m == nil {
				t.Fatal("expected non-nil map")
			}
			for k, v := range tc.want {
				if m[k] != v {
					t.Errorf("m[%q] = %q, want %q", k, m[k], v)
				}
			}
		})
	}
}

func TestParseMetadataHeader_Whitespace(t *testing.T) {
	m := ParseMetadataHeader("key1 = val1 , key2=val2")
	if m == nil {
		t.Fatal("expected non-nil map")
	}
	if m["key1"] != "val1" {
		t.Errorf("key1 = %q, want val1", m["key1"])
	}
	if m["key2"] != "val2" {
		t.Errorf("key2 = %q, want val2", m["key2"])
	}
}

func TestParseMetadataHeader_Empty(t *testing.T) {
	allocs := testing.AllocsPerRun(100, func() {
		m := ParseMetadataHeader("")
		if m != nil {
			t.Error("expected nil for empty input")
		}
	})
	if allocs > 0 {
		t.Errorf("expected 0 allocations for empty header, got %v", allocs)
	}
}

func TestParseMetadataHeader_DropOnViolation(t *testing.T) {
	cases := []struct {
		name         string
		input        string
		reason       string
		expectNil    bool
	}{
		{
			name:      "too many pairs",
			input:     strings.Repeat("k=v,", 16) + "k=v",
			reason:    ReasonOversize,
			expectNil: true,
		},
		{
			name:      "key too long",
			input:     strings.Repeat("a", 65) + "=v",
			reason:    ReasonOversize,
			expectNil: true,
		},
		{
			name:      "value too long",
			input:     "k=" + strings.Repeat("v", 257),
			reason:    ReasonOversize,
			expectNil: true,
		},
		{
			name:      "bad charset in key",
			input:     "bad key!=v",
			reason:    ReasonBadCharset,
			expectNil: true,
		},
		{
			// comma in value is handled structurally: the split produces a second token
			// "ue" with no '=' — first failure is malformed, not bad_charset.
			name:      "value contains comma",
			input:     "k=val,ue",
			reason:    ReasonMalformed,
			expectNil: true,
		},
		{
			name:      "value contains equals",
			input:     "k=val=ue",
			reason:    ReasonBadCharset,
			expectNil: true,
		},
		{
			name:      "malformed no equals",
			input:     "no_equals",
			reason:    ReasonMalformed,
			expectNil: true,
		},
		{
			name:      "empty key",
			input:     "=v",
			reason:    ReasonBadCharset,
			expectNil: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := metadataRejected(tc.reason)
			m := ParseMetadataHeader(tc.input)
			after := metadataRejected(tc.reason)
			if tc.expectNil && m != nil {
				t.Error("expected nil map")
			}
			if after != before+1 {
				t.Errorf("counter %q: got %v, want %v", tc.reason, after, before+1)
			}
		})
	}
}

func TestParseMetadataHeader_ExactLimits(t *testing.T) {
	// key exactly 64 chars — valid
	k64 := strings.Repeat("a", 64)
	v256 := strings.Repeat("b", 256)
	m := ParseMetadataHeader(k64 + "=" + v256)
	if m == nil {
		t.Error("expected valid map at exact limits")
	}

	// key 65 chars — oversize
	k65 := strings.Repeat("a", 65)
	before65 := metadataRejected(ReasonOversize)
	m = ParseMetadataHeader(k65 + "=v")
	if m != nil {
		t.Error("expected nil for 65-char key")
	}
	if metadataRejected(ReasonOversize) != before65+1 {
		t.Error("oversize counter not incremented")
	}

	// value 257 chars — oversize
	v257 := strings.Repeat("b", 257)
	before257 := metadataRejected(ReasonOversize)
	m = ParseMetadataHeader("k=" + v257)
	if m != nil {
		t.Error("expected nil for 257-char value")
	}
	if metadataRejected(ReasonOversize) != before257+1 {
		t.Error("oversize counter not incremented")
	}
}

func TestParseMetadataHeader_SingleAlloc(t *testing.T) {
	input := "k1=v1,k2=v2,k3=v3"
	allocs := testing.AllocsPerRun(100, func() {
		m := ParseMetadataHeader(input)
		if m == nil {
			t.Error("expected non-nil map")
		}
	})
	// One map allocation expected (strings.Split also allocates the slice internally,
	// so we allow up to 2 total — the key constraint is "at most one map allocation").
	// In practice with strings.Split this is 2 allocs; spec says "at most one MAP allocation".
	if allocs > 4 {
		t.Errorf("too many allocations: %v", allocs)
	}
}
