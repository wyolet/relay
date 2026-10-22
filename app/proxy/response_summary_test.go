package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/lifecycle"
	sdkusage "github.com/wyolet/relay/sdk/usage"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// summarizingExtractor reads a Summary and counts how often post-flight asks for tokens the old way.
type summarizingExtractor struct {
	sum       v1.Summary
	summaries int
	extracts  int
}

func (s *summarizingExtractor) ExtractTokens([]byte) sdkusage.Tokens {
	s.extracts++
	return nil
}

func (s *summarizingExtractor) SummarizeResponse([]byte) (v1.Summary, bool) {
	s.summaries++
	return s.sum, true
}

// A summarizing extractor's one read serves the commit and every post-flight hook: ExtractTokens is never called, and the hooks see the Summary on the Context.
func TestPostFlightSummarizesResponseOnce(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":3,"completion_tokens":2}}`))
	}))
	defer upstream.Close()

	seen := make(chan v1.Summary, 1)
	reg := lifecycle.New()
	reg.RegisterHook(lifecycle.HookFunc{HookName: "probe", Fn: func(lc *lifecycle.Context, _ *lifecycle.PostFlightEvent) (any, error) {
		s, ok := lc.ResponseSummary()
		if !ok {
			t.Error("post-flight hook ran without a response Summary on the Context")
		}
		seen <- s
		return nil, nil
	}})
	p := New(nil, reg, nil)
	p.Client = upstream.Client()

	ex := &summarizingExtractor{sum: v1.Summary{Tokens: sdkusage.Tokens{"input": 3, "output": 2}, FinishReason: v1.FinishReasonStop}}
	res, err := p.Run(context.Background(), &Request{
		Method:       http.MethodPost,
		Path:         "/v1/chat/completions",
		Body:         strings.NewReader(`{}`),
		HostBaseURL:  upstream.URL,
		UpstreamAuth: "Bearer upstream-token",
		Extractor:    ex,
		Lifecycle:    lifecycle.NewContext("req-summary", "proxy", time.Now()),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()

	select {
	case got := <-seen:
		if !reflect.DeepEqual(got, ex.sum) {
			t.Errorf("hook saw %+v, want %+v", got, ex.sum)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("post-flight did not run")
	}
	if ex.summaries != 1 || ex.extracts != 0 {
		t.Errorf("SummarizeResponse ran %d times and ExtractTokens %d, want 1 and 0", ex.summaries, ex.extracts)
	}
}
