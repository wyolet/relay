package inference

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/adapter"
	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/pkg/adapters"
	"github.com/wyolet/relay/pkg/clientprofile"
	"github.com/wyolet/relay/pkg/httpheader"
	"github.com/wyolet/relay/pkg/httpmw"
	"github.com/wyolet/relay/pkg/lifecycle"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// stubProfile speaks the anthropic shape and never matches on User-Agent, so tests exercise the prefix mount alone. It records one attribution header and mints model ids with a "stub/" prefix.
type stubProfile struct{}

func (stubProfile) Name() string                 { return "stub" }
func (stubProfile) Shape() string                { return string(adapters.Anthropic) }
func (stubProfile) Match(*http.Request) bool     { return false }
func (stubProfile) AttributionHeaders() []string { return []string{"x-stub-session-id"} }
func (stubProfile) Inbound(model string) string  { return strings.TrimPrefix(model, "stub/") }

// stubMultiShapeProfile speaks both inbound shapes off one prefix, the way a client whose SDK picks the path per wire shape needs.
type stubMultiShapeProfile struct{ stubProfile }

func (stubMultiShapeProfile) Name() string  { return "stubmulti" }
func (stubMultiShapeProfile) Shape() string { return string(adapters.OpenAI) }
func (stubMultiShapeProfile) Shapes() []string {
	return []string{string(adapters.OpenAI), string(adapters.Anthropic)}
}

// streamBoolTranslator names "stream" as its shape's stream selector, as the vendor chat shapes do.
type streamBoolTranslator struct{ stubV1Translator }

func (streamBoolTranslator) RequestKeys() v1.RequestKeys { return v1.RequestKeys{Stream: "stream"} }

// buildPathRegistry is buildTestRegistry's counterpart for mount tests: the anthropic spec carries the inbound path a profile mirrors, and its own overload answer.
func buildPathRegistry() *adapter.Registry {
	return adapter.NewRegistry((&adapter.Spec{
		Name:         adapters.Anthropic,
		InboundPaths: []adapter.InboundPath{{Path: "/anthropic/v1/messages", OperationID: "anthropic_messages", Summary: "Create a message"}},
		DefaultPath:  "/v1/messages",
		Auth:         adapter.AuthStrategy{Header: "x-api-key"},
		Translator:   streamBoolTranslator{},
		Overloaded:   testOverloaded,
	}).Build())
}

// buildTwoShapeRegistry carries one inbound path per shape, so a mirrored prefix has two paths to cover.
func buildTwoShapeRegistry() *adapter.Registry {
	openaiSpec := (&adapter.Spec{
		Name:         adapters.OpenAI,
		InboundPaths: []adapter.InboundPath{{Path: "/openai/v1/chat/completions", OperationID: "openai_chat_completions", Summary: "Create a chat completion"}},
		DefaultPath:  "/v1/chat/completions",
		Auth:         adapter.AuthStrategy{Header: "Authorization", Scheme: "Bearer"},
		Translator:   stubV1Translator{},
	}).Build()
	return adapter.NewRegistry(openaiSpec, buildPathRegistry().Specs()[0])
}

func routePaths(t *testing.T, r chi.Router) []string {
	t.Helper()
	var paths []string
	err := chi.Walk(r, func(_ string, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		paths = append(paths, route)
		return nil
	})
	if err != nil {
		t.Fatalf("walk routes: %v", err)
	}
	sort.Strings(paths)
	return paths
}

// A profile's prefix mirrors the inbound path of every shape it speaks, as the same operation: the mirrored routes stay out of the OpenAPI document. Without profiles the routes are the specs' own.
func TestMountRegistry_Routes(t *testing.T) {
	for _, tc := range []struct {
		name       string
		profile    clientprofile.Profile
		specs      *adapter.Registry
		routes     []string
		documented []string
	}{
		{"no profiles", nil, buildTwoShapeRegistry(),
			[]string{"/anthropic/v1/messages", "/openai/v1/chat/completions"}, []string{"/anthropic/v1/messages", "/openai/v1/chat/completions"}},
		{"profile speaking one of two shapes", stubProfile{}, buildTwoShapeRegistry(),
			[]string{"/anthropic/v1/messages", "/openai/v1/chat/completions", "/stub/v1/messages"}, []string{"/anthropic/v1/messages", "/openai/v1/chat/completions"}},
		{"profile speaking both shapes", stubMultiShapeProfile{}, buildTwoShapeRegistry(),
			[]string{"/anthropic/v1/messages", "/openai/v1/chat/completions", "/stubmulti/v1/chat/completions", "/stubmulti/v1/messages"}, []string{"/anthropic/v1/messages", "/openai/v1/chat/completions"}},
		{"profile speaking a shape with no route", stubMultiShapeProfile{}, buildPathRegistry(),
			[]string{"/anthropic/v1/messages", "/stubmulti/v1/messages"}, []string{"/anthropic/v1/messages"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cat, _ := buildDispatchCatalog(t, "anthropic", adapters.Anthropic)
			d := buildDeps(t, cat)
			if tc.profile != nil {
				d.Profiles = profileRegistry(tc.profile)
			}
			r := chi.NewRouter()
			cfg := huma.DefaultConfig("test", "1")
			cfg.DocsPath, cfg.OpenAPIPath, cfg.SchemasPath = "", "", ""
			api := humachi.New(r, cfg)
			MountRegistry(tc.specs)(api, d, nil)

			if diff := cmp.Diff(tc.routes, routePaths(t, r)); diff != "" {
				t.Errorf("routes (-want +got):\n%s", diff)
			}
			var documented []string
			for path := range api.OpenAPI().Paths {
				documented = append(documented, path)
			}
			sort.Strings(documented)
			if diff := cmp.Diff(tc.documented, documented); diff != "" {
				t.Errorf("OpenAPI paths (-want +got):\n%s", diff)
			}
		})
	}
}

// profileRouter mounts the path registry with stubProfile registered behind the classifier, and reports each request's lifecycle as post-flight saw it.
func profileRouter(t *testing.T) (http.Handler, <-chan *lifecycle.Context) {
	t.Helper()
	cat, _ := buildDispatchCatalog(t, "anthropic", adapters.Anthropic)
	d := buildDeps(t, cat)
	d.Profiles = profileRegistry(stubProfile{})
	seen := make(chan *lifecycle.Context, 1)
	reg := lifecycle.New()
	reg.RegisterHook(lifecycle.HookFunc{HookName: "test", Fn: func(lc *lifecycle.Context, _ *lifecycle.PostFlightEvent) (any, error) {
		seen <- lc
		return nil, nil
	}})
	d.Lifecycle = reg
	r := chi.NewRouter()
	r.Use(ClassifyMiddleware())
	MountRegistry(buildPathRegistry())(humachi.New(r, huma.DefaultConfig("test", "1")), d, nil)
	return r, seen
}

// A request on a profile's prefix reaches the shape's handler carrying the profile: its name and attribution headers on the lifecycle, and the model as the profile rewrites it. The same request on the shape's own path carries none of it.
func TestMountRegistry_ProfilePrefixCarriesTheProfile(t *testing.T) {
	long := strings.Repeat("a", MaxAttributionValueBytes+64)
	for _, tc := range []struct {
		name, path, model, session string
		metadata                   map[string]any
		requested                  string
	}{
		{"attribution header recorded", "/stub/v1/messages", "test-model", "sess-42", map[string]any{"client_ip": "192.0.2.1", "client": "stub", "x-stub-session-id": "sess-42"}, "test-model"},
		{"absent header records nothing", "/stub/v1/messages", "test-model", "", map[string]any{"client_ip": "192.0.2.1", "client": "stub"}, "test-model"},
		{"attribution value capped", "/stub/v1/messages", "test-model", long, map[string]any{"client_ip": "192.0.2.1", "client": "stub", "x-stub-session-id": long[:MaxAttributionValueBytes]}, "test-model"},
		{"minted model prefix stripped", "/stub/v1/messages", "stub/x", "", map[string]any{"client_ip": "192.0.2.1", "client": "stub"}, "x"},
		{"shape path carries no profile", "/anthropic/v1/messages", "stub/x", "sess-42", map[string]any{"client_ip": "192.0.2.1"}, "stub/x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, seen := profileRouter(t)
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(`{"model":"`+tc.model+`","max_tokens":16}`))
			if tc.session != "" {
				req.Header.Set("x-stub-session-id", tc.session)
			}
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			// No credential on the context, so the shape handler refuses: reaching it is what the mirrored route proves.
			wantUnauthenticated(t, rec, "missing relay key")
			lc := <-seen
			if diff := cmp.Diff(tc.metadata, lc.Metadata); diff != "" {
				t.Errorf("lifecycle metadata (-want +got):\n%s", diff)
			}
			if lc.RequestedModel != tc.requested || lc.Source != "pipeline" {
				t.Errorf("requested model %q from source %q, want %q from pipeline", lc.RequestedModel, lc.Source, tc.requested)
			}
		})
	}
}

// A proxy-mode request on the profile prefix is classified from its headers alone: it takes the proxy flow and its refusal, as on the shape's own path.
func TestMountRegistry_ProfilePrefixClassifiesProxyMode(t *testing.T) {
	r, seen := profileRouter(t)
	for _, path := range []string{"/anthropic/v1/messages", "/stub/v1/messages"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"test-model","max_tokens":16}`))
		req.Header.Set(httpheader.HeaderProxyMode, httpheader.ProxyModeValueProxy)
		req.Header.Set(httpheader.HeaderRelayAPIKey, "relay-key")
		req.Header.Set("Authorization", "Bearer upstream-key")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		wantAnswer(t, rec, http.StatusBadRequest, httpapi.APIErrorBody{
			Type: "invalid_request_error", Code: "missing_upstream_host",
			Message: "anonymous proxy traffic requires X-WR-Upstream-Host header naming a configured host",
		}, map[string]string{"Connection": "close"})
		if lc := <-seen; lc.Source != "proxy" {
			t.Errorf("%s: lifecycle source = %q, want proxy", path, lc.Source)
		}
	}
}

type namedProfile struct{ clientprofile.Profile }

func (namedProfile) Name() string { return "named" }

func TestWithRouteProfile_SetOnlyWhenDifferent(t *testing.T) {
	p := namedProfile{clientprofile.Default}
	r := httptest.NewRequest(http.MethodPost, "/named/v1/messages", nil)

	set := withRouteProfile(r, p)
	if set == r || clientprofile.FromContext(set.Context()).Name() != "named" {
		t.Fatalf("route profile not set on a request without one")
	}
	if again := withRouteProfile(set, p); again != set {
		t.Fatalf("route profile set again over the same profile")
	}
}

const duplicateModelBody = `{"model":"test-model","messages":[{"role":"user","content":"hi"}],"model":"other-model"}`

// A body relay cannot route on is refused before routing. Normal mode reads encoding/json's last model while a byte-pass may forward both, and a stream key an upstream could read differently is a stream relay would not meter.
func TestHandleShape_RefusesTheBody(t *testing.T) {
	cat, prin := buildDispatchCatalog(t, "anthropic", adapters.Anthropic)
	d := buildDeps(t, cat)
	spec := buildPathRegistry().Spec(adapters.Anthropic)
	text := func(s string) io.Reader { return strings.NewReader(s) }
	for _, tc := range []struct {
		name      string
		body      io.Reader
		status    int
		code, msg string
	}{
		{"read fails", iotest.ErrReader(io.ErrUnexpectedEOF), 400, "read_body", "unexpected EOF"},
		{"over the body limit", http.MaxBytesReader(httptest.NewRecorder(), io.NopCloser(text(duplicateModelBody)), 8), 413, "request_too_large", "http: request body too large"},
		{"truncated", text(`{"model":`), 400, "parse_error", "invalid JSON: unexpected end of JSON input"},
		{"not an object", text(`[1]`), 400, "parse_error", "invalid JSON: request body must be a JSON object"},
		{"null", text(`null`), 400, "missing_model", "field 'model' is required"},
		{"no model", text(`{"messages":[]}`), 400, "missing_model", "field 'model' is required"},
		{"duplicate model", text(duplicateModelBody), 400, "duplicate_model", "request body has more than one top-level 'model' field"},
		{"duplicate stream in another case", text(`{"model":"test-model","stream":true,"Stream":false}`), 400, "duplicate_stream", "request body has more than one top-level 'stream' field"},
		{"duplicate stream", text(`{"model":"test-model","stream":true,"stream":null}`), 400, "duplicate_stream", "request body has more than one top-level 'stream' field"},
		{"stream key folded", text(`{"model":"test-model","` + longS + `tream":true}`), 400, "invalid_stream_key", `the top-level 'stream' key must be spelled exactly "stream", without other letter case`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := withNormalContext(httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", tc.body), prin)
			w := httptest.NewRecorder()
			handleShape(spec, d, w, r)
			wantAnswer(t, w, tc.status, httpapi.APIErrorBody{Type: "invalid_request_error", Code: tc.code, Message: tc.msg}, nil)
		})
	}
}

func TestReadRequestBody(t *testing.T) {
	const limit = 2 << 20
	short := strings.Repeat("b", 4096)
	large := strings.Repeat("l", maxUpfrontBodyBytes+7)
	failing := errors.New("connection reset")
	for _, tc := range []struct {
		name     string
		body     io.Reader
		declared int64
		want     string
		maxCap   int // 0 = unchecked
		err      error
		tooLarge bool
	}{
		{name: "declared length, one exact allocation", body: strings.NewReader(short), declared: 4096, want: short, maxCap: 4096},
		{name: "declared length ending with its last read", body: iotest.DataErrReader(strings.NewReader(short)), declared: 4096, want: short, maxCap: 4096},
		{name: "declared length past the upfront cap", body: strings.NewReader(large), declared: int64(len(large)), want: large},
		{name: "unknown length", body: strings.NewReader(short), declared: -1, want: short},
		{name: "more bytes than declared are kept", body: strings.NewReader(short), declared: 10, want: short},
		{name: "fewer bytes than declared", body: strings.NewReader("0123456789"), declared: limit, want: "0123456789", maxCap: maxUpfrontBodyBytes, err: io.ErrUnexpectedEOF},
		{name: "read fails mid-body", body: io.MultiReader(strings.NewReader("0123"), iotest.ErrReader(failing)), declared: 10, want: "0123", err: failing},
		{name: "read fails after the declared length", body: io.MultiReader(strings.NewReader("0123"), iotest.ErrReader(failing)), declared: 4, want: "0123", err: failing},
		{name: "over the limit, declared", body: strings.NewReader(strings.Repeat("b", limit+1)), declared: limit + 1, tooLarge: true},
		{name: "over the limit, declared within it", body: strings.NewReader(strings.Repeat("b", limit+1)), declared: limit, tooLarge: true},
		{name: "over the limit, unknown", body: strings.NewReader(strings.Repeat("b", limit+1)), declared: -1, tooLarge: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/v1/generate", nil)
			r.ContentLength = tc.declared
			r.Body = http.MaxBytesReader(httptest.NewRecorder(), io.NopCloser(tc.body), limit)
			got, err := readRequestBody(r, limit)
			if tc.tooLarge {
				if !httpmw.IsBodyTooLargeError(err) {
					t.Fatalf("err = %v, want a body-too-large error", err)
				}
				return
			}
			if !errors.Is(err, tc.err) || string(got) != tc.want {
				t.Fatalf("read %d bytes, err %v; want %d bytes, err %v", len(got), err, len(tc.want), tc.err)
			}
			if tc.maxCap > 0 && cap(got) > tc.maxCap {
				t.Fatalf("allocated %d bytes, want at most %d", cap(got), tc.maxCap)
			}
		})
	}
}
