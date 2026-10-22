package jsonscan

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"
)

func membersFromEnd(b string, floor int) ([][2]string, bool) {
	var out [][2]string
	m := ObjectFromEnd([]byte(b), floor)
	for m.Next() {
		out = append(out, [2]string{string(m.Key()), string(m.Value())})
	}
	return out, m.Err()
}

func TestObjectFromEnd(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		floor int
		want  [][2]string
		bad   bool
	}{
		{"compact", `{"a":1,"b":"x","c":null}`, 0, [][2]string{{"c", "null"}, {"b", `"x"`}, {"a", "1"}}, false},
		{"whitespace", " {\n \"a\" : 1 ,\t\"b\"\r\n:\n[1, 2] }\n", 0, [][2]string{{"b", "[1, 2]"}, {"a", "1"}}, false},
		{"empty", `{ }`, 0, nil, false},
		{"nested values", `{"a":{"b":[{"c":"}]"}]},"d":true}`, 0, [][2]string{{"d", "true"}, {"a", `{"b":[{"c":"}]"}]}`}}, false},
		{"escaped quotes in strings", `{"a":"x\"y\\","b":"\\\"}"}`, 0, [][2]string{{"b", `"\\\"}"`}, {"a", `"x\"y\\"`}}, false},
		{"quote after escaped backslash", `{"a":"\\","b":"q"}`, 0, [][2]string{{"b", `"q"`}, {"a", `"\\"`}}, false},
		{"escaped key", `{"\u0075sage":1}`, 0, nil, true},
		{"not an object", `[1]`, 0, nil, true},
		{"missing colon", `{"a" 1}`, 0, nil, true},
		{"missing separator", `{"a":1 "b":2}`, 0, [][2]string{{"b", "2"}}, true},
		{"floor stops before a member", `{"big":"0123456789","u":1}`, 10, [][2]string{{"u", "1"}}, true},
		{"floor below the opening brace", `{"big":"0123456789","u":1}`, 0, [][2]string{{"u", "1"}, {"big", `"0123456789"`}}, false},
		{"floor inside a backslash run", `{"a":"x\\\\\"y","b":1}`, 7, [][2]string{{"b", "1"}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, bad := membersFromEnd(tc.in, tc.floor)
			if !reflect.DeepEqual(got, tc.want) || bad != tc.bad {
				t.Errorf("got %q bad=%v, want %q bad=%v", got, bad, tc.want, tc.bad)
			}
		})
	}
}

func TestArrayFromEnd(t *testing.T) {
	cases := []struct {
		in   string
		want []string
		bad  bool
	}{
		{`[{"a":1}, "x" ,null]`, []string{"null", `"x"`, `{"a":1}`}, false},
		{` [ ] `, nil, false},
		{`null`, nil, true},
		{`,1]`, []string{"1"}, true},
	}
	for _, tc := range cases {
		var got []string
		e := ArrayFromEnd([]byte(tc.in), 0)
		for e.Next() {
			got = append(got, string(e.Value()))
		}
		if !reflect.DeepEqual(got, tc.want) || e.Err() != tc.bad {
			t.Errorf("%s: got %q bad=%v, want %q bad=%v", tc.in, got, e.Err(), tc.want, tc.bad)
		}
	}
}

func TestFromEndDescendAndUnread(t *testing.T) {
	in := []byte(`{"choices":[{"message":{"content":"long text","refusal":null},"finish_reason":"stop"}],"usage":{"n":1}}`)
	top := ObjectFromEnd(in, 0)
	if !top.Next() || string(top.Key()) != "usage" {
		t.Fatalf("first member %q", top.Key())
	}
	if got := string(top.Unread()); got != `{"choices":[{"message":{"content":"long text","refusal":null},"finish_reason":"stop"}],` {
		t.Errorf("Unread = %q", got)
	}
	if !top.Next() || string(top.Key()) != "choices" {
		t.Fatalf("second member %q", top.Key())
	}
	choices := top.ValueArray()
	if !choices.Next() {
		t.Fatal("no choice")
	}
	choice := choices.ValueObject()
	var keys []string
	for choice.Next() {
		keys = append(keys, string(choice.Key()))
		if string(choice.Key()) == "message" {
			msg := choice.ValueObject()
			if !msg.Next() || string(msg.Key()) != "refusal" || string(msg.Value()) != "null" {
				t.Errorf("message's last member %q = %q", msg.Key(), msg.Value())
			}
		}
	}
	if !slices.Equal(keys, []string{"finish_reason", "message"}) || choice.Err() {
		t.Errorf("choice members %q err=%v", keys, choice.Err())
	}
	if choices.Next() || choices.Err() {
		t.Errorf("choices: a second element or an error")
	}
}

func TestHasLetterEscape(t *testing.T) {
	for in, want := range map[string]bool{
		`"plain"`:               false,
		`"\u003cb\u003e\u0026"`: false,
		`"\u001b[0m"`:           false,
		`"\u0041"`:              true,
		`"f\u0075nction"`:       true,
		`"\u005f"`:              true,
		`"\u00`:                 false,
		`trailing \`:            false,
	} {
		if got := HasLetterEscape([]byte(in)); got != want {
			t.Errorf("HasLetterEscape(%s) = %v, want %v", in, got, want)
		}
	}
}

func TestMayHoldString(t *testing.T) {
	for in, want := range map[string]bool{
		`{"output":[],"usage":{}}`:  true,
		`{"output":[],"Usage":{}}`:  true,
		`{"output":"a usage note"}`: false,
		`{"text":"\"usage\""}`:      false,
		"{\"ſtatus\":1}":            true,
		"{\"note\":\"20 K\"}":       true,
	} {
		if got := MayHoldString([]byte(in), "usage", "status"); got != want {
			t.Errorf("MayHoldString(%s) = %v, want %v", in, got, want)
		}
	}
}

// FuzzObjectFromEnd checks that walking a valid object from its end yields the members the forward walk does, in reverse, and that a floor only ever cuts the walk short.
func FuzzObjectFromEnd(f *testing.F) {
	for _, s := range []string{
		`{"a":1,"b":"x","c":null}`,
		`{"a":{"b":[{"c":"}]"}]},"d":true}`,
		`{"a":"x\"y\\","b":"\\\"}"}`,
		`{"choices":[{"message":{"content":"t","refusal":null},"finish_reason":"stop"}],"usage":{"n":1}}`,
		` { "k" : [ 1 , 2 , { } ] , "v" : -1.5e3 } `,
	} {
		f.Add([]byte(s), 0)
		f.Add([]byte(s), 8)
	}
	f.Fuzz(func(t *testing.T, b []byte, floor int) {
		if !json.Valid(b) {
			return
		}
		var fwd [][2]string
		m := Object(b)
		for m.Next() {
			fwd = append(fwd, [2]string{string(m.Key()), string(m.Value())})
		}
		if m.Err() {
			return
		}
		slices.Reverse(fwd)
		var back [][2]string
		e := ObjectFromEnd(b, 0)
		for e.Next() {
			back = append(back, [2]string{string(e.Key()), string(e.Value())})
		}
		if e.Err() || !reflect.DeepEqual(back, fwd) {
			t.Fatalf("from end %q (err %v), forward reversed %q", back, e.Err(), fwd)
		}
		floor = min(max(floor, 0), len(b))
		var cut [][2]string
		e = ObjectFromEnd(b, floor)
		for e.Next() {
			cut = append(cut, [2]string{string(e.Key()), string(e.Value())})
		}
		if len(cut) > len(fwd) || !slices.Equal(cut, fwd[:len(cut)]) || (!e.Err() && len(cut) != len(fwd)) {
			t.Fatalf("floor %d: %q (err %v), want a prefix of %q", floor, cut, e.Err(), fwd)
		}
	})
}
