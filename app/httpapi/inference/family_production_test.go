package inference

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/pkg/adapters"
)

const (
	messagesReply = `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
	cloudModel    = "claude-test-1@20250101"
	cloudPrefix   = "/v1/projects/p1/locations/global/publishers/anthropic/models/"
)

var cloudBackend = map[string]string{"project": "p1", "location": "global"}

// wireCall is one request an upstream received: its escaped path, both credential headers, the anthropic-beta lines joined by "|", and the body.
type wireCall struct{ Path, Auth, APIKey, Beta, Body string }

func wireUpstream(t *testing.T, status int, contentType, reply string) (string, func() []wireCall) {
	t.Helper()
	got := make(chan wireCall, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- wireCall{r.URL.EscapedPath(), r.Header.Get("Authorization"), r.Header.Get("X-Api-Key"), strings.Join(r.Header.Values("Anthropic-Beta"), "|"), string(b)}
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() []wireCall {
		var out []wireCall
		for len(got) > 0 {
			out = append(out, <-got)
		}
		return out
	}
}

func dispatchWithHeaders(d Deps, pr *Principal, in DispatchInput, hdr http.Header) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.Header = hdr
	w := httptest.NewRecorder()
	Dispatch(d, w, withNormalContext(r, pr), in)
	return w
}

// wantCloudBody checks body is sent without model and with anthropic_version, every other member of sent byte-identical when sent is given.
func wantCloudBody(t *testing.T, label, body, version string, sent map[string]json.RawMessage) {
	t.Helper()
	got := members(t, body)
	if got["model"] != nil || string(got["anthropic_version"]) != version {
		t.Errorf("%s: body %s, want no model and anthropic_version %s", label, body, version)
	}
	if sent == nil {
		return
	}
	delete(got, "anthropic_version")
	want := map[string]string{}
	for k, v := range sent {
		if k != "model" && k != "anthropic_version" {
			want[k] = string(v)
		}
	}
	have := map[string]string{}
	for k, v := range got {
		have[k] = string(v)
	}
	if diff := cmp.Diff(want, have); diff != "" {
		t.Errorf("%s: members beyond the envelope (-sent +upstream):\n%s", label, diff)
	}
}

// A Messages caller byte-passes to a cloud binding of the anthropic family, any other caller goes through the shared translator; either way the hop gets its own path with the model escaped as one segment (an @ kept), a bearer token alone, and the body in its envelope.
func TestDispatch_CloudMessagesHop(t *testing.T) {
	stream := strings.Join(anthropicMultiDeltaStream, "")
	for _, tc := range []struct {
		name    string
		inbound adapters.Name
		stream  bool
		body    string
	}{
		{"messages caller", adapters.Anthropic, false, `{"model":"test-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"metadata":{"user_id":"u"}}`},
		{"messages caller, streamed", adapters.Anthropic, true, `{"model":"test-model","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`},
		{"chat completions caller", adapters.OpenAI, false, `{"model":"test-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`},
		{"chat completions caller, streamed", adapters.OpenAI, true, `{"model":"test-model","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			contentType, reply, method := "application/json", messagesReply, ":rawPredict"
			if tc.stream {
				contentType, reply, method = "text/event-stream", stream, ":streamRawPredict"
			}
			url, calls := wireUpstream(t, http.StatusOK, contentType, reply)
			d, pr := buildHopsDeps(t, hopFixture{host: "cloud", url: url, adapter: adapters.VertexAnthropic, upstreamName: cloudModel, backend: cloudBackend})
			useSpecs(&d, productionSpecs())

			w := dispatch(d, pr, DispatchInput{Inbound: tc.inbound, Body: []byte(tc.body), ModelName: "test-model", Stream: tc.stream})

			if w.Code != http.StatusOK {
				t.Fatalf("status %d: %s", w.Code, w.Body)
			}
			bytePass := tc.inbound == adapters.Anthropic
			if bytePass && w.Body.String() != reply {
				t.Errorf("answer %q, want the upstream's verbatim", w.Body)
			}
			got := calls()
			if len(got) != 1 {
				t.Fatalf("calls = %+v, want one", got)
			}
			if want := (wireCall{Path: cloudPrefix + cloudModel + method, Auth: "Bearer sk-cloud"}); got[0].Path != want.Path || got[0].Auth != want.Auth || got[0].APIKey != "" {
				t.Errorf("call = %+v, want path %s, bearer auth and no x-api-key", got[0], want.Path)
			}
			var sent map[string]json.RawMessage
			if bytePass {
				sent = members(t, tc.body)
			}
			wantCloudBody(t, "upstream body", got[0].Body, `"vertex-2023-10-16"`, sent)
		})
	}
}

// A route from a direct Messages host to a cloud one fails over within the family, byte-pass and canonical alike, each hop sent its own path, auth and body.
func TestDispatch_CloudMessagesFailover(t *testing.T) {
	for name, inbound := range map[string]adapters.Name{"byte-pass": adapters.Anthropic, "canonical": adapters.OpenAI} {
		t.Run(name, func(t *testing.T) {
			directURL, directCalls := wireUpstream(t, http.StatusInternalServerError, "application/json", downReply)
			cloudURL, cloudCalls := wireUpstream(t, http.StatusOK, "application/json", messagesReply)
			d, pr := buildHopsDeps(t,
				hopFixture{host: "direct", url: directURL, adapter: adapters.Anthropic},
				hopFixture{host: "cloud", url: cloudURL, adapter: adapters.VertexAnthropic, upstreamName: cloudModel, backend: cloudBackend})
			useSpecs(&d, productionSpecs())

			w := dispatch(d, pr, DispatchInput{Inbound: inbound, Body: []byte(`{"model":"test-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`), ModelName: "test-model"})

			if w.Code != http.StatusOK {
				t.Fatalf("status %d: %s", w.Code, w.Body)
			}
			direct, cloud := directCalls(), cloudCalls()
			if len(direct) != 1 || len(cloud) != 1 {
				t.Fatalf("calls: direct %+v, cloud %+v; want one each", direct, cloud)
			}
			if d := direct[0]; d.Path != "/v1/messages" || d.APIKey != "sk-direct" || d.Auth != "" || string(members(t, d.Body)["model"]) != `"test-model"` {
				t.Errorf("direct hop = %+v, want /v1/messages, x-api-key and the model in the body", d)
			}
			if c := cloud[0]; c.Path != cloudPrefix+cloudModel+":rawPredict" || c.Auth != "Bearer sk-cloud" || c.APIKey != "" {
				t.Errorf("cloud hop = %+v, want its own path and bearer auth", c)
			}
			wantCloudBody(t, "cloud body", cloud[0].Body, `"vertex-2023-10-16"`, nil)
		})
	}
}

// The cloud reads one anthropic-beta line, so repeated ones go out joined; a version the caller set in the body is its own and kept.
func TestDispatch_CloudMessagesKeepsCallerChoices(t *testing.T) {
	url, calls := wireUpstream(t, http.StatusOK, "application/json", messagesReply)
	d, pr := buildHopsDeps(t, hopFixture{host: "cloud", url: url, adapter: adapters.VertexAnthropic, upstreamName: cloudModel, backend: cloudBackend})
	useSpecs(&d, productionSpecs())
	body := `{"model":"test-model","anthropic_version":"2026-10-21","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`

	w := dispatchWithHeaders(d, pr, DispatchInput{Inbound: adapters.Anthropic, Body: []byte(body), ModelName: "test-model"}, http.Header{"Anthropic-Beta": {"beta-a", "beta-b"}})

	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	got := calls()
	if len(got) != 1 || got[0].Beta != "beta-a,beta-b" {
		t.Fatalf("calls = %+v, want one with the beta lines joined", got)
	}
	wantCloudBody(t, "upstream body", got[0].Body, `"2026-10-21"`, members(t, body))
}

// A cloud host whose backend leaves a path placeholder unset is a configuration fault, never a request to a malformed URL.
func TestDispatch_CloudMessagesUnfilledProject(t *testing.T) {
	url, calls := wireUpstream(t, http.StatusOK, "application/json", messagesReply)
	d, pr := buildHopsDeps(t, hopFixture{host: "cloud", url: url, adapter: adapters.VertexAnthropic, upstreamName: cloudModel, backend: map[string]string{"location": "global"}})
	useSpecs(&d, productionSpecs())

	w := dispatch(d, pr, DispatchInput{Inbound: adapters.Anthropic, Body: []byte(`{"model":"test-model","max_tokens":16,"messages":[]}`), ModelName: "test-model"})

	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), `"host_misconfigured"`) {
		t.Errorf("answer %d %s, want 502 host_misconfigured", w.Code, w.Body)
	}
	if got := calls(); len(got) != 0 {
		t.Errorf("the upstream was called: %+v", got)
	}
}

// The model rides in the URL, so an alias suffix lands there unless the binding names the upstream model: with upstreamName the bare id goes out, without it the caller's alias string as one escaped segment.
func TestDispatch_CloudMessagesAliasInPath(t *testing.T) {
	for name, tc := range map[string]struct{ upstreamName, want string }{
		"binding names the model": {cloudModel, cloudPrefix + cloudModel + ":rawPredict"},
		"binding names none":      {"", cloudPrefix + "test-modelx%5B1m%5D:rawPredict"},
	} {
		t.Run(name, func(t *testing.T) {
			url, calls := wireUpstream(t, http.StatusOK, "application/json", messagesReply)
			d, pr := buildAliasedHopsDeps(t, []string{"test-modelx[*]"}, hopFixture{host: "cloud", url: url, adapter: adapters.VertexAnthropic, upstreamName: tc.upstreamName, backend: cloudBackend})
			useSpecs(&d, productionSpecs())

			w := dispatch(d, pr, DispatchInput{Inbound: adapters.Anthropic, Body: []byte(`{"model":"test-modelx[1m]","max_tokens":16,"messages":[]}`), ModelName: "test-modelx[1m]"})

			if w.Code != http.StatusOK {
				t.Fatalf("status %d: %s", w.Code, w.Body)
			}
			if got := calls(); len(got) != 1 || got[0].Path != tc.want {
				t.Errorf("calls = %+v, want one to %s", got, tc.want)
			}
		})
	}
}
