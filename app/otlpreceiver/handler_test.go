package otlpreceiver_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	"github.com/wyolet/relay/pkg/otlp"
	"github.com/wyolet/relay/pkg/otlp/genai"
	"github.com/wyolet/relay/pkg/slug"
)

const relayKey = "sk-wr-reporter"

type rows[T any] []*T

func (r rows[T]) List(context.Context) ([]*T, error) { return r, nil }

// fixture is a receiver in front of a catalog holding one model served by two hosts: the provider's own (priced) and a reseller (priced higher).
type fixture struct {
	handler  http.Handler
	enabled  bool
	events   []usagelog.Event
	project  *project.Project
	team     *team.Team
	sa       *serviceaccount.ServiceAccount
	keyRow   *key.Key
	model    *model.Model
	ownPrice *pricing.Pricing
}

func system() meta.Owner { return meta.Owner{Kind: meta.OwnerSystem} }

func perMillion(m pricing.Meter, usd float64) pricing.Rate {
	return pricing.Rate{Meter: m, Unit: pricing.UnitPerMillion, Amount: usd}
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	fx := &fixture{enabled: true}

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

	cat := appcatalog.New(
		rows[provider.Provider]{prov},
		rows[host.Host]{ownHost, reseller},
		rows[policy.Policy]{},
		rows[model.Model]{fx.model},
		rows[hostkey.HostKey]{},
		rows[ratelimit.RateLimit]{},
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

	h := otlpreceiver.New(otlpreceiver.Options{
		Enabled:    func() bool { return fx.enabled },
		Snapshot:   cat.Current,
		Emit:       func(ev usagelog.Event) { fx.events = append(fx.events, ev) },
		Pricer:     usagelog.NewPricer(func(id string) (*pricing.Pricing, bool) { return cat.Current().Pricing(id) }),
		Mappers:    []otlp.SpanMapper{genai.Mapper{}},
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
