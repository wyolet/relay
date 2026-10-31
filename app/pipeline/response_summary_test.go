package pipeline_test

import (
	"context"
	"maps"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/pipeline"
	"github.com/wyolet/relay/pkg/lifecycle"
	pkgusage "github.com/wyolet/relay/sdk/usage"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// summarizingAdapter reads a fixed Summary and counts how often post-flight decodes the body the old way.
type summarizingAdapter struct {
	fakeAdapter
	sum       v1.Summary
	summaries atomic.Int32
	extracts  atomic.Int32
}

func (a *summarizingAdapter) ExtractTokens(b []byte) pkgusage.Tokens {
	a.extracts.Add(1)
	return a.fakeAdapter.ExtractTokens(b)
}

func (a *summarizingAdapter) SummarizeResponse([]byte) (v1.Summary, bool) {
	a.summaries.Add(1)
	return a.sum, true
}

// decodeCountingTranslator counts the full decodes a usage hook would run on its own.
type decodeCountingTranslator struct {
	v1.Translator
	decodes *atomic.Int32
}

func (d decodeCountingTranslator) ParseResponse([]byte) (*v1.Response, error) {
	d.decodes.Add(1)
	return &v1.Response{}, nil
}

// A buffered response is decoded once: the adapter's Summary feeds the commit and the usage event, and the usage hook does not decode the body again.
func TestRun_BufferedResponseIsSummarizedOnce(t *testing.T) {
	reg, events := usageRegistry()
	p := newPipeline()
	p.Lifecycle = reg

	var decodes atomic.Int32
	lc := lifecycle.NewContext("req", "pipeline", time.Now())
	lc.Translator = decodeCountingTranslator{decodes: &decodes}
	a := &summarizingAdapter{sum: v1.Summary{Tokens: pkgusage.Tokens{"input": 7, "output": 5}, FinishReason: v1.FinishReasonLength}}

	res, err := p.Run(context.Background(), &pipeline.Request{
		Adapter:   a,
		Hops:      hopOf([]*hostkey.HostKey{makeKey("h", "sk")}),
		Lifecycle: lc,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	drainResult(t, res)

	ev := events.next(t)
	if !maps.Equal(ev.Tokens, a.sum.Tokens) || ev.FinishReason != string(a.sum.FinishReason) {
		t.Errorf("usage event tokens %v finish %q, want %v %q", ev.Tokens, ev.FinishReason, a.sum.Tokens, a.sum.FinishReason)
	}
	if a.summaries.Load() != 1 || a.extracts.Load() != 0 || decodes.Load() != 0 {
		t.Errorf("SummarizeResponse %d, ExtractTokens %d, usage-hook decodes %d; want 1, 0, 0", a.summaries.Load(), a.extracts.Load(), decodes.Load())
	}
}
