package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/wyolet/relay/app/pipeline"
)

// TokenCounter is the optional capability of a pipeline.Adapter whose upstream counts a request's input tokens exactly. Callers type-assert for it and fall back to their own estimate when the assertion fails — so it stays off pipeline.Adapter itself, which every shape must implement.
type TokenCounter interface {
	// CountTokens posts body to the upstream's counting endpoint and returns the input-token count it reports. Errors are returned as they happen: a caller that wants an estimate must ask for one, never receive one silently in place of a count.
	CountTokens(ctx context.Context, baseURL, keyValue string, body []byte, headers http.Header, oauth bool) (int, error)
}

// maxCountBody bounds the counting endpoint's response read. The reply is a single small JSON object, so anything larger is an error page or a hostile upstream and must not be buffered whole.
const maxCountBody = 8 << 10

// countingSpecAdapter is the pipeline.Adapter of a spec that declares a CountPath.
type countingSpecAdapter struct {
	*specAdapter
}

var (
	_ pipeline.Adapter = (*countingSpecAdapter)(nil)
	_ TokenCounter     = (*countingSpecAdapter)(nil)
)

func (a *countingSpecAdapter) CountTokens(ctx context.Context, baseURL, keyValue string, body []byte, headers http.Header, oauth bool) (int, error) {
	callCtx, cancel := context.WithTimeout(ctx, a.spec.syncTimeout)
	defer cancel()

	req, err := a.newRequest(callCtx, baseURL+a.spec.CountPath, keyValue, body, headers, oauth)
	if err != nil {
		return 0, err
	}
	resp, err := a.spec.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxCountBody))
	if err != nil {
		return 0, err
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("adapter: count_tokens upstream returned %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
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
