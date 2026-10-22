package v1

import (
	"errors"
	"reflect"
	"testing"
)

// decoderTranslator answers DecodeSummary with sum, or err.
type decoderTranslator struct {
	fakeTranslator
	sum Summary
	err error
}

func (d decoderTranslator) DecodeSummary([]byte) (Summary, error) { return d.sum, d.err }

// ExtractSummary reads a sync body through the shape's SummaryDecoder instead of ParseResponse, treats its error as a body without a Summary, and leaves streams to the stream walk.
func TestExtractSummaryUsesSummaryDecoder(t *testing.T) {
	decoded := Summary{FinishReason: FinishReasonToolCalls, ServiceTier: "flex"}
	body := []byte(`{"usage":{"input":1},"finish_reason":"length"}`)
	if got, err := ExtractSummary(decoderTranslator{sum: decoded}, body); err != nil || !reflect.DeepEqual(got, decoded) {
		t.Errorf("sync body: %+v (%v), want the decoder's %+v", got, err, decoded)
	}
	if got, err := ExtractSummary(decoderTranslator{sum: decoded, err: errors.New("bad")}, body); err != nil || !reflect.DeepEqual(got, Summary{}) {
		t.Errorf("decoder error: %+v (%v), want an empty Summary", got, err)
	}
	stream := []byte("event: generation.completed\ndata: {\"usage\":{\"output\":4},\"finish_reason\":\"stop\"}\n\n")
	if got, _ := ExtractSummary(decoderTranslator{sum: decoded}, stream); got.Tokens["output"] != 4 {
		t.Errorf("stream: %+v, want the stream walk's Summary", got)
	}
}
