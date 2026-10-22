package proxy

import (
	"context"
	"io"
	"maps"
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
	tokens    sdkusage.Tokens
	summaries int
	extracts  int
}

func (s *summarizingExtractor) ExtractTokens([]byte) sdkusage.Tokens {
	s.extracts++
	return s.tokens
}

func (s *summarizingExtractor) SummarizeResponse([]byte) (v1.Summary, bool) {
	s.summaries++
	return s.sum, true
}

// The commit charges the Summary's tokens, and asks ExtractTokens only when the Summary has none, as for a stream that ended on an error event after reporting usage.
func TestChargedTokens(t *testing.T) {
	reported := sdkusage.Tokens{"input": 12, "output": 7}
	for _, tc := range []struct {
		name         string
		sum          v1.Summary
		want         sdkusage.Tokens
		wantExtracts int
	}{
		{"summary tokens", v1.Summary{Tokens: sdkusage.Tokens{"input": 3}}, sdkusage.Tokens{"input": 3}, 0},
		{"summary without tokens", v1.Summary{FinishReason: v1.FinishReasonStop}, reported, 1},
	} {
		ex := &summarizingExtractor{sum: tc.sum, tokens: reported}
		got := chargedTokens(lifecycle.NewContext("req", "proxy", time.Now()), ex, []byte("event: error\ndata: {}\n\n"))
		if !maps.Equal(got, tc.want) || ex.extracts != tc.wantExtracts {
			t.Errorf("%s: charged %v with %d extractions, want %v with %d", tc.name, got, ex.extracts, tc.want, tc.wantExtracts)
		}
	}
	if got := chargedTokens(lifecycle.NewContext("req", "proxy", time.Now()), nil, []byte("{}")); len(got) != 0 {
		t.Errorf("no extractor: charged %v, want nothing", got)
	}
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
