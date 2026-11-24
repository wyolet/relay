package inference

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/pkg/adapters"
)

const wireName = "global.acme/test-model-v1:0"

const downReply = `{"error":{"message":"down"}}`

// hopCall is one request an upstream received: its path, and the body's model member ("" when the body has none).
type hopCall struct{ Path, Model string }

// hopUpstream answers every request with status and reply, recording each.
func hopUpstream(t *testing.T, status int, reply string) (string, func() []hopCall) {
	t.Helper()
	got := make(chan hopCall, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Model string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		got <- hopCall{r.URL.EscapedPath(), body.Model}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() []hopCall {
		var out []hopCall
		for len(got) > 0 {
			out = append(out, <-got)
		}
		return out
	}
}

// Failover between a direct host and one that names the model its own way sends each hop its own name, whichever fails first.
func TestDispatch_FailoverSendsEachHopItsUpstreamName(t *testing.T) {
	for name, tc := range map[string]struct {
		first, second         string // the bindings' upstreamName
		firstSent, secondSent string
	}{
		"unnamed hop fails, named hop answers": {"", wireName, "test-model", wireName},
		"named hop fails, unnamed hop answers": {wireName, "", wireName, "test-model"},
		"both hops name the same model":        {wireName, wireName, wireName, wireName},
	} {
		t.Run(name, func(t *testing.T) {
			firstURL, firstCalls := hopUpstream(t, http.StatusInternalServerError, downReply)
			secondURL, secondCalls := hopUpstream(t, http.StatusOK, `{"answer":true}`)
			d, pr := buildHopsDeps(t,
				hopFixture{host: "first", url: firstURL, adapter: adapters.OpenAI, upstreamName: tc.first},
				hopFixture{host: "second", url: secondURL, adapter: adapters.OpenAI, upstreamName: tc.second})

			w := dispatch(d, pr, DispatchInput{Inbound: adapters.OpenAI, Body: []byte(`{"model":"test-model","stream":false}`), ModelName: "test-model"})

			if w.Code != http.StatusOK {
				t.Fatalf("status %d: %s", w.Code, w.Body)
			}
			if diff := cmp.Diff([]hopCall{{"/v1/chat/completions", tc.firstSent}}, firstCalls()); diff != "" {
				t.Errorf("first hop (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff([]hopCall{{"/v1/chat/completions", tc.secondSent}}, secondCalls()); diff != "" {
				t.Errorf("second hop (-want +got):\n%s", diff)
			}
		})
	}
}

// A translated request is serialized once; a failover still sends each hop its own name, in the body or, for a shape that names the model in its URL, in the path.
func TestDispatch_CrossShapeFailoverSendsEachHopItsUpstreamName(t *testing.T) {
	const anthropicReply = `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
	const geminiReply = `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1}}`
	for _, tc := range []struct {
		name          string
		adapter       adapters.Name
		reply         string
		upstreamName  string
		first, second hopCall
	}{
		{"model in the body", adapters.Anthropic, anthropicReply, wireName,
			hopCall{"/v1/messages", wireName}, hopCall{"/v1/messages", "test-model"}},
		{"model in the path", adapters.Gemini, geminiReply, "test-model-002",
			hopCall{"/v1beta/models/test-model-002:generateContent", ""}, hopCall{"/v1beta/models/test-model:generateContent", ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			firstURL, firstCalls := hopUpstream(t, http.StatusInternalServerError, downReply)
			secondURL, secondCalls := hopUpstream(t, http.StatusOK, tc.reply)
			d, pr := buildHopsDeps(t,
				hopFixture{host: "first", url: firstURL, adapter: tc.adapter, upstreamName: tc.upstreamName},
				hopFixture{host: "second", url: secondURL, adapter: tc.adapter})
			useSpecs(&d, productionSpecs())
			if tc.adapter == adapters.Gemini {
				useSpecs(&d, pathModelRegistry())
			}

			w := dispatch(d, pr, DispatchInput{Inbound: adapters.OpenAI, Body: []byte(`{"model":"test-model","messages":[{"role":"user","content":"hi"}],"max_tokens":16}`), ModelName: "test-model"})

			if w.Code != http.StatusOK {
				t.Fatalf("status %d: %s", w.Code, w.Body)
			}
			if diff := cmp.Diff([]hopCall{tc.first}, firstCalls()); diff != "" {
				t.Errorf("first hop (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff([]hopCall{tc.second}, secondCalls()); diff != "" {
				t.Errorf("second hop (-want +got):\n%s", diff)
			}
		})
	}
}

// Embeddings keep only the OpenAI-compatible hops, so the body is built for the first of those, not for the route's first hop.
func TestDispatch_EmbeddingsSendTheFirstKeptHopsUpstreamName(t *testing.T) {
	skippedURL, skippedCalls := hopUpstream(t, http.StatusOK, `{}`)
	servingURL, servingCalls := hopUpstream(t, http.StatusOK, `{"data":[]}`)
	d, pr := buildHopsDeps(t,
		hopFixture{host: "messages-only", url: skippedURL, adapter: adapters.Anthropic},
		hopFixture{host: "compatible", url: servingURL, adapter: adapters.OpenAI, upstreamName: wireName})

	w := dispatch(d, pr, DispatchInput{Inbound: adapters.OpenAIEmbeddings, Body: []byte(`{"model":"test-model","input":"hello"}`), ModelName: "test-model"})

	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if got := skippedCalls(); len(got) != 0 {
		t.Errorf("the anthropic hop was called: %+v", got)
	}
	if diff := cmp.Diff([]hopCall{{"/v1/embeddings", wireName}}, servingCalls()); diff != "" {
		t.Errorf("embeddings hop (-want +got):\n%s", diff)
	}
}

// An upstream token count fails over like a generation, each hop counting under its own name.
func TestCountTokens_FailoverSendsEachHopItsUpstreamName(t *testing.T) {
	firstURL, firstCalls := hopUpstream(t, http.StatusInternalServerError, downReply)
	secondURL, secondCalls := hopUpstream(t, http.StatusOK, `{"input_tokens":7}`)
	d, pr := buildHopsDeps(t,
		hopFixture{host: "first", url: firstURL, adapter: adapters.Anthropic, upstreamName: wireName},
		hopFixture{host: "second", url: secondURL, adapter: adapters.Anthropic})
	useSpecs(&d, countingRegistry())

	wantTokenCount(t, countRequest(d, pr, strings.NewReader(countBody), nil), "7", "exact")

	if diff := cmp.Diff([]hopCall{{"/v1/messages/count_tokens", wireName}}, firstCalls()); diff != "" {
		t.Errorf("first hop (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]hopCall{{"/v1/messages/count_tokens", "test-model"}}, secondCalls()); diff != "" {
		t.Errorf("second hop (-want +got):\n%s", diff)
	}
}
