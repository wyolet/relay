package v1

import (
	"bytes"
	"compress/gzip"
	"reflect"
	"testing"
)

// endsReaderTranslator answers from its ends readers as told: settle with ends, decline, or panic.
type endsReaderTranslator struct {
	fakeTranslator
	ends   Summary
	settle bool
	panics bool
	calls  *[]string
}

func (e endsReaderTranslator) read(framing string) (Summary, bool) {
	*e.calls = append(*e.calls, framing)
	if e.panics {
		panic("ends reader bug")
	}
	return e.ends, e.settle
}

func (e endsReaderTranslator) SummaryFromJSONEnds([]byte) (Summary, bool)   { return e.read("json") }
func (e endsReaderTranslator) SummaryFromStreamEnds([]byte) (Summary, bool) { return e.read("stream") }

func TestExtractSummaryEndsFirst(t *testing.T) {
	jsonBody := []byte(`{"usage":{"input":1},"finish_reason":"length"}`)
	full := Summary{Tokens: map[string]int64{"input": 1}, FinishReason: FinishReasonLength}
	ends := Summary{Tokens: map[string]int64{"input": 9}, FinishReason: FinishReasonStop}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write(jsonBody)
	_ = zw.Close()

	cases := []struct {
		name         string
		tr           func(calls *[]string) Translator
		body         []byte
		want         Summary
		wantFellBack bool
		wantCalls    []string
	}{
		{"no ends reader", func(*[]string) Translator { return fakeTranslator{} }, jsonBody, full, false, nil},
		{"settled", func(c *[]string) Translator { return endsReaderTranslator{ends: ends, settle: true, calls: c} }, jsonBody, ends, false, []string{"json"}},
		{"declined", func(c *[]string) Translator { return endsReaderTranslator{ends: ends, calls: c} }, jsonBody, full, true, []string{"json"}},
		{"panicked", func(c *[]string) Translator { return endsReaderTranslator{panics: true, calls: c} }, jsonBody, full, true, []string{"json"}},
		{"gzip", func(c *[]string) Translator { return endsReaderTranslator{ends: ends, settle: true, calls: c} }, gz.Bytes(), ends, false, []string{"json"}},
		{"stream", func(c *[]string) Translator { return endsReaderTranslator{ends: ends, settle: true, calls: c} }, []byte("event: x\ndata: {}\n\n"), ends, false, []string{"stream"}},
		{"empty body", func(c *[]string) Translator { return endsReaderTranslator{ends: ends, settle: true, calls: c} }, nil, Summary{}, false, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			got, fellBack, err := ExtractSummaryEndsFirst(tc.tr(&calls), tc.body)
			if err != nil || !reflect.DeepEqual(got, tc.want) || fellBack != tc.wantFellBack || !reflect.DeepEqual(calls, tc.wantCalls) {
				t.Errorf("got %+v fellBack=%v err=%v calls=%v; want %+v fellBack=%v calls=%v", got, fellBack, err, calls, tc.want, tc.wantFellBack, tc.wantCalls)
			}
		})
	}
}
