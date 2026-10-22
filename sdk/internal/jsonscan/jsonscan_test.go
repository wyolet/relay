package jsonscan

import (
	"reflect"
	"testing"
)

func members(b string) ([][2]string, bool) {
	var out [][2]string
	m := Object([]byte(b))
	for m.Next() {
		out = append(out, [2]string{string(m.Key()), string(m.Value())})
	}
	return out, m.Err()
}

func TestObject(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want [][2]string
		bad  bool
	}{
		{"compact", `{"a":1,"b":"x","c":null}`, [][2]string{{"a", "1"}, {"b", `"x"`}, {"c", "null"}}, false},
		{"whitespace", " {\n \"a\" : 1 ,\t\"b\"\r\n:\n[1, 2] }", [][2]string{{"a", "1"}, {"b", "[1, 2]"}}, false},
		{"empty", `{ }`, nil, false},
		{"nested values", `{"a":{"b":[{"c":"}]"}]},"d":true}`, [][2]string{{"a", `{"b":[{"c":"}]"}]}`}, {"d", "true"}}, false},
		{"escaped quotes in strings", `{"a":"x\"y\\","b":"\\\"}"}`, [][2]string{{"a", `"x\"y\\"`}, {"b", `"\\\"}"`}}, false},
		{"escaped key", `{"\u0075sage":1}`, nil, true},
		{"not an object", `[1]`, nil, true},
		{"truncated", `{"a":"xy`, nil, true},
		{"missing colon", `{"a" 1}`, nil, true},
		{"missing separator", `{"a":1 "b":2}`, [][2]string{{"a", "1"}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, bad := members(tc.in)
			if !reflect.DeepEqual(got, tc.want) || bad != tc.bad {
				t.Errorf("got %q bad=%v, want %q bad=%v", got, bad, tc.want, tc.bad)
			}
		})
	}
}

func TestArray(t *testing.T) {
	cases := []struct {
		in   string
		want []string
		bad  bool
	}{
		{`[{"a":1}, "x" ,null]`, []string{`{"a":1}`, `"x"`, "null"}, false},
		{` [ ] `, nil, false},
		{`null`, nil, true},
		{`[1,`, []string{"1"}, true},
	}
	for _, tc := range cases {
		var got []string
		e := Array([]byte(tc.in))
		for e.Next() {
			got = append(got, string(e.Value()))
		}
		if !reflect.DeepEqual(got, tc.want) || e.Err() != tc.bad {
			t.Errorf("%s: got %q bad=%v, want %q bad=%v", tc.in, got, e.Err(), tc.want, tc.bad)
		}
	}
}

func TestValuePredicates(t *testing.T) {
	if !KeyIs([]byte("Usage"), "usage") || KeyIs([]byte("usages"), "usage") {
		t.Error("KeyIs must fold case and nothing else")
	}
	if !NullOrEmpty([]byte("null")) || !NullOrEmpty([]byte(`""`)) || NullOrEmpty([]byte(`"x"`)) {
		t.Error("NullOrEmpty")
	}
	if !StringIs([]byte(`"flex"`), "flex") || StringIs([]byte(`"fle\u0078"`), "flex") || StringIs([]byte(`"flex"`), "") {
		t.Error("StringIs")
	}
}

func TestScanAllocs(t *testing.T) {
	in := []byte(`{"id":"c1","choices":[{"index":0,"delta":{"content":"The quick brown fox"},"finish_reason":null}],"usage":null}`)
	allocs := testing.AllocsPerRun(100, func() {
		m := Object(in)
		for m.Next() {
			if KeyIs(m.Key(), "choices") {
				e := Array(m.Value())
				for e.Next() {
				}
			}
		}
	})
	if allocs != 0 {
		t.Errorf("allocs = %v, want 0", allocs)
	}
}
