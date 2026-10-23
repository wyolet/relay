package inference

import (
	"bytes"
	"strings"
)

// rawKey returns member i's key as sent, without quotes.
func (o *jsonObject) rawKey(i int) []byte {
	m := &o.members[i]
	return o.b[m.keyStart:m.keyEnd]
}

// keyString returns member i's decoded key.
func (o *jsonObject) keyString(i int) string {
	if name := o.members[i].name; name != "" {
		return name
	}
	return string(o.rawKey(i))
}

// keyIs reports whether member i's decoded key is exactly key.
func (o *jsonObject) keyIs(i int, key string) bool {
	if name := o.members[i].name; name != "" {
		return name == key
	}
	return string(o.rawKey(i)) == key
}

// keyFold reports whether member i's decoded key matches key the way encoding/json matches a struct field: case-insensitively.
func (o *jsonObject) keyFold(i int, key string) bool {
	if name := o.members[i].name; name != "" {
		return strings.EqualFold(name, key)
	}
	return bytes.EqualFold(o.rawKey(i), []byte(key))
}

func (o *jsonObject) sameKey(i, j int) bool {
	if o.members[i].name == "" && o.members[j].name == "" {
		return bytes.Equal(o.rawKey(i), o.rawKey(j))
	}
	return o.keyString(i) == o.keyString(j)
}

// lastIndex returns the index of the last member whose decoded key is exactly key, -1 when there is none.
func (o *jsonObject) lastIndex(key string) int {
	for i := len(o.members) - 1; i >= 0; i-- {
		if o.keyIs(i, key) {
			return i
		}
	}
	return -1
}

// maxPairwiseKeys bounds the pairwise duplicate check; larger objects use a map so an object with many keys stays linear.
const maxPairwiseKeys = 32

// overridden marks each member a later member with the same key overrides, nil when every key is unique.
func (o *jsonObject) overridden() []bool {
	n := len(o.members)
	var out []bool
	if n <= maxPairwiseKeys {
		for i := 0; i < n; i++ {
			for j := i + 1; j < n; j++ {
				if o.sameKey(i, j) {
					if out == nil {
						out = make([]bool, n)
					}
					out[i] = true
					break
				}
			}
		}
		return out
	}
	last := make(map[string]int, n)
	for i := range o.members {
		k := o.keyString(i)
		if prev, ok := last[k]; ok {
			if out == nil {
				out = make([]bool, n)
			}
			out[prev] = true
		}
		last[k] = i
	}
	return out
}

// assemble returns the object with edits applied, keeping every other member's bytes and the whitespace between members as sent. As a round trip through map[string]json.RawMessage does, a key sent more than once keeps only its last member; an edit setting a key the object lacks adds the member at the end.
func (o *jsonObject) assemble(edits []memberEdit) []byte {
	b := o.b
	skip := o.overridden()
	size := len(b)
	for _, e := range edits {
		size += len(e.key) + len(e.value) + 4
	}
	out := make([]byte, 0, size)
	out = append(out, b[:o.open+1]...)
	wrote := false
	for i, m := range o.members {
		if skip != nil && skip[i] {
			continue
		}
		value := b[m.valStart:m.valEnd]
		if k := o.editFor(i, edits); k >= 0 {
			if edits[k].value == nil {
				continue
			}
			value = edits[k].value
		}
		// Each member's leading separator is the bytes since the previous member; the first one written has no comma to keep.
		switch {
		case wrote:
			out = append(out, b[o.members[i-1].valEnd:m.keyStart-1]...)
		case i == 0:
			out = append(out, b[o.open+1:m.keyStart-1]...)
		}
		out = append(out, b[m.keyStart-1:m.valStart]...)
		out = append(out, value...)
		wrote = true
	}
	for _, e := range edits {
		if e.value == nil || o.lastIndex(e.key) >= 0 {
			continue
		}
		if wrote {
			out = append(out, ',')
		}
		out = appendJSONString(out, e.key)
		out = append(out, ':')
		out = append(out, e.value...)
		wrote = true
	}
	tail := o.open + 1
	if n := len(o.members); n > 0 {
		tail = o.members[n-1].valEnd
	}
	return append(out, b[tail:]...)
}

func (o *jsonObject) editFor(i int, edits []memberEdit) int {
	for k := range edits {
		if o.keyIs(i, edits[k].key) {
			return k
		}
	}
	return -1
}
