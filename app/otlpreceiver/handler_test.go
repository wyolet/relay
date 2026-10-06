package otlpreceiver_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/wyolet/relay/app/binding"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/httpapi/inference"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/otlpreceiver"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/app/usagelog"
	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/otlp"
	"github.com/wyolet/relay/pkg/otlp/genai"
	"github.com/wyolet/relay/pkg/payload"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
	"github.com/wyolet/relay/pkg/slug"
)

// relayKey is the reporter every export uses unless a test names another. siblingKey belongs to a second service account of the same project, outsiderKey to another project.
const (
	relayKey    = "sk-wr-reporter"
	siblingKey  = "sk-wr-sibling"
	outsiderKey = "sk-wr-outsider"
)

func keyHash(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

type rows[T any] []*T

func (r rows[T]) List(context.Context) ([]*T, error) { return r, nil }

// fixture is a receiver in front of a catalog holding one model served by two hosts: the provider's own (priced) and a reseller (priced higher), plus one model name that two providers both have. The fixture is the receiver's usage queue: events holds what was queued, and emptying it stands in for the queue draining.
type fixture struct {
	handler  http.Handler
	enabled  bool
	events   []usagelog.Event
	capacity int
	// phantomRoom is added to the room the queue reports, like emits from other requests landing between the receiver's check and its own.
	phantomRoom int
	// captureContent is the receiver's own content switch; payloads is the payload store behind it.
	captureContent bool
	// usageRetention and contentRetention are how long the stores keep a record; zero keeps everything. Read per export, like the settings behind them.
	usageRetention   time.Duration
	contentRetention time.Duration
	payloads         *payloadStore
	project          *project.Project
	// otherProject is the project of outsiderKey.
	otherProject *project.Project
	team         *team.Team
	sa           *serviceaccount.ServiceAccount
	keyRow       *key.Key
	model        *model.Model
	ownPrice     *pricing.Pricing
	// sharedAcme and sharedZeta are two providers' models with the same snapshot name.
	sharedAcme *model.Model
	sharedZeta *model.Model
}

func (fx *fixture) TryEmit(ev usagelog.Event) bool {
	if len(fx.events) >= fx.capacity {
		return false
	}
	fx.events = append(fx.events, ev)
	return true
}

func (fx *fixture) Free() int { return fx.capacity - len(fx.events) + fx.phantomRoom }

func (fx *fixture) Capacity() int { return fx.capacity }

// payloadStore stands in for the payload controller and its queue: records holds what was queued.
type payloadStore struct {
	enabled  bool
	maxBytes int
	records  []payload.Record
	capacity int
}

func (p *payloadStore) Enabled() bool { return p.enabled }

func (p *payloadStore) MaxBytes() int { return p.maxBytes }

func (p *payloadStore) TryEmit(r payload.Record) bool {
	if len(p.records) >= p.capacity {
		return false
	}
	p.records = append(p.records, r)
	return true
}

func (p *payloadStore) Free() int { return p.capacity - len(p.records) }

func (p *payloadStore) Capacity() int { return p.capacity }

type fixtureOptions struct {
	// exportsPerMinute above zero adds the system rate limit on exports with that budget.
	exportsPerMinute int64
	// rateLimitDisabled switches that rate limit row off.
	rateLimitDisabled bool
	// markerStore replaces the in-memory store behind the duplicate markers.
	markerStore kv.Scripter
	// reasoningRate above zero adds a reasoning meter at that rate (USD per million) to the model's own rate sheet.
	reasoningRate float64
	// policyCaptures, when set, puts the reporting key under a policy whose payload logging flag has that value. Nil leaves the key with no policy.
	policyCaptures *bool
	// policyDisabled switches that policy off.
	policyDisabled bool
	// policyModels is that policy's model grant; empty grants every model.
	policyModels []string
	// keyCaptures sets the reporting key's own payload logging flag.
	keyCaptures bool
}

func system() meta.Owner { return meta.Owner{Kind: meta.OwnerSystem} }

func perMillion(m pricing.Meter, usd float64) pricing.Rate {
	return pricing.Rate{Meter: m, Unit: pricing.UnitPerMillion, Amount: usd}
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	return newFixtureWith(t, fixtureOptions{})
}

func newFixtureWith(t *testing.T, o fixtureOptions) *fixture {
	t.Helper()
	fx := &fixture{enabled: true, capacity: usagelog.DefaultQueueSize, payloads: &payloadStore{capacity: 256}}

	fx.team = &team.Team{Meta: meta.Metadata{ID: meta.NewID(), Name: "platform", Owner: system()}}
	fx.project = &project.Project{Meta: meta.Metadata{ID: meta.NewID(), Name: "ml-search"}, Spec: project.Spec{TeamID: fx.team.Meta.ID}}
	fx.project.StampOwner()
	// No policy anywhere on this principal: reporting needs a credential, not a route.
	fx.sa = &serviceaccount.ServiceAccount{Meta: meta.Metadata{ID: meta.NewID(), Name: "indexer"}, Spec: serviceaccount.Spec{ProjectID: fx.project.Meta.ID}}
	fx.sa.StampOwner()
	fx.keyRow = &key.Key{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "indexer-prod", Owner: meta.Owner{Kind: meta.OwnerProject, ID: fx.project.Meta.ID}},
		Spec: key.Spec{Principal: key.Principal{Kind: key.PrincipalServiceAccount, ID: fx.sa.Meta.ID}, KeyHash: keyHash(relayKey)},
	}
	fx.keyRow.Spec.PayloadLoggingEnabled = o.keyCaptures
	var policies rows[policy.Policy]
	if o.policyCaptures != nil {
		enabled := !o.policyDisabled
		governing := &policy.Policy{
			Meta: meta.Metadata{ID: meta.NewID(), Name: "indexer-policy", Owner: meta.Owner{Kind: meta.OwnerProject, ID: fx.project.Meta.ID}},
			Spec: policy.Spec{PayloadLoggingEnabled: *o.policyCaptures, Enabled: &enabled, Models: o.policyModels},
		}
		policies = append(policies, governing)
		fx.keyRow.Spec.PolicyID = governing.Meta.ID
	}
	sibling := &serviceaccount.ServiceAccount{Meta: meta.Metadata{ID: meta.NewID(), Name: "crawler"}, Spec: serviceaccount.Spec{ProjectID: fx.project.Meta.ID}}
	sibling.StampOwner()
	siblingRow := &key.Key{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "crawler-prod", Owner: meta.Owner{Kind: meta.OwnerProject, ID: fx.project.Meta.ID}},
		Spec: key.Spec{Principal: key.Principal{Kind: key.PrincipalServiceAccount, ID: sibling.Meta.ID}, KeyHash: keyHash(siblingKey)},
	}
	fx.otherProject = &project.Project{Meta: meta.Metadata{ID: meta.NewID(), Name: "ml-ads"}, Spec: project.Spec{TeamID: fx.team.Meta.ID}}
	fx.otherProject.StampOwner()
	outsider := &serviceaccount.ServiceAccount{Meta: meta.Metadata{ID: meta.NewID(), Name: "bidder"}, Spec: serviceaccount.Spec{ProjectID: fx.otherProject.Meta.ID}}
	outsider.StampOwner()
	outsiderRow := &key.Key{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "bidder-prod", Owner: meta.Owner{Kind: meta.OwnerProject, ID: fx.otherProject.Meta.ID}},
		Spec: key.Spec{Principal: key.Principal{Kind: key.PrincipalServiceAccount, ID: outsider.Meta.ID}, KeyHash: keyHash(outsiderKey)},
	}

	prov := &provider.Provider{Meta: meta.Metadata{ID: meta.NewID(), Name: "acme", Owner: system()}}
	ownHost := &host.Host{Meta: meta.Metadata{ID: meta.NewID(), Name: "acme", Owner: system()}, Spec: host.Spec{BaseURL: "https://api.acme.example"}}
	reseller := &host.Host{Meta: meta.Metadata{ID: meta.NewID(), Name: "a-reseller", Owner: system()}, Spec: host.Spec{BaseURL: "https://reseller.example"}}
	fx.model = &model.Model{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "acme-large", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: prov.Meta.ID}},
		Spec: model.Spec{Snapshots: []model.Snapshot{{Name: slug.From("acme-large")}}, Pointer: slug.From("acme-large")},
	}
	fx.ownPrice = &pricing.Pricing{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "acme-large-list", Owner: meta.Owner{Kind: meta.OwnerHost, ID: ownHost.Meta.ID}},
		Spec: pricing.Spec{Currency: "USD", TargetModelIDs: []string{fx.model.Meta.ID}, Rates: []pricing.Rate{
			perMillion(pricing.MeterTokensInput, 3),
			perMillion(pricing.MeterTokensOutput, 15),
			perMillion(pricing.MeterTokensCacheRead, 0.3),
			perMillion(pricing.MeterTokensCacheCreation, 3.75),
		}},
	}
	if o.reasoningRate > 0 {
		fx.ownPrice.Spec.Rates = append(fx.ownPrice.Spec.Rates, perMillion(pricing.MeterTokensReasoning, o.reasoningRate))
	}
	resellerPrice := &pricing.Pricing{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "reseller-markup", Owner: meta.Owner{Kind: meta.OwnerHost, ID: reseller.Meta.ID}},
		Spec: pricing.Spec{Currency: "USD", TargetModelIDs: []string{fx.model.Meta.ID}, Rates: []pricing.Rate{
			perMillion(pricing.MeterTokensInput, 30),
			perMillion(pricing.MeterTokensOutput, 150),
		}},
	}
	bindings := rows[binding.Binding]{
		// Sorted first by name, so only the provider-host preference keeps it from winning.
		{Meta: meta.Metadata{ID: meta.NewID(), Name: "a-reseller-acme-large", Owner: system()}, Spec: binding.Spec{ModelID: fx.model.Meta.ID, HostID: reseller.Meta.ID, Adapter: "openai", PricingID: resellerPrice.Meta.ID}},
		{Meta: meta.Metadata{ID: meta.NewID(), Name: "acme-acme-large", Owner: system()}, Spec: binding.Spec{ModelID: fx.model.Meta.ID, HostID: ownHost.Meta.ID, Adapter: "openai", PricingID: fx.ownPrice.Meta.ID}},
	}

	zeta := &provider.Provider{Meta: meta.Metadata{ID: meta.NewID(), Name: "zeta", Owner: system()}}
	fx.sharedAcme = &model.Model{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "acme-shared", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: prov.Meta.ID}},
		Spec: model.Spec{Snapshots: []model.Snapshot{{Name: "shared"}}, Pointer: "shared"},
	}
	fx.sharedZeta = &model.Model{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "zeta-shared", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: zeta.Meta.ID}},
		Spec: model.Spec{Snapshots: []model.Snapshot{{Name: "shared"}}, Pointer: "shared"},
	}

	var limits rows[ratelimit.RateLimit]
	if o.exportsPerMinute > 0 {
		enabled := !o.rateLimitDisabled
		limits = append(limits, &ratelimit.RateLimit{
			Meta: meta.Metadata{ID: meta.NewID(), Name: otlpreceiver.RateLimitName, Owner: system()},
			Spec: ratelimit.Spec{Enabled: &enabled, Rules: []ratelimit.Rule{
				{Meter: ratelimit.MeterRequests, Amount: o.exportsPerMinute, Window: ratelimit.Window(time.Minute), Strategy: ratelimit.StrategySlidingWindow},
				// Not a requests rule, so it must not apply: nothing would ever release the slot.
				{Meter: ratelimit.MeterConcurrency, Amount: 1, Window: ratelimit.Window(time.Minute), Strategy: ratelimit.StrategySlidingWindow},
			}},
		})
	}

	cat := appcatalog.New(
		rows[provider.Provider]{prov, zeta},
		rows[host.Host]{ownHost, reseller},
		policies,
		rows[model.Model]{fx.model, fx.sharedAcme, fx.sharedZeta},
		rows[hostkey.HostKey]{},
		limits,
		rows[key.Key]{fx.keyRow, siblingRow, outsiderRow},
		rows[pricing.Pricing]{fx.ownPrice, resellerPrice},
		bindings,
	)
	cat.UseTenancy(
		rows[team.Team]{fx.team},
		rows[project.Project]{fx.project, fx.otherProject},
		rows[serviceaccount.ServiceAccount]{fx.sa, sibling, outsider},
		rows[group.Group]{},
		rows[role.Role]{},
		rows[rolebinding.RoleBinding]{},
		rows[policybinding.PolicyBinding]{},
	)
	if err := cat.Reload(context.Background()); err != nil {
		t.Fatalf("catalog reload: %v", err)
	}

	state := kv.NewMem()
	t.Cleanup(func() { _ = state.Close() })
	markers := otlpreceiver.NewMarkers(state)
	if o.markerStore != nil {
		markers = otlpreceiver.NewMarkers(o.markerStore)
	}
	h := otlpreceiver.New(otlpreceiver.Options{
		Enabled:     func() bool { return fx.enabled },
		Snapshot:    cat.Current,
		Usage:       fx,
		Markers:     markers,
		Limiter:     pkgratelimit.New(state, slog.New(slog.DiscardHandler), nil),
		Pricer:      usagelog.NewPricer(func(id string) (*pricing.Pricing, bool) { return cat.Current().Pricing(id) }),
		SpanMappers: []otlp.SpanMapper{genai.Mapper{}},
		LogMappers:  []otlp.LogMapper{genai.Mapper{}},
		ProviderHints: map[string]otlpreceiver.ProviderHint{
			"acme.cloud": {Provider: "acme", Host: "a-reseller"},
			"zeta_ai":    {Provider: "zeta"},
		},
		CaptureContent:   func() bool { return fx.captureContent },
		UsageRetention:   func() time.Duration { return fx.usageRetention },
		ContentRetention: func() time.Duration { return fx.contentRetention },
		PayloadLog:       fx.payloads,
		Payloads:         fx.payloads,
		InstanceID:       "pod-a",
	})
	authenticated := func(next http.Handler) http.Handler {
		return inference.ClassifyMiddleware()(inference.AuthenticateMiddleware(cat, nil)(next))
	}
	mux := http.NewServeMux()
	mux.Handle(otlpreceiver.TracesPath, authenticated(h.Traces()))
	mux.Handle(otlpreceiver.LogsPath, authenticated(h.Logs()))
	fx.handler = mux
	return fx
}

// post sends a trace export.
func (fx *fixture) post(t *testing.T, contentType string, body []byte, header ...string) *httptest.ResponseRecorder {
	t.Helper()
	return fx.postTo(t, otlpreceiver.TracesPath, contentType, body, header...)
}

// postLogs sends a logs export.
func (fx *fixture) postLogs(t *testing.T, contentType string, body []byte, header ...string) *httptest.ResponseRecorder {
	t.Helper()
	return fx.postTo(t, otlpreceiver.LogsPath, contentType, body, header...)
}

func (fx *fixture) postTo(t *testing.T, path, contentType string, body []byte, header ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer "+relayKey)
	for i := 0; i+1 < len(header); i += 2 {
		if header[i+1] == "" {
			req.Header.Del(header[i])
		} else {
			req.Header.Set(header[i], header[i+1])
		}
	}
	rec := httptest.NewRecorder()
	fx.handler.ServeHTTP(rec, req)
	return rec
}

var spanStart = time.Date(2026, 8, 4, 10, 52, 22, 0, time.UTC)

func str(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}

func num(k string, v int64) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: v}}}
}

func pbSpan(spanID byte, attrs ...*commonpb.KeyValue) *tracepb.Span {
	return &tracepb.Span{
		TraceId:           bytes.Repeat([]byte{0xab}, 16),
		SpanId:            bytes.Repeat([]byte{spanID}, 8),
		Name:              "chat acme-large",
		StartTimeUnixNano: uint64(spanStart.UnixNano()),
		EndTimeUnixNano:   uint64(spanStart.Add(2 * time.Second).UnixNano()),
		Attributes:        attrs,
	}
}

func export(t *testing.T, spans ...*tracepb.Span) []byte {
	t.Helper()
	b, err := proto.Marshal(&tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{{
		Resource:   &resourcepb.Resource{Attributes: []*commonpb.KeyValue{str("service.name", "billing-agent")}},
		ScopeSpans: []*tracepb.ScopeSpans{{Spans: spans}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// chatAttrs are the attributes of one chat call, which its span and its event both carry.
func chatAttrs(modelName string) []*commonpb.KeyValue {
	return []*commonpb.KeyValue{
		str("gen_ai.operation.name", "chat"),
		str("gen_ai.provider.name", "acme"),
		str("gen_ai.request.model", modelName),
		str("gen_ai.response.id", "resp_123"),
		str("gen_ai.conversation.id", "conv_9"),
		num("gen_ai.usage.input_tokens", 1000),
		num("gen_ai.usage.cache_read.input_tokens", 700),
		num("gen_ai.usage.cache_write.input_tokens", 200),
		num("gen_ai.usage.output_tokens", 300),
	}
}

func chatSpan(spanID byte, modelName string) *tracepb.Span {
	return pbSpan(spanID, chatAttrs(modelName)...)
}

func TestExportRecordsAPricedUsageEvent(t *testing.T) {
	fx := newFixture(t)
	rec := fx.post(t, otlp.MediaTypeProtobuf, export(t, chatSpan(1, "acme-large")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
	if rec.Body.Len() != 0 || rec.Header().Get("Content-Type") != otlp.MediaTypeProtobuf {
		t.Errorf("success response = %q (%s), want the empty protobuf message", rec.Body, rec.Header().Get("Content-Type"))
	}
	if len(fx.events) != 1 {
		t.Fatalf("recorded %d events, want 1", len(fx.events))
	}
	ev := fx.events[0]

	if ev.Source != "otlp" || ev.Status != 200 || ev.DurationMs != 2000 || !ev.Timestamp.Equal(spanStart) {
		t.Errorf("outcome = source %q status %d duration %d ts %v", ev.Source, ev.Status, ev.DurationMs, ev.Timestamp)
	}
	if ev.RequestID != "otlp-"+strings.Repeat("ab", 16)+"-"+strings.Repeat("01", 8) {
		t.Errorf("RequestID = %q, want it derived from the span's ids", ev.RequestID)
	}
	// Attribution is the authenticated reporter's, never anything the export claims.
	if ev.ProjectID != fx.project.Meta.ID || ev.Project != "ml-search" || ev.TeamID != fx.team.Meta.ID || ev.Team != "platform" {
		t.Errorf("tenancy = %q/%q %q/%q", ev.ProjectID, ev.Project, ev.TeamID, ev.Team)
	}
	if ev.PrincipalKind != "serviceaccount" || ev.PrincipalID != fx.sa.Meta.ID || ev.Principal != "indexer" {
		t.Errorf("principal = %q %q %q", ev.PrincipalKind, ev.PrincipalID, ev.Principal)
	}
	if ev.CredentialKind != "key" || ev.CredentialID != fx.keyRow.Meta.ID || ev.RelayKeyHash != fx.keyRow.Spec.KeyHash {
		t.Errorf("credential = %q %q %q", ev.CredentialKind, ev.CredentialID, ev.RelayKeyHash)
	}
	// Relay routed nothing, so no policy, host or host key is named.
	if ev.PolicyID != "" || ev.HostID != "" || ev.HostKeyID != "" {
		t.Errorf("routing identity set on a reported event: %q %q %q", ev.PolicyID, ev.HostID, ev.HostKeyID)
	}
	if ev.ModelID != fx.model.Meta.ID || ev.Model != "acme-large" || ev.Provider != "acme" || ev.RequestedModel != "acme-large" {
		t.Errorf("model = %q %q provider %q requested %q", ev.ModelID, ev.Model, ev.Provider, ev.RequestedModel)
	}

	wantTokens := map[string]int64{"input": 100, "output": 300, "cache_read": 700, "cache_creation": 200}
	if len(ev.Tokens) != len(wantTokens) {
		t.Errorf("tokens = %v, want %v", ev.Tokens, wantTokens)
	}
	for k, v := range wantTokens {
		if ev.Tokens[k] != v {
			t.Errorf("tokens[%s] = %d, want %d", k, ev.Tokens[k], v)
		}
	}
	// 100×$3 + 300×$15 + 700×$0.30 + 200×$3.75 per million, in nano-USD, at the provider's own host rather than the reseller's.
	if ev.Pricing != "acme-large-list" || ev.CostNanos == nil || *ev.CostNanos != 5_760_000 {
		t.Errorf("pricing = %q cost = %v, want acme-large-list / 5760000", ev.Pricing, ev.CostNanos)
	}

	wantExtras := map[string]string{
		"telemetry_convention": "gen_ai",
		"trace_id":             strings.Repeat("ab", 16),
		"span_id":              strings.Repeat("01", 8),
		"response_id":          "resp_123",
		"session_id":           "conv_9",
		"service":              "billing-agent",
		"operation":            "chat",
		"reported_provider":    "acme",
		"instance":             "pod-a",
	}
	for k, v := range wantExtras {
		if ev.Extras[k] != v {
			t.Errorf("extras[%s] = %q, want %q", k, ev.Extras[k], v)
		}
	}
}

func TestExportOfAnUnknownModelStaysUnpriced(t *testing.T) {
	fx := newFixture(t)
	rec := fx.post(t, otlp.MediaTypeProtobuf, export(t, chatSpan(1, "someone-elses-model")))
	if rec.Code != http.StatusOK || len(fx.events) != 1 {
		t.Fatalf("status = %d events = %d", rec.Code, len(fx.events))
	}
	ev := fx.events[0]
	if ev.RequestedModel != "someone-elses-model" || ev.ModelID != "" || ev.Pricing != "" {
		t.Errorf("model = %q id %q pricing %q", ev.RequestedModel, ev.ModelID, ev.Pricing)
	}
	// Unpriced must stay distinguishable from a zero cost.
	if ev.CostNanos != nil {
		t.Errorf("CostNanos = %d, want nil", *ev.CostNanos)
	}
	if ev.Tokens["output"] != 300 {
		t.Errorf("tokens dropped for an unknown model: %v", ev.Tokens)
	}
}

func TestExportKeepsOnlyModelCalls(t *testing.T) {
	fx := newFixture(t)
	agent := pbSpan(2,
		str("gen_ai.operation.name", "invoke_agent"),
		num("gen_ai.usage.input_tokens", 1000),
		num("gen_ai.usage.output_tokens", 300),
	)
	httpCall := pbSpan(3, str("http.request.method", "POST"))
	noIdentity := chatSpan(4, "acme-large")
	noIdentity.SpanId = nil

	rec := fx.post(t, otlp.MediaTypeJSON, mustJSON(t, chatSpan(1, "acme-large"), agent, httpCall, noIdentity))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
	// The agent span repeats the usage of the chat span beneath it; recording both would double count.
	if len(fx.events) != 1 || fx.events[0].Extras["span_id"] != strings.Repeat("01", 8) {
		t.Fatalf("events = %+v, want only the chat span", fx.events)
	}
	var resp struct {
		PartialSuccess struct {
			RejectedSpans string `json:"rejectedSpans"`
		} `json:"partialSuccess"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response %q: %v", rec.Body, err)
	}
	if resp.PartialSuccess.RejectedSpans != "1" {
		t.Errorf("rejectedSpans = %q, want 1 (the model call without a span id)", resp.PartialSuccess.RejectedSpans)
	}
}

// mustJSON renders spans as an OTLP/JSON export, which writes ids as hex and 64-bit integers as strings.
func mustJSON(t *testing.T, spans ...*tracepb.Span) []byte {
	t.Helper()
	type kv = map[string]any
	var out []kv
	for _, s := range spans {
		var attrs []kv
		for _, a := range s.Attributes {
			v := kv{"stringValue": a.Value.GetStringValue()}
			if _, ok := a.Value.Value.(*commonpb.AnyValue_IntValue); ok {
				v = kv{"intValue": strconv.FormatInt(a.Value.GetIntValue(), 10)}
			}
			attrs = append(attrs, kv{"key": a.Key, "value": v})
		}
		out = append(out, kv{
			"traceId":           hex.EncodeToString(s.TraceId),
			"spanId":            hex.EncodeToString(s.SpanId),
			"name":              s.Name,
			"startTimeUnixNano": strconv.FormatUint(s.StartTimeUnixNano, 10),
			"endTimeUnixNano":   strconv.FormatUint(s.EndTimeUnixNano, 10),
			"attributes":        attrs,
		})
	}
	b, err := json.Marshal(kv{"resourceSpans": []kv{{"scopeSpans": []kv{{"spans": out}}}}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestExportAcceptsGzip(t *testing.T) {
	fx := newFixture(t)
	var zipped bytes.Buffer
	zw := gzip.NewWriter(&zipped)
	_, _ = zw.Write(export(t, chatSpan(1, "acme-large")))
	_ = zw.Close()
	rec := fx.post(t, otlp.MediaTypeProtobuf, zipped.Bytes(), "Content-Encoding", "gzip")
	if rec.Code != http.StatusOK || len(fx.events) != 1 {
		t.Fatalf("status = %d events = %d body = %s", rec.Code, len(fx.events), rec.Body)
	}
}

func TestExportRefusals(t *testing.T) {
	good := func(t *testing.T) []byte { return export(t, chatSpan(1, "acme-large")) }

	t.Run("receiver disabled", func(t *testing.T) {
		fx := newFixture(t)
		fx.enabled = false
		rec := fx.post(t, otlp.MediaTypeProtobuf, good(t))
		if rec.Code != http.StatusNotFound || len(fx.events) != 0 {
			t.Errorf("status = %d events = %d", rec.Code, len(fx.events))
		}
	})
	t.Run("no credential", func(t *testing.T) {
		fx := newFixture(t)
		rec := fx.post(t, otlp.MediaTypeProtobuf, good(t), "Authorization", "")
		if rec.Code != http.StatusUnauthorized || len(fx.events) != 0 {
			t.Errorf("status = %d events = %d", rec.Code, len(fx.events))
		}
	})
	t.Run("unknown key", func(t *testing.T) {
		fx := newFixture(t)
		rec := fx.post(t, otlp.MediaTypeProtobuf, good(t), "Authorization", "Bearer sk-wr-nobody")
		if rec.Code != http.StatusUnauthorized || len(fx.events) != 0 {
			t.Errorf("status = %d events = %d", rec.Code, len(fx.events))
		}
	})
	t.Run("unsupported content type", func(t *testing.T) {
		fx := newFixture(t)
		rec := fx.post(t, "text/plain", good(t))
		if rec.Code != http.StatusUnsupportedMediaType || len(fx.events) != 0 {
			t.Errorf("status = %d events = %d", rec.Code, len(fx.events))
		}
	})
	t.Run("undecodable body", func(t *testing.T) {
		fx := newFixture(t)
		rec := fx.post(t, otlp.MediaTypeJSON, []byte(`{"resourceSpans":`))
		if rec.Code != http.StatusBadRequest || len(fx.events) != 0 {
			t.Errorf("status = %d events = %d", rec.Code, len(fx.events))
		}
		var status struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil || status.Code != 3 || status.Message == "" {
			t.Errorf("error body = %q (%v), want a Status with code 3", rec.Body, err)
		}
	})
	t.Run("unsupported encoding", func(t *testing.T) {
		fx := newFixture(t)
		rec := fx.post(t, otlp.MediaTypeProtobuf, good(t), "Content-Encoding", "br")
		if rec.Code != http.StatusUnsupportedMediaType || len(fx.events) != 0 {
			t.Errorf("status = %d events = %d", rec.Code, len(fx.events))
		}
	})
}

func TestFailedCallIsALogOnlyRow(t *testing.T) {
	fx := newFixture(t)
	failed := pbSpan(1,
		str("gen_ai.operation.name", "chat"),
		str("gen_ai.request.model", "acme-large"),
		str("error.type", "rate_limit_exceeded"),
	)
	failed.Status = &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR}
	withStatus := pbSpan(2,
		str("gen_ai.operation.name", "chat"),
		str("gen_ai.request.model", "acme-large"),
		str("error.type", "rate_limit_exceeded"),
		num("http.response.status_code", 429),
	)
	if rec := fx.post(t, otlp.MediaTypeProtobuf, export(t, failed, withStatus)); rec.Code != http.StatusOK || len(fx.events) != 2 {
		t.Fatalf("status = %d events = %d", rec.Code, len(fx.events))
	}
	// The kind is relay's own; what the client called the error stays in the extras.
	if ev := fx.events[0]; ev.Status != 0 || ev.ErrorKind != "error" || !ev.LogOnly() || ev.CostNanos != nil || ev.Extras["reported_error_type"] != "rate_limit_exceeded" {
		t.Errorf("failed call without a status = %+v, want a log-only row of kind error", ev)
	}
	if ev := fx.events[1]; ev.Status != 429 || ev.ErrorKind != "upstream_error" || ev.Extras["reported_error_type"] != "rate_limit_exceeded" {
		t.Errorf("failed call with a status = status %d kind %q extras %v", ev.Status, ev.ErrorKind, ev.Extras)
	}
}

func TestACallThatSucceededHasNoErrorKind(t *testing.T) {
	fx := newFixture(t)
	if rec := fx.post(t, otlp.MediaTypeProtobuf, export(t, chatSpan(1, "acme-large"))); rec.Code != http.StatusOK || len(fx.events) != 1 {
		t.Fatalf("status = %d events = %d", rec.Code, len(fx.events))
	}
	if ev := fx.events[0]; ev.ErrorKind != "" || ev.Status != 200 {
		t.Errorf("event = status %d kind %q", ev.Status, ev.ErrorKind)
	}
	if _, ok := fx.events[0].Extras["reported_error_type"]; ok {
		t.Errorf("extras = %v, want no reported error type", fx.events[0].Extras)
	}
}

func TestACredentialUnderADisabledPolicyMayNotReport(t *testing.T) {
	yes := true
	fx := newFixtureWith(t, fixtureOptions{policyCaptures: &yes, policyDisabled: true})
	fx.captureContent, fx.payloads.enabled = true, true
	for path, body := range map[string][]byte{
		otlpreceiver.TracesPath: export(t, pbSpan(1, contentAttrs(t, asText)...)),
		otlpreceiver.LogsPath:   logsExport(t, chatEvent(2, "acme-large")),
	} {
		rec := fx.postTo(t, path, otlp.MediaTypeProtobuf, body)
		var refusal struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &refusal); err != nil {
			t.Fatalf("%s: body %q: %v", path, rec.Body, err)
		}
		// The answer inference gives the same credential.
		if rec.Code != http.StatusForbidden || refusal.Error.Code != "policy_disabled" {
			t.Errorf("%s: status = %d code = %q, want 403 policy_disabled", path, rec.Code, refusal.Error.Code)
		}
	}
	if len(fx.events) != 0 || len(fx.payloads.records) != 0 {
		t.Errorf("recorded %d events and %d payloads from a credential that may not report", len(fx.events), len(fx.payloads.records))
	}
	// Refused before the body is read: a body that cannot be decoded gets the same answer.
	if rec := fx.post(t, otlp.MediaTypeProtobuf, []byte("not an export")); rec.Code != http.StatusForbidden {
		t.Errorf("undecodable body: status = %d, want 403", rec.Code)
	}
	// Another key of the same project, under no policy, still reports.
	if rec := fx.post(t, otlp.MediaTypeProtobuf, export(t, chatSpan(3, "acme-large")), "Authorization", "Bearer "+siblingKey); rec.Code != http.StatusOK || len(fx.events) != 1 {
		t.Errorf("key with no policy: status = %d events = %d", rec.Code, len(fx.events))
	}
}

func TestAPolicyThatDoesNotGrantTheModelStillReports(t *testing.T) {
	no := false
	fx := newFixtureWith(t, fixtureOptions{policyCaptures: &no, policyModels: []string{"zeta"}})
	if rec := fx.post(t, otlp.MediaTypeProtobuf, export(t, chatSpan(1, "acme-large"))); rec.Code != http.StatusOK || len(fx.events) != 1 || fx.events[0].Model != "acme-large" {
		t.Errorf("status = %d events = %d, want the call recorded although the policy grants another provider's models", rec.Code, len(fx.events))
	}
}

// spanIDs returns the span ids of the queued events, sorted.
func (fx *fixture) spanIDs() []string {
	var ids []string
	for _, ev := range fx.events {
		ids = append(ids, ev.Extras["span_id"])
	}
	slices.Sort(ids)
	return ids
}

func spanIDsOf(ids ...byte) []string {
	var out []string
	for _, id := range ids {
		out = append(out, strings.Repeat(hex.EncodeToString([]byte{id}), 8))
	}
	return out
}

// statusOf decodes the Status body of a refused OTLP/JSON export.
func statusOf(t *testing.T, rec *httptest.ResponseRecorder) (code int, message string) {
	t.Helper()
	var status struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatalf("error body %q: %v", rec.Body, err)
	}
	return status.Code, status.Message
}

func TestResentExportIsRecordedOnce(t *testing.T) {
	fx := newFixture(t)
	body := export(t, chatSpan(1, "acme-large"), chatSpan(2, "acme-large"))
	if rec := fx.post(t, otlp.MediaTypeProtobuf, body); rec.Code != http.StatusOK || len(fx.events) != 2 {
		t.Fatalf("first export: status = %d events = %d", rec.Code, len(fx.events))
	}

	rec := fx.post(t, otlp.MediaTypeProtobuf, body)
	// A duplicate is not a rejection: the exporter must see plain success, or it would report data loss.
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Errorf("resent export: status = %d body = %q, want the empty success message", rec.Code, rec.Body)
	}
	if len(fx.events) != 2 {
		t.Fatalf("resent export recorded again: %d events, want 2", len(fx.events))
	}

	// A resend that also carries a new call records only that one, and a call repeated inside one export is recorded once.
	rec = fx.post(t, otlp.MediaTypeProtobuf, export(t, chatSpan(2, "acme-large"), chatSpan(3, "acme-large"), chatSpan(3, "acme-large")))
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Errorf("mixed export: status = %d body = %q", rec.Code, rec.Body)
	}
	if got, want := fx.spanIDs(), spanIDsOf(1, 2, 3); !slices.Equal(got, want) {
		t.Errorf("recorded spans = %v, want %v", got, want)
	}
}

func TestDuplicatesAreRecognisedPerProject(t *testing.T) {
	fx := newFixture(t)
	body := export(t, chatSpan(1, "acme-large"))
	as := func(secret string) int {
		return fx.post(t, otlp.MediaTypeProtobuf, body, "Authorization", "Bearer "+secret).Code
	}
	if code := as(relayKey); code != http.StatusOK || len(fx.events) != 1 {
		t.Fatalf("first report: status = %d events = %d", code, len(fx.events))
	}
	// One service reporting through two keys of its project is still one call.
	if code := as(siblingKey); code != http.StatusOK || len(fx.events) != 1 {
		t.Fatalf("same project, another key: status = %d events = %d, want the call skipped", code, len(fx.events))
	}
	// Another project reporting the same ids must not find its call already taken.
	if code := as(outsiderKey); code != http.StatusOK || len(fx.events) != 2 {
		t.Fatalf("another project: status = %d events = %d, want its call recorded", code, len(fx.events))
	}
	if got := fx.events[1].ProjectID; got != fx.otherProject.Meta.ID {
		t.Errorf("second event project = %q, want the other project", got)
	}
}

type brokenStore struct{}

func (brokenStore) RunScript(context.Context, string, string, []string, ...any) ([]byte, error) {
	return nil, errors.New("kv down")
}

func TestDuplicateCheckFailureStillRecords(t *testing.T) {
	fx := newFixtureWith(t, fixtureOptions{markerStore: brokenStore{}})
	body := export(t, chatSpan(1, "acme-large"))
	for range 2 {
		if rec := fx.post(t, otlp.MediaTypeProtobuf, body); rec.Code != http.StatusOK {
			t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
		}
	}
	// Without the store a resend cannot be recognised; both are kept rather than risking the first.
	if len(fx.events) != 2 {
		t.Errorf("events = %d, want 2", len(fx.events))
	}
}

func TestFullUsageQueueRefusesTheExport(t *testing.T) {
	fx := newFixture(t)
	// Half the queue is the receiver's to use; three of those four places are taken.
	fx.capacity = 8
	fx.events = make([]usagelog.Event, 3)
	body := mustJSON(t, chatSpan(1, "acme-large"), chatSpan(2, "acme-large"))

	rec := fx.post(t, otlp.MediaTypeJSON, body)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "1" {
		t.Fatalf("status = %d Retry-After = %q, want 503 and 1", rec.Code, rec.Header().Get("Retry-After"))
	}
	if code, _ := statusOf(t, rec); code != int(otlp.StatusUnavailable) {
		t.Errorf("status code = %d, want UNAVAILABLE", code)
	}
	if len(fx.events) != 3 {
		t.Fatalf("a refused export queued %d events", len(fx.events)-3)
	}

	fx.events = nil
	if rec := fx.post(t, otlp.MediaTypeJSON, body); rec.Code != http.StatusOK {
		t.Fatalf("resend after the queue drained: status = %d body = %s", rec.Code, rec.Body)
	}
	// Refusing must not have marked the calls as recorded.
	if got, want := fx.spanIDs(), spanIDsOf(1, 2); !slices.Equal(got, want) {
		t.Errorf("recorded spans = %v, want %v", got, want)
	}
}

func TestReportedCallsLeaveHalfTheQueueToOtherTraffic(t *testing.T) {
	fx := newFixture(t)
	fx.capacity = 8
	for id := byte(1); id <= 4; id++ {
		if rec := fx.post(t, otlp.MediaTypeProtobuf, export(t, chatSpan(id, "acme-large"))); rec.Code != http.StatusOK {
			t.Fatalf("export %d: status = %d, want 200 while the queue is under half full", id, rec.Code)
		}
	}
	if rec := fx.post(t, otlp.MediaTypeProtobuf, export(t, chatSpan(5, "acme-large"))); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 once the queue is half full", rec.Code)
	}
	if len(fx.events) != 4 {
		t.Fatalf("queue holds %d of %d, want the receiver to stop at half", len(fx.events), fx.capacity)
	}

	// An export larger than the receiver's half is taken in parts, and each part stops at half too.
	fx.events = nil
	big := export(t, chatSpan(6, "acme-large"), chatSpan(7, "acme-large"), chatSpan(8, "acme-large"), chatSpan(9, "acme-large"), chatSpan(10, "acme-large"))
	if rec := fx.post(t, otlp.MediaTypeProtobuf, big); rec.Code != http.StatusServiceUnavailable || len(fx.events) != 4 {
		t.Fatalf("status = %d queued = %d, want 503 with 4 queued", rec.Code, len(fx.events))
	}
}

func TestQueueFillingDuringAnExportLosesNothing(t *testing.T) {
	fx := newFixture(t)
	fx.capacity = 1
	fx.phantomRoom = 1
	body := export(t, chatSpan(1, "acme-large"), chatSpan(2, "acme-large"))

	if rec := fx.post(t, otlp.MediaTypeProtobuf, body); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 for an export that was queued only in part", rec.Code)
	}
	recorded := fx.spanIDs()
	if len(recorded) != 1 {
		t.Fatalf("queued %d events before the queue filled, want 1", len(recorded))
	}

	fx.events, fx.phantomRoom = nil, 0
	if rec := fx.post(t, otlp.MediaTypeProtobuf, body); rec.Code != http.StatusOK {
		t.Fatalf("resend: status = %d body = %s", rec.Code, rec.Body)
	}
	// The resend adds exactly the call that did not fit: the queued one is skipped, the other was unmarked.
	if got, want := append(recorded, fx.spanIDs()...), spanIDsOf(1, 2); !slices.Equal(got, want) {
		t.Errorf("recorded across both attempts = %v, want %v", got, want)
	}
}

func TestExportLargerThanTheQueueIsTakenInParts(t *testing.T) {
	fx := newFixture(t)
	fx.capacity = 4
	body := export(t, chatSpan(1, "acme-large"), chatSpan(2, "acme-large"), chatSpan(3, "acme-large"), chatSpan(4, "acme-large"), chatSpan(5, "acme-large"))

	var recorded []string
	var codes []int
	for range 3 {
		rec := fx.post(t, otlp.MediaTypeProtobuf, body)
		codes = append(codes, rec.Code)
		recorded = append(recorded, fx.spanIDs()...)
		fx.events = nil
	}
	if want := []int{http.StatusServiceUnavailable, http.StatusServiceUnavailable, http.StatusOK}; !slices.Equal(codes, want) {
		t.Errorf("statuses = %v, want %v", codes, want)
	}
	slices.Sort(recorded)
	if want := spanIDsOf(1, 2, 3, 4, 5); !slices.Equal(recorded, want) {
		t.Errorf("recorded across resends = %v, want each call once: %v", recorded, want)
	}
}

func TestStartTimes(t *testing.T) {
	fx := newFixture(t)
	startingAt := func(id byte, start time.Time) *tracepb.Span {
		s := chatSpan(id, "acme-large")
		s.StartTimeUnixNano = uint64(start.UnixNano())
		s.EndTimeUnixNano = uint64(start.Add(time.Second).UnixNano())
		return s
	}
	unstamped := chatSpan(4, "acme-large")
	unstamped.StartTimeUnixNano, unstamped.EndTimeUnixNano = 0, 0

	before := time.Now()
	rec := fx.post(t, otlp.MediaTypeJSON, mustJSON(t,
		startingAt(1, before.Add(10*time.Minute)),
		startingAt(2, before.Add(time.Minute)),
		startingAt(3, time.Date(2019, 3, 1, 0, 0, 0, 0, time.UTC)),
		unstamped,
	))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
	// Ten minutes ahead is a broken clock; a minute ahead is ordinary skew, and an old call is a late report.
	if got, want := fx.spanIDs(), spanIDsOf(2, 3, 4); !slices.Equal(got, want) {
		t.Fatalf("recorded spans = %v, want %v", got, want)
	}
	var resp struct {
		PartialSuccess struct {
			RejectedSpans string `json:"rejectedSpans"`
			ErrorMessage  string `json:"errorMessage"`
		} `json:"partialSuccess"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response %q: %v", rec.Body, err)
	}
	if resp.PartialSuccess.RejectedSpans != "1" || !strings.Contains(resp.PartialSuccess.ErrorMessage, "future") {
		t.Errorf("partial success = %+v, want 1 span rejected for starting in the future", resp.PartialSuccess)
	}
	for _, ev := range fx.events {
		if ev.Extras["span_id"] != spanIDsOf(4)[0] {
			continue
		}
		if ev.Timestamp.Before(before) || ev.Timestamp.After(time.Now()) {
			t.Errorf("call without a start time stamped %v, want the time it was received", ev.Timestamp)
		}
	}
}

const day = 24 * time.Hour

// startingAt moves a span to start at start and last a second.
func startingAt(s *tracepb.Span, start time.Time) *tracepb.Span {
	s.StartTimeUnixNano = uint64(start.UnixNano())
	s.EndTimeUnixNano = uint64(start.Add(time.Second).UnixNano())
	return s
}

func TestACallOlderThanTheUsageRetentionIsRejected(t *testing.T) {
	fx := newFixture(t)
	fx.usageRetention = 30 * day
	now := time.Now()
	unstamped := chatSpan(5, "acme-large")
	unstamped.StartTimeUnixNano, unstamped.EndTimeUnixNano = 0, 0
	export := func(t *testing.T) []byte {
		return mustJSON(t,
			startingAt(chatSpan(1, "acme-large"), now.Add(-45*day)),
			startingAt(chatSpan(2, "acme-large"), now.Add(-29*day)),
			// Inside the window by half an hour: the store would delete it within the hour, so it is refused too.
			startingAt(chatSpan(3, "acme-large"), now.Add(-30*day+30*time.Minute)),
			// Inside by two hours: kept.
			startingAt(chatSpan(4, "acme-large"), now.Add(-30*day+2*time.Hour)),
			unstamped,
		)
	}

	rejectedBefore := otlpreceiver.RecordCount("spans", "rejected")
	rec := fx.post(t, otlp.MediaTypeJSON, export(t))
	rejected, message, _ := partialSuccess(t, rec)
	if rec.Code != http.StatusOK || rejected != "2" {
		t.Fatalf("status = %d body = %s, want two calls rejected", rec.Code, rec.Body)
	}
	if want := "model-call spans rejected: 2 older than the usage retention of 30 days"; message != want {
		t.Errorf("message = %q, want %q", message, want)
	}
	// A call with no start time is dated at receipt, so it is never too old.
	if got, want := fx.spanIDs(), spanIDsOf(2, 4, 5); !slices.Equal(got, want) {
		t.Errorf("recorded spans = %v, want %v", got, want)
	}
	if got := otlpreceiver.RecordCount("spans", "rejected") - rejectedBefore; got != 2 {
		t.Errorf("spans counted as rejected = %v, want 2", got)
	}

	// The setting is followed without a restart: once the store keeps everything, the same export is accepted whole.
	fx.usageRetention = 0
	rec = fx.post(t, otlp.MediaTypeJSON, export(t))
	if rec.Code != http.StatusOK || rec.Body.String() != "{}" {
		t.Fatalf("keep forever: status = %d body = %s, want the empty success", rec.Code, rec.Body)
	}
	if got, want := fx.spanIDs(), spanIDsOf(1, 2, 3, 4, 5); !slices.Equal(got, want) {
		t.Errorf("recorded spans = %v, want %v", got, want)
	}
}

func TestAnEventOlderThanTheUsageRetentionIsRejected(t *testing.T) {
	fx := newFixture(t)
	fx.usageRetention = 7 * day
	old := chatEvent(1, "acme-large")
	old.TimeUnixNano = uint64(time.Now().Add(-8 * day).UnixNano())
	recent := chatEvent(2, "acme-large")
	recent.TimeUnixNano = uint64(time.Now().Add(-time.Hour).UnixNano())
	// The reasons an export's calls are refused for share the one message.
	anonymous := detachedEvent("")
	anonymous.TimeUnixNano = recent.TimeUnixNano

	rejectedBefore := otlpreceiver.RecordCount("log_records", "rejected")
	rec := fx.postLogs(t, otlp.MediaTypeJSON, logsJSON(t, old, recent, anonymous))
	rejected, message, _ := partialSuccess(t, rec)
	if rec.Code != http.StatusOK || rejected != "2" || len(fx.events) != 1 {
		t.Fatalf("status = %d body = %s events = %d, want one recorded and two rejected", rec.Code, rec.Body, len(fx.events))
	}
	if want := "model-call log records rejected: 1 with neither span ids nor a response id; 1 older than the usage retention of 7 days"; message != want {
		t.Errorf("message = %q, want %q", message, want)
	}
	if got := otlpreceiver.RecordCount("log_records", "rejected") - rejectedBefore; got != 2 {
		t.Errorf("log records counted as rejected = %v, want 2", got)
	}
}

func TestContentOlderThanThePayloadRetentionIsNotStored(t *testing.T) {
	fx := capturing(t)
	fx.usageRetention, fx.contentRetention = 90*day, 30*day
	now := time.Now()
	withContent := func(id byte, age time.Duration) *tracepb.Span {
		return startingAt(pbSpan(id, contentAttrs(t, asText)...), now.Add(-age))
	}
	expiredBefore := otlpreceiver.ContentCount("expired")
	rec := fx.post(t, otlp.MediaTypeJSON, mustJSON(t,
		withContent(1, 45*day),
		withContent(2, 30*day-30*time.Minute),
		withContent(3, time.Hour),
		// Older than the usage retention as well: rejected, and not part of the content warning.
		withContent(4, 120*day),
	))
	rejected, message, _ := partialSuccess(t, rec)
	// Three usage rows, whatever became of their content; only the newest call's content is kept.
	if rec.Code != http.StatusOK || rejected != "1" || !slices.Equal(fx.spanIDs(), spanIDsOf(1, 2, 3)) {
		t.Fatalf("status = %d body = %s recorded spans = %v, want three recorded and one rejected", rec.Code, rec.Body, fx.spanIDs())
	}
	if len(fx.payloads.records) != 1 || !strings.HasSuffix(fx.payloads.records[0].RequestID, spanIDsOf(3)[0]) {
		t.Errorf("payload records = %+v, want only the recent call's", fx.payloads.records)
	}
	want := "model-call spans rejected: 1 older than the usage retention of 90 days. Also: message content of 2 model calls was not stored: the calls are older than the content retention of 30 days. It is safe to leave content out of calls that old; usage was recorded."
	if message != want {
		t.Errorf("message =\n%s\nwant\n%s", message, want)
	}
	if got := otlpreceiver.ContentCount("expired") - expiredBefore; got != 2 {
		t.Errorf("content counted as expired = %v, want 2", got)
	}

	// A payload store that keeps everything takes content of any age the usage store accepts.
	fx.contentRetention = 0
	if rec := fx.post(t, otlp.MediaTypeJSON, mustJSON(t, withContent(5, 45*day))); rec.Body.String() != "{}" || len(fx.payloads.records) != 2 {
		t.Errorf("keep forever: body = %s payload records = %d, want the empty success and the content stored", rec.Body, len(fx.payloads.records))
	}
}

func TestExportOverTheRecordLimitIsRefused(t *testing.T) {
	manyCalls := func(n int) []*tracepb.Span {
		spans := make([]*tracepb.Span, n)
		for i := range spans {
			spans[i] = chatSpan(0, "acme-large")
			id := i + 1
			spans[i].SpanId = []byte{1, 0, 0, 0, 0, 0, byte(id >> 8), byte(id)}
		}
		return spans
	}

	t.Run("over the limit", func(t *testing.T) {
		fx := newFixture(t)
		fx.capacity = 2 * otlpreceiver.MaxRecords
		rec := fx.post(t, otlp.MediaTypeJSON, mustJSON(t, manyCalls(otlpreceiver.MaxRecords+1)...))
		if rec.Code != http.StatusBadRequest || len(fx.events) != 0 {
			t.Fatalf("status = %d events = %d, want 400 and none", rec.Code, len(fx.events))
		}
		if code, msg := statusOf(t, rec); code != int(otlp.StatusInvalidArgument) || msg == "" {
			t.Errorf("error body = %d %q, want INVALID_ARGUMENT with a message", code, msg)
		}
	})
	t.Run("at the limit, beside spans that are not model calls", func(t *testing.T) {
		fx := newFixture(t)
		fx.capacity = 2 * otlpreceiver.MaxRecords
		spans := manyCalls(otlpreceiver.MaxRecords)
		for i := byte(1); i <= 100; i++ {
			spans = append(spans, pbSpan(i, str("http.request.method", "POST")))
		}
		rec := fx.post(t, otlp.MediaTypeProtobuf, export(t, spans...))
		if rec.Code != http.StatusOK || len(fx.events) != otlpreceiver.MaxRecords {
			t.Fatalf("status = %d events = %d, want 200 and %d", rec.Code, len(fx.events), otlpreceiver.MaxRecords)
		}
	})
}

func TestExportRateLimit(t *testing.T) {
	fx := newFixtureWith(t, fixtureOptions{exportsPerMinute: 2})
	for id := byte(1); id <= 2; id++ {
		if rec := fx.post(t, otlp.MediaTypeJSON, mustJSON(t, chatSpan(id, "acme-large"))); rec.Code != http.StatusOK {
			t.Fatalf("export %d: status = %d body = %s", id, rec.Code, rec.Body)
		}
	}
	rec := fx.post(t, otlp.MediaTypeJSON, mustJSON(t, chatSpan(3, "acme-large")))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("third export in the window: status = %d, want 429", rec.Code)
	}
	if secs, err := strconv.Atoi(rec.Header().Get("Retry-After")); err != nil || secs < 1 || secs > 60 {
		t.Errorf("Retry-After = %q, want whole seconds within the window", rec.Header().Get("Retry-After"))
	}
	if code, _ := statusOf(t, rec); code != int(otlp.StatusResourceExhausted) {
		t.Errorf("status code = %d, want RESOURCE_EXHAUSTED", code)
	}
	if len(fx.events) != 2 {
		t.Errorf("events = %d, want only the two exports inside the limit", len(fx.events))
	}
}

func TestExportRateLimitWithoutACatalogRow(t *testing.T) {
	fx := newFixture(t)
	// Not a model call, so nothing is recorded: only the request count matters here.
	body := export(t, pbSpan(1, str("http.request.method", "POST")))
	for i := range otlpreceiver.DefaultExportsPerMinute {
		if rec := fx.post(t, otlp.MediaTypeProtobuf, body); rec.Code != http.StatusOK {
			t.Fatalf("export %d: status = %d, want 200 inside the built-in limit", i+1, rec.Code)
		}
	}
	if rec := fx.post(t, otlp.MediaTypeProtobuf, body); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 past the built-in limit", rec.Code)
	}
	// The limit is per credential.
	if rec := fx.post(t, otlp.MediaTypeProtobuf, body, "Authorization", "Bearer "+siblingKey); rec.Code != http.StatusOK {
		t.Fatalf("another credential: status = %d, want 200", rec.Code)
	}
}

func TestDisabledExportRateLimitRowAppliesNoLimit(t *testing.T) {
	fx := newFixtureWith(t, fixtureOptions{exportsPerMinute: 1, rateLimitDisabled: true})
	for i := range 3 {
		if rec := fx.post(t, otlp.MediaTypeProtobuf, export(t, pbSpan(1, str("http.request.method", "POST")))); rec.Code != http.StatusOK {
			t.Fatalf("export %d: status = %d, want 200 with the limit switched off", i+1, rec.Code)
		}
	}
}

func TestProviderHints(t *testing.T) {
	reported := func(provider, modelName string) *tracepb.Span {
		return pbSpan(1,
			str("gen_ai.operation.name", "chat"),
			str("gen_ai.provider.name", provider),
			str("gen_ai.request.model", modelName),
			num("gen_ai.usage.input_tokens", 1000),
			num("gen_ai.usage.output_tokens", 300),
		)
	}
	record := func(t *testing.T, fx *fixture, provider, modelName string) usagelog.Event {
		t.Helper()
		rec := fx.post(t, otlp.MediaTypeProtobuf, export(t, reported(provider, modelName)))
		if rec.Code != http.StatusOK || len(fx.events) != 1 {
			t.Fatalf("status = %d events = %d", rec.Code, len(fx.events))
		}
		return fx.events[0]
	}

	t.Run("the hinted host prices the call", func(t *testing.T) {
		fx := newFixture(t)
		ev := record(t, fx, "ACME.cloud", "acme-large")
		// 1000×$30 + 300×$150 per million at the host the reported name stands for, not the provider's own list price.
		if ev.ModelID != fx.model.Meta.ID || ev.Provider != "acme" || ev.Pricing != "reseller-markup" || ev.CostNanos == nil || *ev.CostNanos != 75_000_000 {
			t.Errorf("model = %q provider = %q pricing = %q cost = %v", ev.Model, ev.Provider, ev.Pricing, ev.CostNanos)
		}
		if ev.Extras["reported_provider"] != "ACME.cloud" {
			t.Errorf("reported_provider = %q, want the name as the client sent it", ev.Extras["reported_provider"])
		}
	})
	t.Run("the hinted provider picks between models that share a name", func(t *testing.T) {
		fx := newFixture(t)
		if ev := record(t, fx, "zeta_ai", "shared"); ev.ModelID != fx.sharedZeta.Meta.ID || ev.Provider != "zeta" {
			t.Errorf("zeta_ai/shared resolved to %q of %q, want zeta's model", ev.Model, ev.Provider)
		}
		fx = newFixture(t)
		if ev := record(t, fx, "acme.cloud", "shared"); ev.ModelID != fx.sharedAcme.Meta.ID || ev.Provider != "acme" {
			t.Errorf("acme.cloud/shared resolved to %q of %q, want acme's model", ev.Model, ev.Provider)
		}
	})
	t.Run("a provider without a hint resolves as reported", func(t *testing.T) {
		fx := newFixture(t)
		if ev := record(t, fx, "zeta", "shared"); ev.ModelID != fx.sharedZeta.Meta.ID {
			t.Errorf("zeta/shared resolved to %q, want zeta's model", ev.Model)
		}
		fx = newFixture(t)
		if ev := record(t, fx, "nobody", "acme-large"); ev.ModelID != fx.model.Meta.ID || ev.Pricing != "acme-large-list" {
			t.Errorf("unknown provider: model = %q pricing = %q, want the bare name at the provider's own host", ev.Model, ev.Pricing)
		}
	})
}

func TestReasoningTokensAreChargedOnce(t *testing.T) {
	// The usage of a reasoning call as an exporter reports it: output_tokens (214) includes the reasoning tokens (64), input_tokens (800) the cached ones (600).
	reasoningCall := pbSpan(1,
		str("gen_ai.operation.name", "generate_content"),
		str("gen_ai.provider.name", "acme"),
		str("gen_ai.request.model", "acme-large"),
		num("gen_ai.usage.input_tokens", 800),
		num("gen_ai.usage.cache_read.input_tokens", 600),
		num("gen_ai.usage.output_tokens", 214),
		num("gen_ai.usage.reasoning.output_tokens", 64),
	)
	// 200×$3 + 600×$0.30 + 214×$15 per million either way: reasoning tokens cost the output rate once.
	const wantCost = 3_990_000
	for _, tc := range []struct {
		name          string
		reasoningRate float64
		// wantOutputCost and wantReasoningCost are the breakdown: with a reasoning meter the 64 reasoning tokens are charged there and the other 150 on output.
		wantOutputCost    int64
		wantReasoningCost int64
	}{
		{name: "rate sheet without a reasoning meter", wantOutputCost: 3_210_000},
		{name: "rate sheet with a reasoning meter", reasoningRate: 15, wantOutputCost: 2_250_000, wantReasoningCost: 960_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newFixtureWith(t, fixtureOptions{reasoningRate: tc.reasoningRate})
			if rec := fx.post(t, otlp.MediaTypeProtobuf, export(t, reasoningCall)); rec.Code != http.StatusOK || len(fx.events) != 1 {
				t.Fatalf("status = %d events = %d", rec.Code, len(fx.events))
			}
			ev := fx.events[0]
			// The stored counts mean the same whatever sheet prices them: output includes reasoning.
			if ev.Tokens["output"] != 214 || ev.Tokens["reasoning"] != 64 || ev.Tokens["input"] != 200 || ev.Tokens["cache_read"] != 600 {
				t.Errorf("tokens = %v, want output 214 reasoning 64 input 200 cache_read 600", ev.Tokens)
			}
			if ev.CostNanos == nil || *ev.CostNanos != wantCost {
				t.Errorf("cost = %v (%v), want %d", ev.CostNanos, ev.CostBreakdown, wantCost)
			}
			if ev.CostBreakdown["tokens.output"] != tc.wantOutputCost || ev.CostBreakdown["tokens.reasoning"] != tc.wantReasoningCost {
				t.Errorf("breakdown = %v, want output %d reasoning %d", ev.CostBreakdown, tc.wantOutputCost, tc.wantReasoningCost)
			}
		})
	}
}

func TestErrorTypeCarryingTheHTTPStatusIsTheEventStatus(t *testing.T) {
	fx := newFixture(t)
	failed := pbSpan(1,
		str("gen_ai.operation.name", "generate_content"),
		str("gen_ai.request.model", "acme-large"),
		str("error.type", "429"),
	)
	failed.Status = &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR, Message: "429 RESOURCE_EXHAUSTED."}
	if rec := fx.post(t, otlp.MediaTypeProtobuf, export(t, failed)); rec.Code != http.StatusOK || len(fx.events) != 1 {
		t.Fatalf("status = %d events = %d", rec.Code, len(fx.events))
	}
	if ev := fx.events[0]; ev.Status != 429 || ev.ErrorKind != "upstream_error" || ev.Extras["reported_error_type"] != "429" || ev.LogOnly() {
		t.Errorf("event = status %d kind %q extras %v log-only %v, want a 429 row", ev.Status, ev.ErrorKind, ev.Extras, ev.LogOnly())
	}
}

func TestThePolicyOfTheReporterDecidesWhetherContentIsStored(t *testing.T) {
	yes, no := true, false
	for _, tc := range []struct {
		name string
		opts fixtureOptions
		// receiverOff and payloadLoggingOff turn one of the operator's switches off.
		receiverOff, payloadLoggingOff bool
		wantStored                     bool
		// wantByPolicy is whether the content is counted as refused by the policy.
		wantByPolicy bool
	}{
		{name: "policy captures", opts: fixtureOptions{policyCaptures: &yes}, wantStored: true},
		{name: "policy does not capture", opts: fixtureOptions{policyCaptures: &no}, wantByPolicy: true},
		{name: "no policy: what the client sent is kept", opts: fixtureOptions{}, wantStored: true},
		{name: "key flag set, policy does not capture", opts: fixtureOptions{policyCaptures: &no, keyCaptures: true}, wantByPolicy: true},
		{name: "policy captures, receiver switch off", opts: fixtureOptions{policyCaptures: &yes}, receiverOff: true},
		{name: "policy captures, payload logging off", opts: fixtureOptions{policyCaptures: &yes}, payloadLoggingOff: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newFixtureWith(t, tc.opts)
			fx.captureContent = !tc.receiverOff
			fx.payloads.enabled = !tc.payloadLoggingOff
			byPolicy := otlpreceiver.ContentCount("policy")

			rec := fx.post(t, otlp.MediaTypeJSON, mustJSON(t, pbSpan(1, contentAttrs(t, asText)...)))
			// The usage row is recorded whatever happens to the content, and the export succeeds.
			if rec.Code != http.StatusOK || len(fx.events) != 1 {
				t.Fatalf("status = %d body = %q events = %d, want an accepted export and one usage row", rec.Code, rec.Body, len(fx.events))
			}
			// Stored content needs no word to the client; withheld content is answered with a warning, never a rejection.
			if warned := rec.Body.String() != "{}"; warned == tc.wantStored {
				t.Errorf("body = %s, want a warning only when the content was withheld", rec.Body)
			}
			if stored := len(fx.payloads.records) == 1; stored != tc.wantStored {
				t.Errorf("payload records = %d, want stored = %v", len(fx.payloads.records), tc.wantStored)
			}
			if counted := otlpreceiver.ContentCount("policy")-byPolicy == 1; counted != tc.wantByPolicy {
				t.Errorf("counted as refused by policy = %v, want %v", counted, tc.wantByPolicy)
			}
			// A reported row names no policy even when one decided its content.
			if fx.events[0].PolicyID != "" {
				t.Errorf("usage row carries policy %q", fx.events[0].PolicyID)
			}
		})
	}
}

// partialSuccess decodes the body of an accepted JSON export.
func partialSuccess(t *testing.T, rec *httptest.ResponseRecorder) (rejected, message string, present bool) {
	t.Helper()
	var got struct {
		PartialSuccess *struct {
			RejectedSpans      string `json:"rejectedSpans"`
			RejectedLogRecords string `json:"rejectedLogRecords"`
			ErrorMessage       string `json:"errorMessage"`
		} `json:"partialSuccess"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body %q: %v", rec.Body, err)
	}
	if got.PartialSuccess == nil {
		return "", "", false
	}
	return got.PartialSuccess.RejectedSpans + got.PartialSuccess.RejectedLogRecords, got.PartialSuccess.ErrorMessage, true
}

func TestTheClientIsToldWhenItsContentWasNotStored(t *testing.T) {
	no := false
	withContent := func(t *testing.T, id byte) *tracepb.Span { return pbSpan(id, contentAttrs(t, asText)...) }

	t.Run("policy does not store content", func(t *testing.T) {
		fx := newFixtureWith(t, fixtureOptions{policyCaptures: &no})
		fx.captureContent, fx.payloads.enabled = true, true
		// Two calls with content and one without: the warning counts the two.
		rec := fx.post(t, otlp.MediaTypeJSON, mustJSON(t, withContent(t, 1), withContent(t, 2), chatSpan(3, "acme-large")))
		rejected, message, present := partialSuccess(t, rec)
		if rec.Code != http.StatusOK || !present || rejected != "" || len(fx.events) != 3 {
			t.Fatalf("status = %d body = %s events = %d, want an accepted export with a warning and nothing rejected", rec.Code, rec.Body, len(fx.events))
		}
		want := "message content of 2 model calls was not stored: the policy of this credential does not allow storing message content. It is safe to stop sending content; usage was recorded."
		if message != want {
			t.Errorf("warning =\n%s\nwant\n%s", message, want)
		}
	})

	t.Run("content capture is off", func(t *testing.T) {
		fx := newFixture(t)
		rec := fx.postLogs(t, otlp.MediaTypeJSON, logsJSON(t, pbEvent(1, contentAttrs(t, asStructure)...)))
		rejected, message, present := partialSuccess(t, rec)
		if rec.Code != http.StatusOK || !present || rejected != "" || !strings.Contains(message, "message content of 1 model calls was not stored: this server does not capture message content") {
			t.Errorf("status = %d body = %s, want a warning that the server does not capture content", rec.Code, rec.Body)
		}
	})

	t.Run("beside a rejection", func(t *testing.T) {
		fx := newFixtureWith(t, fixtureOptions{policyCaptures: &no})
		fx.captureContent, fx.payloads.enabled = true, true
		anonymous := withContent(t, 2)
		anonymous.SpanId = nil
		rec := fx.post(t, otlp.MediaTypeJSON, mustJSON(t, withContent(t, 1), anonymous))
		rejected, message, _ := partialSuccess(t, rec)
		if rec.Code != http.StatusOK || rejected != "1" || len(fx.events) != 1 {
			t.Fatalf("status = %d body = %s events = %d, want one call recorded and one rejected", rec.Code, rec.Body, len(fx.events))
		}
		if !strings.HasPrefix(message, "model-call spans rejected: 1 without a trace id or span id. Also: message content of 1 model calls was not stored") {
			t.Errorf("message = %q, want the rejection and then the warning", message)
		}
	})

	t.Run("in protobuf", func(t *testing.T) {
		fx := newFixture(t)
		rec := fx.post(t, otlp.MediaTypeProtobuf, export(t, withContent(t, 1)))
		if rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte("this server does not capture message content")) {
			t.Errorf("status = %d body = %q, want the warning in the partial success message", rec.Code, rec.Body)
		}
	})

	t.Run("not for what the client cannot change", func(t *testing.T) {
		// A resend whose content is already stored, and content dropped because the payload queue is busy.
		fx := capturing(t)
		body := mustJSON(t, withContent(t, 1))
		fx.post(t, otlp.MediaTypeJSON, body)
		if rec := fx.post(t, otlp.MediaTypeJSON, body); rec.Body.String() != "{}" {
			t.Errorf("resend: body = %s, want the empty success", rec.Body)
		}
		busy := capturing(t)
		busy.payloads.capacity = 0
		if rec := busy.post(t, otlp.MediaTypeJSON, body); rec.Body.String() != "{}" || len(busy.payloads.records) != 0 {
			t.Errorf("busy payload queue: body = %s records = %d, want the empty success and nothing stored", rec.Body, len(busy.payloads.records))
		}
		// An export that carries no content says nothing either.
		quiet := newFixture(t)
		if rec := quiet.post(t, otlp.MediaTypeJSON, mustJSON(t, chatSpan(1, "acme-large"))); rec.Body.String() != "{}" {
			t.Errorf("no content sent: body = %s", rec.Body)
		}
	})
}

func TestContentRefusedByPolicyCanBeStoredOnceThePolicyAllowsIt(t *testing.T) {
	no := false
	fx := newFixtureWith(t, fixtureOptions{policyCaptures: &no})
	fx.captureContent, fx.payloads.enabled = true, true
	body := export(t, pbSpan(1, contentAttrs(t, asText)...))
	if rec := fx.post(t, otlp.MediaTypeProtobuf, body); rec.Code != http.StatusOK || len(fx.payloads.records) != 0 {
		t.Fatalf("status = %d payload records = %d, want none under a policy that does not capture", rec.Code, len(fx.payloads.records))
	}
	// Refused content was never marked as stored, so the same call reported through a key with no policy still lands.
	if rec := fx.post(t, otlp.MediaTypeProtobuf, body, "Authorization", "Bearer "+siblingKey); rec.Code != http.StatusOK || len(fx.events) != 1 || len(fx.payloads.records) != 1 {
		t.Errorf("status = %d events = %d payload records = %d, want the one usage row and the content", rec.Code, len(fx.events), len(fx.payloads.records))
	}
}

func TestAudioTokensAreRecorded(t *testing.T) {
	fx := newFixture(t)
	rec := fx.post(t, otlp.MediaTypeProtobuf, export(t, pbSpan(1,
		str("gen_ai.operation.name", "chat"),
		str("gen_ai.request.model", "acme-large"),
		num("gen_ai.usage.input_tokens", 1000),
		num("gen_ai.usage.audio.input_tokens", 250),
		num("gen_ai.usage.output_tokens", 300),
		num("gen_ai.usage.audio.output_tokens", 120),
	)))
	if rec.Code != http.StatusOK || len(fx.events) != 1 {
		t.Fatalf("status = %d events = %d", rec.Code, len(fx.events))
	}
	// Audio is a breakdown of input and output, as on proxied events, so the totals are not reduced.
	want := map[string]int64{"input": 1000, "output": 300, "audio_input": 250, "audio_output": 120}
	got := fx.events[0].Tokens
	if len(got) != len(want) {
		t.Errorf("tokens = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("tokens[%s] = %d, want %d", k, got[k], v)
		}
	}
}
