package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/wyolet/relay/app/pipeline"
	pkgusage "github.com/wyolet/relay/sdk/usage"
)

// TokenCounter is the optional capability of a pipeline.Adapter whose upstream counts a request's input tokens exactly. Callers type-assert for it and fall back to their own estimate when the assertion fails — so it stays off pipeline.Adapter itself, which every shape must implement.
type TokenCounter interface {
	// CountAdapter returns the pipeline.Adapter whose Call posts to the upstream's counting endpoint, so a count runs through Pipeline.Run and is reserved, key-selected and failed over exactly like a generation.
	CountAdapter() pipeline.Adapter
}

// MaxCountBody bounds the counting endpoint's response read. The reply is a single small JSON object, so anything larger is an error page or a hostile upstream and must not be buffered whole.
const MaxCountBody = 8 << 10

// countingSpecAdapter is the pipeline.Adapter of a spec that declares a CountPath.
type countingSpecAdapter struct {
	*specAdapter
}

var (
	_ pipeline.Adapter = (*countingSpecAdapter)(nil)
	_ TokenCounter     = (*countingSpecAdapter)(nil)
	_ pipeline.Adapter = (*countCallAdapter)(nil)
)

func (a *countingSpecAdapter) CountAdapter() pipeline.Adapter {
	return &countCallAdapter{specAdapter: a.specAdapter}
}

// countCallAdapter posts to the spec's CountPath. The host path override and the per-model URL builder both name the generation endpoint, so neither applies here.
type countCallAdapter struct {
	*specAdapter
}

func (a *countCallAdapter) Call(ctx context.Context, baseURL string, _ *string, apiKey string, body []byte, hdr http.Header, _ string, _, oauth bool) (*http.Response, error) {
	callCtx, cancel := a.callContext(ctx, false)
	req, err := a.newRequest(callCtx, baseURL+a.spec.CountPath, apiKey, body, hdr, oauth)
	if err != nil {
		cancel()
		return nil, err
	}
	resp, err := a.spec.client.Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	resp.Body = a.bindBody(resp.Body, cancel, false)
	return resp, nil
}

// ExtractTokens reports nothing: counting consumes no tokens, so the caller's token meters are not charged for it.
func (*countCallAdapter) ExtractTokens([]byte) pkgusage.Tokens { return nil }

// ParseTokenCount reads the counting endpoint's reply. A non-200 status or a reply without a positive count is an error — never a number the caller would take for a measurement.
func ParseTokenCount(status int, raw []byte) (int, error) {
	if status != http.StatusOK {
		return 0, fmt.Errorf("adapter: count_tokens upstream returned %d: %s", status, strings.TrimSpace(string(raw)))
	}
	var out struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return 0, fmt.Errorf("adapter: count_tokens response: %w", err)
	}
	if out.InputTokens <= 0 {
		return 0, fmt.Errorf("adapter: count_tokens response carried no input_tokens")
	}
	return out.InputTokens, nil
}
