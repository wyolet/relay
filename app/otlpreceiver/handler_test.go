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
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
	"github.com/wyolet/relay/pkg/slug"
)

const relayKey = "sk-wr-reporter"

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
	project     *project.Project
	team        *team.Team
	sa          *serviceaccount.ServiceAccount
	keyRow      *key.Key
	model       *model.Model
	ownPrice    *pricing.Pricing
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

type fixtureOptions struct {
	// exportsPerMinute above zero adds the system rate limit on exports with that budget.
	exportsPerMinute int64
	// markerStore replaces the in-memory store behind the duplicate markers.
	markerStore kv.Scripter
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
	fx := &fixture{enabled: true, capacity: usagelog.DefaultQueueSize}

	fx.team = &team.Team{Meta: meta.Metadata{ID: meta.NewID(), Name: "platform", Owner: system()}}
	fx.project = &project.Project{Meta: meta.Metadata{ID: meta.NewID(), Name: "ml-search"}, Spec: project.Spec{TeamID: fx.team.Meta.ID}}
	fx.project.StampOwner()
	// No policy anywhere on this principal: reporting needs a credential, not a route.
	fx.sa = &serviceaccount.ServiceAccount{Meta: meta.Metadata{ID: meta.NewID(), Name: "indexer"}, Spec: serviceaccount.Spec{ProjectID: fx.project.Meta.ID}}
	fx.sa.StampOwner()
	sum := sha256.Sum256([]byte(relayKey))
	fx.keyRow = &key.Key{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "indexer-prod", Owner: meta.Owner{Kind: meta.OwnerProject, ID: fx.project.Meta.ID}},
		Spec: key.Spec{Principal: key.Principal{Kind: key.PrincipalServiceAccount, ID: fx.sa.Meta.ID}, KeyHash: hex.EncodeToString(sum[:])},
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
		limits = append(limits, &ratelimit.RateLimit{
			Meta: meta.Metadata{ID: meta.NewID(), Name: otlpreceiver.RateLimitName, Owner: system()},
			Spec: ratelimit.Spec{Rules: []ratelimit.Rule{
				{Meter: ratelimit.MeterRequests, Amount: o.exportsPerMinute, Window: ratelimit.Window(time.Minute), Strategy: ratelimit.StrategySlidingWindow},
				// Not a requests rule, so it must not apply: nothing would ever release the slot.
				{Meter: ratelimit.MeterConcurrency, Amount: 1, Window: ratelimit.Window(time.Minute), Strategy: ratelimit.StrategySlidingWindow},
			}},
		})
	}

	cat := appcatalog.New(
		rows[provider.Provider]{prov, zeta},
		rows[host.Host]{ownHost, reseller},
		rows[policy.Policy]{},
		rows[model.Model]{fx.model, fx.sharedAcme, fx.sharedZeta},
		rows[hostkey.HostKey]{},
		limits,
		rows[key.Key]{fx.keyRow},
		rows[pricing.Pricing]{fx.ownPrice, resellerPrice},
		bindings,
	)
	cat.UseTenancy(
		rows[team.Team]{fx.team},
		rows[project.Project]{fx.project},
		rows[serviceaccount.ServiceAccount]{fx.sa},
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
		Enabled:  func() bool { return fx.enabled },
		Snapshot: cat.Current,
		Usage:    fx,
		Markers:  markers,
		Limiter:  pkgratelimit.New(state, slog.New(slog.DiscardHandler), nil),
		Pricer:   usagelog.NewPricer(func(id string) (*pricing.Pricing, bool) { return cat.Current().Pricing(id) }),
		Mappers:  []otlp.SpanMapper{genai.Mapper{}},
		ProviderHints: map[string]otlpreceiver.ProviderHint{
			"acme.cloud": {Provider: "acme", Host: "a-reseller"},
			"zeta_ai":    {Provider: "zeta"},
		},
		InstanceID: "pod-a",
	})
	fx.handler = inference.ClassifyMiddleware()(inference.AuthenticateMiddleware(cat, nil)(h))
	return fx
}

func (fx *fixture) post(t *testing.T, contentType string, body []byte, header ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, otlpreceiver.TracesPath, bytes.NewReader(body))
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

func chatSpan(spanID byte, modelName string) *tracepb.Span {
	return pbSpan(spanID,
		str("gen_ai.operation.name", "chat"),
		str("gen_ai.provider.name", "acme"),
		str("gen_ai.request.model", modelName),
		str("gen_ai.response.id", "resp_123"),
		str("gen_ai.conversation.id", "conv_9"),
		num("gen_ai.usage.input_tokens", 1000),
		num("gen_ai.usage.cache_read.input_tokens", 700),
		num("gen_ai.usage.cache_write.input_tokens", 200),
		num("gen_ai.usage.output_tokens", 300),
	)
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
	if ev := fx.events[0]; ev.Status != 0 || ev.ErrorKind != "rate_limit_exceeded" || !ev.LogOnly() || ev.CostNanos != nil {
		t.Errorf("failed call without a status = %+v, want a log-only row", ev)
	}
	if ev := fx.events[1]; ev.Status != 429 || ev.ErrorKind != "rate_limit_exceeded" {
		t.Errorf("failed call with a status = status %d kind %q", ev.Status, ev.ErrorKind)
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
	fx.capacity = 3
	fx.events = make([]usagelog.Event, 2)
	body := mustJSON(t, chatSpan(1, "acme-large"), chatSpan(2, "acme-large"))

	rec := fx.post(t, otlp.MediaTypeJSON, body)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "1" {
		t.Fatalf("status = %d Retry-After = %q, want 503 and 1", rec.Code, rec.Header().Get("Retry-After"))
	}
	if code, _ := statusOf(t, rec); code != int(otlp.StatusUnavailable) {
		t.Errorf("status code = %d, want UNAVAILABLE", code)
	}
	if len(fx.events) != 2 {
		t.Fatalf("a refused export queued %d events", len(fx.events)-2)
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
	fx.capacity = 2
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
