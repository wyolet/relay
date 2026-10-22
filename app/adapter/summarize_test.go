package adapter

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/pipeline"
	"github.com/wyolet/relay/pkg/metrics"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// endsTranslator is a translator whose ends reader settles or declines on demand; its full decode answers differently, so the test can tell which ran.
type endsTranslator struct {
	v1.Translator
	settle bool
}

func (e endsTranslator) SummaryFromJSONEnds([]byte) (v1.Summary, bool) {
	return v1.Summary{FinishReason: v1.FinishReasonStop}, e.settle
}

func (endsTranslator) ParseResponse([]byte) (*v1.Response, error) {
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

// SummarizeResponse takes the ends reader's Summary when it settles, decodes in full and counts the fallback under the spec's name when it declines, and reads nothing without a translator.
func TestSummarizeResponseCountsFallbacks(t *testing.T) {
	name := adapters.Name("ends-test")
	fallbacks := func() float64 { return testutil.ToFloat64(metrics.UsageExtractFallbacks.WithLabelValues(string(name))) }
	body := []byte(`{"usage":{}}`)

	settled := summarizer(t, (&Spec{Name: name, Translator: endsTranslator{settle: true}}).Build().PipelineAdapter())
	before := fallbacks()
	if sum, ok := settled.SummarizeResponse(body); !ok || sum.FinishReason != v1.FinishReasonStop || fallbacks() != before {
		t.Errorf("settling reader: %+v ok=%v, fallbacks +%v; want the ends Summary and no fallback", sum, ok, fallbacks()-before)
	}

	declined := summarizer(t, (&Spec{Name: name, Translator: endsTranslator{settle: false}}).Build().PipelineAdapter())
	if sum, ok := declined.SummarizeResponse(body); !ok || sum.FinishReason != v1.FinishReasonLength || fallbacks() != before+1 {
		t.Errorf("declining reader: %+v ok=%v, fallbacks +%v; want the full decode and one fallback", sum, ok, fallbacks()-before)
	}

	if _, ok := summarizer(t, (&Spec{Name: name}).Build().PipelineAdapter()).SummarizeResponse(body); ok {
		t.Error("a spec without a translator summarized a response")
	}
}

// A count reply is not a generation: its adapter reads no Summary, so post-flight charges no tokens for it.
func TestCountAdapterDoesNotSummarize(t *testing.T) {
	spec := (&Spec{Name: "ends-test", CountPath: "/count", Translator: endsTranslator{settle: true}}).Build()
	counter, ok := spec.PipelineAdapter().(TokenCounter)
	if !ok {
		t.Fatal("a spec with CountPath is not a TokenCounter")
	}
	if _, ok := summarizer(t, counter.CountAdapter()).SummarizeResponse([]byte(`{"input_tokens":3}`)); ok {
		t.Error("the count adapter summarized its reply")
	}
}
