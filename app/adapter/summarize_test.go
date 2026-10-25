package adapter

import (
	"bytes"
	"compress/gzip"
	"testing"

	"github.com/wyolet/relay/app/pipeline"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// summaryTranslator answers ParseResponse with a fixed finish reason, or panics.
type summaryTranslator struct {
	v1.Translator
	panics bool
}

func (s summaryTranslator) ParseResponse([]byte) (*v1.Response, error) {
	if s.panics {
		panic("translator bug")
	}
	return &v1.Response{FinishReason: v1.FinishReasonLength}, nil
}

func summarizer(t *testing.T, a pipeline.Adapter) pipeline.ResponseSummarizer {
	t.Helper()
	rs, ok := a.(pipeline.ResponseSummarizer)
	if !ok {
		t.Fatal("the spec adapter does not summarize responses")
	}
	return rs
}

func TestSummarizeResponse(t *testing.T) {
	body := []byte(`{"usage":{}}`)
	if sum, ok := summarizer(t, (&Spec{Name: "summary-test", Translator: summaryTranslator{}}).Build().PipelineAdapter()).SummarizeResponse(body); !ok || sum.FinishReason != v1.FinishReasonLength {
		t.Errorf("got %+v ok=%v, want the translator's Summary", sum, ok)
	}
	if _, ok := summarizer(t, (&Spec{Name: "summary-test"}).Build().PipelineAdapter()).SummarizeResponse(body); ok {
		t.Error("a spec without a translator summarized a response")
	}
	var gz bytes.Buffer
	_, _ = gzip.NewWriter(&gz).Write(body)
	if sum, ok := summarizer(t, (&Spec{Name: "summary-test", Translator: summaryTranslator{}}).Build().PipelineAdapter()).SummarizeResponse(gz.Bytes()[:4]); !ok || sum.FinishReason != "" {
		t.Errorf("an undecompressable body: %+v ok=%v, want an empty Summary", sum, ok)
	}
}

// A translator that panics on an upstream body costs that response its Summary, not the post-flight goroutine.
func TestSummarizeResponseRecoversTranslatorPanic(t *testing.T) {
	a := summarizer(t, (&Spec{Name: "summary-test", Translator: summaryTranslator{panics: true}}).Build().PipelineAdapter())
	if sum, ok := a.SummarizeResponse([]byte(`{}`)); ok || sum.FinishReason != "" {
		t.Errorf("got %+v ok=%v, want no Summary", sum, ok)
	}
}

// A count reply is not a generation: its adapter reads no Summary, so post-flight charges no tokens for it.
func TestCountAdapterDoesNotSummarize(t *testing.T) {
	spec := (&Spec{Name: "summary-test", CountPath: "/count", Translator: summaryTranslator{}}).Build()
	counter, ok := spec.PipelineAdapter().(TokenCounter)
	if !ok {
		t.Fatal("a spec with CountPath is not a TokenCounter")
	}
	if _, ok := summarizer(t, counter.CountAdapter()).SummarizeResponse([]byte(`{"input_tokens":3}`)); ok {
		t.Error("the count adapter summarized its reply")
	}
}
