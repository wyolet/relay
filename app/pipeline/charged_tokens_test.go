package pipeline

import (
	"context"
	"maps"
	"net/http"
	"testing"
	"time"

	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/pkg/lifecycle"
	pkgusage "github.com/wyolet/relay/sdk/usage"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// tokensAdapter summarizes every body to sum and extracts extracted from it, counting the extractions.
type tokensAdapter struct {
	sum       v1.Summary
	extracted pkgusage.Tokens
	extracts  int
}

func (*tokensAdapter) Call(context.Context, string, *string, string, []byte, http.Header, string, bool, bool) (*http.Response, error) {
	return nil, nil
}

func (a *tokensAdapter) ExtractTokens([]byte) pkgusage.Tokens {
	a.extracts++
	return a.extracted
}

func (*tokensAdapter) Retryable(*http.Response) (bool, keypool.FailureKind, time.Duration) {
	return false, 0, 0
}

func (a *tokensAdapter) SummarizeResponse([]byte) (v1.Summary, bool) { return a.sum, true }

// The commit charges the Summary's tokens, and asks ExtractTokens only when the Summary has none, as for a stream that ended on an error event after reporting usage.
func TestChargedTokens(t *testing.T) {
	reported := pkgusage.Tokens{"input": 12, "output": 7}
	cases := []struct {
		name         string
		sum          v1.Summary
		want         pkgusage.Tokens
		wantExtracts int
	}{
		{"summary tokens", v1.Summary{Tokens: pkgusage.Tokens{"input": 3}}, pkgusage.Tokens{"input": 3}, 0},
		{"summary without tokens", v1.Summary{FinishReason: v1.FinishReasonStop}, reported, 1},
	}
	for _, tc := range cases {
		a := &tokensAdapter{sum: tc.sum, extracted: reported}
		got := chargedTokens(lifecycle.NewContext("req", "pipeline", time.Now()), a, []byte("event: error\ndata: {}\n\n"))
		if !maps.Equal(got, tc.want) || a.extracts != tc.wantExtracts {
			t.Errorf("%s: charged %v with %d extractions, want %v with %d", tc.name, got, a.extracts, tc.want, tc.wantExtracts)
		}
	}
	if got := chargedTokens(lifecycle.NewContext("req", "pipeline", time.Now()), &tokensAdapter{extracted: reported}, nil); len(got) != 0 {
		t.Errorf("no body: charged %v, want nothing", got)
	}
}
