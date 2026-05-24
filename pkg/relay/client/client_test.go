package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	v1 "github.com/wyolet/relay/pkg/relay/v1"
)

func sampleReq() *v1.Request {
	return &v1.Request{
		Model:        v1.ModelRefs{"gpt-4o"},
		Instructions: "be concise",
		CacheConfig:  &v1.CacheConfig{Instructions: true},
		Input: []v1.Item{
			&v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.TextPart{Text: "hi"}}},
		},
	}
}

func TestGenerate_SerializesCanonicalAndDecodesResponse(t *testing.T) {
	var gotBody []byte
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		resp := &v1.Response{
			ID: "resp_1", Object: "response", Status: v1.StatusCompleted,
			FinishReason: v1.FinishReasonStop,
			Output: []v1.Item{&v1.Message{
				Role: v1.RoleAssistant, Content: []v1.Part{&v1.OutputTextPart{Text: "hello"}},
			}},
		}
		b, _ := json.Marshal(resp)
		_, _ = w.Write(b)
	}))
	defer srv.Close()

	c := New(srv.URL, "rk-secret")
	resp, err := c.Generate(context.Background(), sampleReq())
	if err != nil {
		t.Fatal(err)
	}

	if gotPath != "/v1/generate" {
		t.Errorf("path: %q", gotPath)
	}
	if gotAuth != "Bearer rk-secret" {
		t.Errorf("auth header: %q", gotAuth)
	}

	// Request wire: model as a bare string, output_mode sync, cache_config present.
	var wire map[string]any
	if err := json.Unmarshal(gotBody, &wire); err != nil {
		t.Fatalf("request body not JSON: %v", err)
	}
	if _, ok := wire["model"].(string); !ok {
		t.Errorf("model should serialize as a string, got %T", wire["model"])
	}
	if wire["output_mode"] != "sync" {
		t.Errorf("output_mode: %v", wire["output_mode"])
	}
	if cc, ok := wire["cache_config"].(map[string]any); !ok || cc["instructions"] != true {
		t.Errorf("cache_config not serialized: %v", wire["cache_config"])
	}

	// Response decode.
	if resp.ID != "resp_1" || resp.FinishReason != v1.FinishReasonStop {
		t.Errorf("response: %+v", resp)
	}
	if len(resp.Output) != 1 {
		t.Fatalf("output items: %d", len(resp.Output))
	}
	if _, ok := resp.Output[0].(*v1.Message); !ok {
		t.Errorf("output[0] type: %T", resp.Output[0])
	}
}

func TestGenerateStream_YieldsCanonicalEvents(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var wire map[string]any
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &wire)
		if wire["output_mode"] != "stream" {
			t.Errorf("server saw output_mode %v, want stream", wire["output_mode"])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, f := range []v1.SSEFrame{
			{Event: v1.EventGenerationCreated, Data: []byte(`{"id":"resp_1"}`)},
			{Event: v1.EventItemDelta, Data: []byte(`{"item_id":"msg_1","kind":"output_text"}`)},
			{Event: v1.EventGenerationCompleted, Data: []byte(`{"id":"resp_1","status":"completed","finish_reason":"stop"}`)},
		} {
			_, _ = w.Write(f.Bytes())
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "rk")
	stream, err := c.GenerateStream(context.Background(), sampleReq())
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	var got []string
	for {
		ev, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, ev.Type)
	}

	want := []string{v1.EventGenerationCreated, v1.EventItemDelta, v1.EventGenerationCompleted}
	if len(got) != len(want) {
		t.Fatalf("events: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event[%d]: got %q want %q", i, got[i], want[i])
		}
	}
}

func TestGenerate_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"translate_request","message":"input is required"}}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "rk")
	_, err := c.Generate(context.Background(), sampleReq())
	if err == nil {
		t.Fatal("want error")
	}
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("want *APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != 400 || apiErr.Code != "translate_request" {
		t.Errorf("apiErr: %+v", apiErr)
	}
}
