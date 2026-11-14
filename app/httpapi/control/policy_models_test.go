package control

import (
	"net/http"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/pkg/modelref"
)

func TestNormalizePolicyRefs(t *testing.T) {
	for _, tc := range []struct {
		name    string
		in      []string
		want    []string
		wantErr string
	}{
		{name: "names are slugified and duplicates collapse", in: []string{"OpenAI/GPT-4o", "openai/gpt-4o", "@Edge", "openai"}, want: []string{"openai/gpt-4o", "@edge", "openai"}},
		{name: "a disabled binding", in: []string{"openai/gpt-4o", "openai/gpt-3-5"}, wantErr: `policy "p": models ref "openai/gpt-3-5" matches no enabled model or host in the catalog`},
		{name: "a disabled model", in: []string{"acme/old"}, wantErr: `policy "p": models ref "acme/old" matches no enabled model or host in the catalog`},
		{name: "a disabled host", in: []string{"@dead-host"}, wantErr: `policy "p": models ref "@dead-host" matches no enabled model or host in the catalog`},
		{name: "a model pattern", in: []string{"openai/gpt-*"}, wantErr: `policy "p": models: modelref "openai/gpt-*": model "gpt-*" contains *: refs take no patterns; leave the model out to match every one`},
		{name: "a host pattern", in: []string{"openai/gpt-4o@clo*"}, wantErr: `policy "p": models: modelref "openai/gpt-4o@clo*": host "clo*" contains *: refs take no patterns; leave the host out to match every one`},
		{name: "a malformed reference", in: []string{"openai/"}, wantErr: `policy "p": models: modelref "openai/": trailing / requires a model`},
	} {
		for name, idx := range resolveIndexes(t) {
			t.Run(tc.name+", "+name, func(t *testing.T) {
				got, err := normalizePolicyRefs(idx, tc.in, "p", "models")
				if tc.wantErr == "" {
					if diff := cmp.Diff(tc.want, got); err != nil || diff != "" {
						t.Fatalf("err = %v (-want +got):\n%s", err, diff)
					}
					return
				}
				if got != nil || statusOf(t, err) != http.StatusBadRequest || err.Error() != tc.wantErr {
					t.Fatalf("got %v, %v; want a 400: %s", got, err, tc.wantErr)
				}
			})
		}
	}
}

func TestRefResolvesEnabled(t *testing.T) {
	for _, tc := range []struct {
		ref  string
		want bool
	}{
		{"openai/gpt-4o", true},
		{"openai/gpt-4o@edge", true},
		{"openai", true},
		{"@cloud", true},
		{"openai/legacy", true}, // deprecated still serves
		{"openai/gpt-3-5", false},
		{"openai/gpt-3-5@cloud", false},
		{"openai/legacy@edge", false},
		{"acme/old", false},
		{"acme", false},
		{"@dead-host", false},
		{"openai/gpt-4o@dead-host", false},
		{"@nowhere", false},
		{"ghost/model", false},
		{"openai/does-not-exist", false},
	} {
		for name, idx := range resolveIndexes(t) {
			if got := refResolvesEnabled(idx, modelref.MustParse(tc.ref)); got != tc.want {
				t.Errorf("%s, %s: %v, want %v", tc.ref, name, got, tc.want)
			}
		}
	}
}
