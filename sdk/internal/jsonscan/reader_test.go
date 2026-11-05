package jsonscan

import (
	"encoding/json"
	"strings"
	"testing"
)

// bu starts a JSON unicode escape.
const bu = `\` + "u"

func readAll(b []byte) bool {
	r := Read(b)
	r.Value()
	return r.End()
}

var readerCases = []struct {
	in string
	ok bool
}{
	{`{"a":1,"b":[true,false,null],"c":{"d":"x\"\\\/\b\f\n\r\t` + bu + `00e9"}}`, true},
	{" \t\r\n[ -0, 0.5, -1.25e+10, 3E-2, 1e5 ] \n", true},
	{`"` + strings.Repeat("long string with no specials ", 20) + `"`, true},
	{"\"\xff\xfe invalid utf-8 is accepted\"", true},
	{`{}`, true},
	{`[]`, true},
	{`{"` + bu + `0061":1}`, false}, // escaped key: valid, but unread
	{`{"a":1,}`, false},
	{`[1,]`, false},
	{`[1 2]`, false},
	{`{"a" 1}`, false},
	{`{"a":1}x`, false},
	{`01`, false},
	{`-`, false},
	{`1.`, false},
	{`1e`, false},
	{`1e+`, false},
	{`.5`, false},
	{`+1`, false},
	{`tru`, false},
	{`nul`, false},
	{`truex`, false},
	{`"a` + "\x01" + `b"`, false},
	{`"\x"`, false},
	{`"\u12g4"`, false},
	{`"\u12"`, false},
	{`"abc`, false},
	{`{"a":"b"`, false},
	{`[`, false},
	{``, false},
	{`   `, false},
	{strings.Repeat("[", 2000) + strings.Repeat("]", 2000), false}, // valid, past maxDepth
}

func TestReaderSyntax(t *testing.T) {
	for _, tc := range readerCases {
		if got := readAll([]byte(tc.in)); got != tc.ok {
			t.Errorf("%q: read %v, want %v", tc.in, got, tc.ok)
		}
	}
}

func TestReaderMembers(t *testing.T) {
	r := Read([]byte(` {"skip":{"x":[1,{"y":"}"}]},"arr":[10, {"k":"v"}, "s"],"v":"x"} `))
	var got []string
	ok := r.Object(func(key []byte) {
		switch string(key) {
		case "arr":
			r.Array(func(i int) {
				if i == 1 {
					r.Object(func(k []byte) { got = append(got, string(k)+"="+string(r.Value())) })
				}
			})
		case "v":
			got = append(got, "v="+string(r.Value()))
		}
	})
	if !ok || !r.End() || strings.Join(got, ",") != `k="v",v="x"` {
		t.Errorf("ok=%v end=%v got %q", ok, r.End(), got)
	}
}

func TestIntField(t *testing.T) {
	for in, want := range map[string]bool{
		`7`: true, `-0`: true, `null`: true,
		`1.0`: false, `1e2`: false, `9223372036854775808`: false, `"1"`: false, `true`: false, `{}`: false,
	} {
		n := 3
		r := Read([]byte(in))
		ok := r.IntField(&n)
		var ref struct{ N int }
		ref.N = 3
		refOK := json.Unmarshal([]byte(`{"N":`+in+`}`), &ref) == nil
		if ok != want || ok != refOK || (ok && n != ref.N) {
			t.Errorf("IntField(%s) = %d, %v; encoding/json %d, %v", in, n, ok, ref.N, refOK)
		}
	}
}

func TestString(t *testing.T) {
	for in, want := range map[string]string{
		`"plain"`:              "plain",
		`"a\"b` + bu + `00e9"`: "a\"bé",
		"\"bad \xff\"":         "bad �",
		`"\ud800 lone"`:        "� lone",
		`""`:                   "",
	} {
		if got, ok := String([]byte(in)); !ok || got != want {
			t.Errorf("String(%q) = %q, %v; want %q", in, got, ok, want)
		}
		if !StringDecodesTo([]byte(in), want) {
			t.Errorf("StringDecodesTo(%q, %q) = false", in, want)
		}
	}
	if _, ok := String([]byte(`5`)); ok {
		t.Error("String accepted a number")
	}
}

// FuzzReader checks the Reader never accepts a text encoding/json rejects.
func FuzzReader(f *testing.F) {
	for _, tc := range readerCases {
		f.Add([]byte(tc.in))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		if readAll(b) && !json.Valid(b) {
			t.Errorf("read %q, which encoding/json rejects", b)
		}
	})
}
