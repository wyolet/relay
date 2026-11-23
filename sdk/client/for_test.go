package client

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/wyolet/relay/sdk/catalog"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// sentModel is the model member of a recorded request body.
func sentModel(t *testing.T, body string) string {
	t.Helper()
	var wire struct {
		Model *string `json:"model"`
	}
	if err := json.Unmarshal([]byte(body), &wire); err != nil || wire.Model == nil {
		t.Fatalf("request body has no model member (%v): %s", err, body)
	}
	return *wire.Model
}

// A catalog-resolved client sends the binding's upstream name on the binding's wire shape and prices the answer from the binding.
func TestForFrom(t *testing.T) {
	tests := []struct {
		name      string
		ref       string
		req       *v1.Request
		opts      []TargetOption
		path      string
		header    string
		wantValue string
		model     string
		cost      float64
		priced    bool
	}{
		{name: "upstream name replaces the caller's model", ref: "gpt-4o@openai-direct", req: sampleReq(),
			path: "/v1/chat/completions", header: "Authorization", wantValue: "Bearer sk-provider", model: "gpt-4o-2024-08-06", cost: 2.5*0.001 + 10*0.0005, priced: true},
		{name: "upstream name fills a request without a model", ref: "gpt-4o", req: &v1.Request{Input: sampleReq().Input},
			path: "/v1/chat/completions", header: "Authorization", wantValue: "Bearer sk-provider", model: "gpt-4o-2024-08-06", cost: 2.5*0.001 + 10*0.0005, priced: true},
		{name: "host without pricing", ref: "llama3@ollama-local", req: sampleReq(),
			path: "/v1/chat/completions", header: "Authorization", wantValue: "Bearer sk-provider", model: "llama3"},
		{name: "WithUpstreamModel", ref: "gpt-4o", req: sampleReq(), opts: []TargetOption{WithUpstreamModel("my-deployment")},
			path: "/v1/chat/completions", header: "Authorization", wantValue: "Bearer sk-provider", model: "my-deployment", cost: 2.5*0.001 + 10*0.0005, priced: true},
		{name: "WithClient options reach the client", ref: "gpt-4o", req: sampleReq(), opts: []TargetOption{WithClient(WithHeader("X-Test", "yes"))},
			path: "/v1/chat/completions", header: "X-Test", wantValue: "yes", model: "gpt-4o-2024-08-06", cost: 2.5*0.001 + 10*0.0005, priced: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := newRecorder(t, http.StatusOK, ccReply)
			before, _ := json.Marshal(tc.req)
			resp, err := catalogClient(t, tc.ref, rec.srv.URL, tc.opts...).Generate(context.Background(), tc.req)
			if err != nil {
				t.Fatal(err)
			}
			if after, _ := json.Marshal(tc.req); string(after) != string(before) {
				t.Errorf("the caller's request was changed:\n before %s\n after  %s", before, after)
			}
			got := rec.only(t)
			if got.Path != tc.path || got.Header.Get(tc.header) != tc.wantValue {
				t.Errorf("sent to %q with %s %q; want %q with %q", got.Path, tc.header, got.Header.Get(tc.header), tc.path, tc.wantValue)
			}
			if model := sentModel(t, got.Body); model != tc.model {
				t.Errorf("model sent = %q, want %q", model, tc.model)
			}
			if cost, ok := resp.Cost(); cost != tc.cost || ok != tc.priced {
				t.Errorf("Cost = %v, %v; want %v, %v", cost, ok, tc.cost, tc.priced)
			}
		})
	}
}

func TestForFrom_WithAdapterName(t *testing.T) {
	rec := newRecorder(t, http.StatusNoContent, "")
	_, _ = catalogClient(t, "gpt-4o", rec.srv.URL, WithAdapterName("anthropic")).Generate(context.Background(), sampleReq())
	got := rec.only(t)
	if got.Path != "/v1/messages" || got.Header.Get("X-Api-Key") != "sk-provider" || got.Header.Get("Anthropic-Version") != "2023-06-01" || got.Header.Get("Authorization") != "" {
		t.Errorf("sent to %q with headers %v, want the anthropic shape's path and auth", got.Path, got.Header)
	}
	if model := sentModel(t, got.Body); model != "gpt-4o-2024-08-06" {
		t.Errorf("model sent = %q, want the binding's upstream name", model)
	}
}

func TestForFrom_Errors(t *testing.T) {
	scoped, err := catalog.LoadBytes([]byte(scopedCatalogJSON))
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := catalog.Index(&catalog.Catalog{Hosts: []catalog.Host{{Name: "h", BaseURL: "https://h.example", Models: []catalog.Binding{
		{Name: "m", MetadataName: "m", Adapter: "cohere"},
	}}}})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		cat  *catalog.IndexedCatalog
		ref  string
		opts []TargetOption
		want string
	}{
		{"binding with an adapter this SDK lacks", foreign, "m", nil, `relay client: unknown adapter "cohere"`},
		{"WithAdapterName for an adapter this SDK lacks", testCatalog(t), "gpt-4o", []TargetOption{WithAdapterName("cohere")}, `relay client: unknown adapter "cohere"`},
		{"host without a base URL", scoped, "big-model-2026@alpha-host", nil,
			`relay client: catalog host "alpha-host" has no base URL (a relay-scoped catalog, or a self-hosted host); use Relay, or pass WithBaseURL`},
		{"an empty WithBaseURL does not satisfy the guard", scoped, "big-model-2026", []TargetOption{WithBaseURL("")},
			`relay client: catalog host "alpha-host" has no base URL (a relay-scoped catalog, or a self-hosted host); use Relay, or pass WithBaseURL`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, err := ForFrom(tc.cat, tc.ref, "sk", tc.opts...)
			if c != nil || errText(err) != tc.want {
				t.Errorf("ForFrom = %v, %q; want nil, %q", c, errText(err), tc.want)
			}
		})
	}

	if c, err := ForFrom(testCatalog(t), "no-such-model", "sk"); c != nil || err == nil {
		t.Errorf("unresolvable ref: %v, %v; want the catalog's error", c, err)
	}
	if c, err := ForFrom(scoped, "big-model-2026@alpha-host", "sk", WithBaseURL("http://gateway.example/")); err != nil || c.baseURL != "http://gateway.example" {
		t.Errorf("WithBaseURL on a host without one: %v, %v", c, err)
	}
}

// The request path follows relay's host semantics: no host path → adapter default, a set path → verbatim ("" appends nothing to BaseURL), and a caller base URL override → adapter default again.
func TestForFrom_HostPath(t *testing.T) {
	ptr := func(s string) *string { return &s }
	rec := newRecorder(t, http.StatusNoContent, "")
	url := rec.srv.URL
	cases := []struct {
		name     string
		adapter  string
		baseURL  string
		hostPath *string
		paths    map[string]string
		opts     []TargetOption
		want     string
	}{
		{"responses default", "openai_responses", url, nil, nil, nil, "/v1/responses"},
		{"host path verbatim", "openai_responses", url, ptr("/backend/responses"), nil, nil, "/backend/responses"},
		{"empty host path appends nothing", "openai", url + "/deploy/chat", ptr(""), nil, nil, "/deploy/chat"},
		{"host path beats the adapter's path function", "gemini", url, ptr("/custom"), nil, nil, "/custom"},
		{"base URL override drops the host path", "openai", "http://unused.example", ptr("/custom"), nil, []TargetOption{WithBaseURL(url)}, "/v1/chat/completions"},
		{"WithPath beats the host path", "openai", url, ptr("/custom"), nil, []TargetOption{WithClient(WithPath("/explicit"))}, "/explicit"},
		{"path for the binding's adapter", "openai", url, nil, map[string]string{"openai": "/v1beta/openai/chat/completions"}, nil, "/v1beta/openai/chat/completions"},
		{"path for another adapter is ignored", "gemini", url, nil, map[string]string{"openai": "/v1beta/openai/chat/completions"}, nil, "/v1beta/models/upstream-name:generateContent"},
		{"path function uses the upstream name", "gemini", url, nil, nil, nil, "/v1beta/models/upstream-name:generateContent"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cat, err := catalog.Index(&catalog.Catalog{Hosts: []catalog.Host{{Name: "h", BaseURL: tc.baseURL, Path: tc.hostPath, Paths: tc.paths, Models: []catalog.Binding{
				{MetadataName: "m", Adapter: tc.adapter, Name: "upstream-name"},
			}}}})
			if err != nil {
				t.Fatal(err)
			}
			c, err := ForFrom(cat, "m", "k", tc.opts...)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = c.Generate(context.Background(), sampleReq())
			rec.mu.Lock()
			defer rec.mu.Unlock()
			if len(rec.reqs) != i+1 || rec.reqs[i].Path != tc.want {
				t.Fatalf("requests %d, last path %q; want path %q", len(rec.reqs), rec.reqs[len(rec.reqs)-1].Path, tc.want)
			}
		})
	}
}

func TestResponse_Cost(t *testing.T) {
	var none *Response
	if cost, ok := none.Cost(); cost != 0 || ok {
		t.Errorf("nil response Cost = %v, %v", cost, ok)
	}
	rec := newRecorder(t, http.StatusOK, ccReply)
	c := catalogClient(t, "gpt-4o", rec.srv.URL)
	resp, err := c.Generate(context.Background(), sampleReq())
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(c.target.binding, resp.binding); diff != "" || !resp.priced {
		t.Errorf("response priced %v with a different binding (-want +got):\n%s", resp.priced, diff)
	}
}
