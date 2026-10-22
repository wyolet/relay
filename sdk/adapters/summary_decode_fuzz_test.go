package adapters_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// FuzzSummaryDecode checks each shape's narrow decoder against ParseResponse on any body: the same Summary whenever ParseResponse succeeds, and a rejection whenever the body is not JSON. Only a body ParseResponse rejects for mistyping a member the Summary does not read may still yield a Summary.
func FuzzSummaryDecode(f *testing.F) {
	paths, err := filepath.Glob(filepath.Join("testdata", "*", "*", responseInput))
	if err != nil {
		f.Fatal(err)
	}
	for _, path := range paths {
		b, err := os.ReadFile(path)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	for _, bodies := range summaryDecodeCases {
		for _, b := range bodies {
			f.Add([]byte(b))
		}
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		for _, s := range shapes {
			d, ok := s.tr.(v1.SummaryDecoder)
			if !ok {
				t.Fatalf("%s has no narrow decoder", s.name)
			}
			got, gerr := d.DecodeSummary(body)
			resp, perr := s.tr.ParseResponse(body)
			switch {
			case perr == nil:
				want := v1.Summary{Tokens: resp.Usage, FinishReason: resp.FinishReason, ServiceTier: resp.ServiceTier}
				if gerr != nil || !reflect.DeepEqual(got, want) {
					t.Errorf("%s: narrow %+v (%v), ParseResponse %+v", s.name, got, gerr, want)
				}
			case !json.Valid(body):
				var syntax *json.SyntaxError
				if gerr == nil || !errors.As(gerr, &syntax) {
					t.Errorf("%s: narrow accepted a body that is not JSON: %+v (%v)", s.name, got, gerr)
				}
			}
		}
	})
}
