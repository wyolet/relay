package usagelog

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/pkg/usage"
)

func tagObject(n int) string {
	pairs := make([]string, n)
	for i := range pairs {
		pairs[i] = fmt.Sprintf(`"k%d":"v"`, i)
	}
	return "{" + strings.Join(pairs, ",") + "}"
}

func TestParseTags(t *testing.T) {
	longKey, longValue := strings.Repeat("k", usage.MaxTagKeyLen), strings.Repeat("v", maxTagValueLen)
	padded := func(size int) string { return `{"k":"v"}` + strings.Repeat(" ", size-len(`{"k":"v"}`)) }

	accepted := map[string]struct {
		raw  string
		want map[string]string
	}{
		"two tags":            {`{"session_id":"0988342e","leg":"translate"}`, map[string]string{"session_id": "0988342e", "leg": "translate"}},
		"empty value":         {`{"k":""}`, map[string]string{"k": ""}},
		"key at the limit":    {`{"` + longKey + `":"v"}`, map[string]string{longKey: "v"}},
		"value at the limit":  {`{"k":"` + longValue + `"}`, map[string]string{"k": longValue}},
		"count at the limit":  {tagObject(maxTagCount), nil},
		"duplicate key":       {`{"k":"a","k":"b"}`, map[string]string{"k": "b"}},
		"surrounding spaces":  {` {"k":"v"} `, map[string]string{"k": "v"}},
		"blob at the max len": {padded(MaxTagsHeaderBytes), map[string]string{"k": "v"}},
	}
	for name, tc := range accepted {
		tags, ok := ParseTags(tc.raw)
		if !ok {
			t.Errorf("%s: dropped", name)
			continue
		}
		if tc.want != nil {
			if diff := cmp.Diff(tc.want, tags); diff != "" {
				t.Errorf("%s (-want +got):\n%s", name, diff)
			}
		}
	}
	if tags, _ := ParseTags(tagObject(maxTagCount)); len(tags) != maxTagCount {
		t.Errorf("count at the limit: %d tags, want %d", len(tags), maxTagCount)
	}

	dropped := map[string]string{
		"invalid json":     `{not json`,
		"trailing data":    `{"k":"v"} x`,
		"top-level array":  `["a","b"]`,
		"top-level string": `"a"`,
		"top-level null":   `null`,
		"number value":     `{"a":1}`,
		"bool value":       `{"a":true}`,
		"null value":       `{"a":null}`,
		"nested object":    `{"a":{"b":"c"}}`,
		"array value":      `{"a":["b"]}`,
		"one bad value":    `{"a":"ok","b":2}`,
		"too many keys":    tagObject(maxTagCount + 1),
		"empty key":        `{"":"v"}`,
		"key too long":     `{"` + longKey + `k":"v"}`,
		"value too long":   `{"k":"` + longValue + `v"}`,
		"empty object":     `{}`,
		"empty string":     ``,
		"oversized blob":   padded(MaxTagsHeaderBytes + 1),
	}
	for name, raw := range dropped {
		if tags, ok := ParseTags(raw); ok || tags != nil {
			t.Errorf("%s: want the blob dropped, got %v", name, tags)
		}
	}
}
