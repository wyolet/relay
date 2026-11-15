package client

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestParseHeaderEnv(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want map[string]string
		err  string
	}{
		{name: "empty", in: ""},
		{name: "blank", in: "  \t "},
		{name: "one pair", in: "X-A=1", want: map[string]string{"X-A": "1"}},
		{name: "pairs with spaces around names and values", in: " X-A = 1 , X-B=two words ", want: map[string]string{"X-A": "1", "X-B": "two words"}},
		{name: "value keeps later equals signs", in: "X-A=b=c", want: map[string]string{"X-A": "b=c"}},
		{name: "empty value", in: "X-A=", want: map[string]string{"X-A": ""}},
		{name: "empty entries are skipped", in: "X-A=1,, ,X-B=2,", want: map[string]string{"X-A": "1", "X-B": "2"}},
		{name: "a repeated name keeps the last value", in: "X-A=1,X-A=2", want: map[string]string{"X-A": "2"}},
		{name: "only separators", in: ",,", want: map[string]string{}},
		{name: "entry without an equals sign", in: "X-A=1,X-B", err: `relay client: invalid WR_HEADERS entry "X-B" (want k=v)`},
		{name: "entry without a name", in: " =1", err: `relay client: invalid WR_HEADERS entry " =1" (want k=v)`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseHeaderEnv(tc.in)
			if msg := errText(err); msg != tc.err {
				t.Fatalf("err = %q, want %q", msg, tc.err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("headers (-want +got):\n%s", diff)
			}
		})
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
