package usagelog

import (
	"errors"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/pkg/catalogkind/pricing"
	"github.com/wyolet/relay/pkg/lifecycle"
	"github.com/wyolet/relay/pkg/meta"
	"github.com/wyolet/relay/pkg/usage"
	sdkusage "github.com/wyolet/relay/sdk/usage"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// stubTranslator hands buildEvent fixed canonical usage without pulling a
// real vendor adapter into the test binary. Its streams are the identity, so
// a canonical SSE body summarizes as-is.
type stubTranslator struct {
	tokens      sdkusage.Tokens
	serviceTier string
	err         error
}

func (stubTranslator) ParseRequest([]byte) (*v1.Request, error)     { return nil, nil }
func (stubTranslator) SerializeRequest(*v1.Request) ([]byte, error) { return nil, nil }
func (s stubTranslator) ParseResponse([]byte) (*v1.Response, error) {
	return &v1.Response{Usage: s.tokens, ServiceTier: s.serviceTier, FinishReason: v1.FinishReasonStop}, s.err
}
func (stubTranslator) SerializeResponse(*v1.Response, *v1.Request) ([]byte, error) {
	return nil, nil
}
func (stubTranslator) NewToCanonicalStream() func([]byte) ([]byte, error)   { return nil }
func (stubTranslator) NewFromCanonicalStream() func([]byte) ([]byte, error) { return nil }

// testPricer prices "pr-1": input 5, output 25 USD per million, input 10 on the priority tier.
func testPricer() *Pricer {
	sheet := &pricing.Pricing{
		Meta: meta.Metadata{ID: "pr-1", Name: "opus-sheet"},
		Spec: pricing.Spec{Currency: "USD", Rates: []pricing.Rate{
			{Meter: pricing.MeterTokensInput, Unit: pricing.UnitPerMillion, Amount: 5},
			{Meter: pricing.MeterTokensOutput, Unit: pricing.UnitPerMillion, Amount: 25},
			{Meter: pricing.MeterTokensInput, Unit: pricing.UnitPerMillion, Amount: 10, ServiceTier: "priority"},
		}},
	}
	return NewPricer(func(id string) (*pricing.Pricing, bool) { return sheet, id == "pr-1" })
}

var eventStart = time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

// fullContext is a request with every field the usage event copies set to a distinct value.
func fullContext() *lifecycle.Context {
	lc := lifecycle.NewContext("req-1", "pipeline", eventStart)
	lc.Timing.End = 1500 * time.Millisecond
	lc.Timing.Upstream = lifecycle.UpstreamTiming{Start: 3 * time.Millisecond, ResponseStart: 240 * time.Millisecond, ResponseEnd: 1400 * time.Millisecond}
	lc.Timing.Reasoning = lifecycle.ReasoningTiming{Start: 250 * time.Millisecond, End: 900 * time.Millisecond}
	lc.Streamed, lc.Attempts = true, 2
	lc.RelayKeyHash, lc.RelayKeyName = "hash", "indexer-prod"
	lc.PolicyID, lc.PolicyName = "pol-id", "default"
	lc.ModelID, lc.ModelName, lc.RequestedModel = "model-id", "opus", "opus[1m]"
	lc.HostID, lc.HostName, lc.HostKeyID = "host-id", "acme", "hostkey-hash"
	lc.ProviderName = "acme-labs"
	lc.PoolID, lc.AccountID, lc.ViaAccountID, lc.LaneAccountID = "pool-id", "acct-real", "acct-share", "acct-lane"
	lc.ProjectID, lc.ProjectName = "p1", "ml-search"
	lc.TeamID, lc.TeamName = "t1", "platform"
	lc.PrincipalKind, lc.PrincipalID, lc.PrincipalName = "serviceaccount", "sa1", "indexer"
	lc.CredentialKind, lc.CredentialID = "key", "k1"
	lc.PricingID, lc.PricingName = "pr-1", "opus-sheet"
	lc.Metadata["client_ip"] = "10.0.0.1"
	lc.Metadata["attempt_count"] = 3 // not a string: never an extra
	lc.Metadata[MetadataKeyRequestTags] = `{"session_id":"s1"}`
	lc.SetResponseSummary(v1.Summary{Tokens: sdkusage.Tokens{"input": 1000, "output": 200}, FinishReason: v1.FinishReasonStop, ServiceTier: "priority"})
	return lc
}

func fullEvent() Event {
	cost := int64(15_000_000) // 1000 × 10000 + 200 × 25000 nanos
	return Event{
		RequestID: "req-1", Source: "pipeline", Timestamp: eventStart,
		Status: 200, DurationMs: 1500, Streamed: true, FinishReason: "stop", Attempts: 2,
		Upstream:     &UpstreamTiming{Start: 3000, ResponseStart: 240_000, ResponseEnd: 1_400_000},
		Reasoning:    &ReasoningTiming{Start: 250_000, End: 900_000},
		RelayKeyHash: "hash", RelayKey: "indexer-prod",
		PolicyID: "pol-id", Policy: "default",
		ModelID: "model-id", Model: "opus", RequestedModel: "opus[1m]",
		HostID: "host-id", Host: "acme", HostKeyID: "hostkey-hash", Provider: "acme-labs",
		PoolID: "pool-id", AccountID: "acct-real", ViaAccountID: "acct-share", LaneAccountID: "acct-lane",
		ProjectID: "p1", Project: "ml-search", TeamID: "t1", Team: "platform",
		PrincipalKind: "serviceaccount", PrincipalID: "sa1", Principal: "indexer",
		CredentialKind: "key", CredentialID: "k1",
		Tokens:    sdkusage.Tokens{"input": 1000, "output": 200},
		Extras:    map[string]string{"client_ip": "10.0.0.1", ExtrasKeyServiceTier: "priority", ExtrasKeyInstance: "relay-0"},
		Tags:      map[string]string{"session_id": "s1"},
		CostNanos: &cost, CostBreakdown: map[string]int64{"tokens.input": 10_000_000, "tokens.output": 5_000_000},
		Pricing: "opus-sheet",
	}
}

// The event is the lifecycle Context copied field for field plus the
// outcome: nothing is re-resolved post-flight.
func TestUsageHook_FillCopiesTheContext(t *testing.T) {
	hook := NewUsageHook(testPricer(), "relay-0")
	if hook.Name() != Namespace {
		t.Fatalf("Name = %q, want %q", hook.Name(), Namespace)
	}
	got, err := hook.Fill(fullContext(), &lifecycle.PostFlightEvent{Status: 200})
	if err != nil {
		t.Fatalf("Fill: %v", err)
	}
	if diff := cmp.Diff(fullEvent(), *got.(*Event)); diff != "" {
		t.Fatalf("event (-want +got):\n%s", diff)
	}

	// A refusal carries its class and the runner's error; it never reached an upstream.
	lc := lifecycle.NewContext("req-2", "proxy", eventStart)
	got, _ = hook.Fill(lc, &lifecycle.PostFlightEvent{Status: 403, ErrorKind: "model_not_allowed", ErrorMessage: "not on the policy", Refusal: "model_not_allowed"})
	want := Event{RequestID: "req-2", Source: "proxy", Timestamp: eventStart, Status: 403,
		ErrorKind: "model_not_allowed", ErrorMessage: "not on the policy", Refusal: "model_not_allowed",
		Extras: map[string]string{ExtrasKeyInstance: "relay-0"}}
	if diff := cmp.Diff(want, *got.(*Event)); diff != "" {
		t.Fatalf("refusal event (-want +got):\n%s", diff)
	}

	// No instance id: no Extras at all.
	got, _ = NewUsageHook(nil, "").Fill(lc, &lifecycle.PostFlightEvent{Status: 200})
	if ev := got.(*Event); ev.Extras != nil {
		t.Fatalf("extras = %v, want none", ev.Extras)
	}
}

func TestBuildEvent_Timestamp(t *testing.T) {
	lc := lifecycle.NewContext("req", "pipeline", eventStart)
	if ev := buildEvent(lc, 200, "", "", nil, nil); !ev.Timestamp.Equal(eventStart) {
		t.Fatalf("timestamp = %v, want the request start", ev.Timestamp)
	}
	// An event time overrides the timestamp only: durations stay anchored on Start.
	lc.EventTime = eventStart.Add(-time.Hour)
	lc.Timing.End = 2 * time.Second
	if ev := buildEvent(lc, 200, "", "", nil, nil); !ev.Timestamp.Equal(lc.EventTime) || ev.DurationMs != 2000 {
		t.Fatalf("timestamp = %v, duration = %d; want the event time and 2000", ev.Timestamp, ev.DurationMs)
	}
	before := time.Now()
	ev := buildEvent(lifecycle.NewContext("req", "pipeline", time.Time{}), 200, "", "", nil, nil)
	if ev.Timestamp.Before(before) || ev.Timestamp.After(time.Now()) {
		t.Fatalf("timestamp of a request with no start = %v, want now", ev.Timestamp)
	}
}

// Caller tags are best-effort: a blob that does not validate drops the tags
// and nothing else, and the raw header never reaches Extras.
func TestBuildEvent_Tags(t *testing.T) {
	for raw, want := range map[string]map[string]string{
		`{"session_id":"s1","leg":"translate"}`: {"session_id": "s1", "leg": "translate"},
		`{"a":1}`:                               nil,
		``:                                      nil,
	} {
		lc := lifecycle.NewContext("req", "pipeline", eventStart)
		lc.Metadata[MetadataKeyRequestTags] = raw
		ev := buildEvent(lc, 200, "", "", nil, nil)
		if diff := cmp.Diff(want, ev.Tags); diff != "" || ev.Extras != nil || ev.Status != 200 {
			t.Errorf("tags %q: extras %v, status %d, tags (-want +got):\n%s", raw, ev.Extras, ev.Status, diff)
		}
	}
}

// The Summary the runner decoded wins; otherwise the body is summarized with
// the request's translator, and a body that cannot be leaves the row token-less.
func TestBuildEvent_SummarySource(t *testing.T) {
	recorded := v1.Summary{Tokens: sdkusage.Tokens{"input": 7}, FinishReason: v1.FinishReasonLength}
	decoded := stubTranslator{tokens: sdkusage.Tokens{"input": 1000, "output": 200}}
	for _, tc := range []struct {
		name       string
		recorded   bool
		tr         v1.Translator
		body       string
		wantTokens sdkusage.Tokens
		wantFinish string
	}{
		{"recorded summary", true, decoded, `{}`, sdkusage.Tokens{"input": 7}, "length"},
		{"decoded from the body", false, decoded, `{}`, sdkusage.Tokens{"input": 1000, "output": 200}, "stop"},
		{"empty body", false, decoded, ``, nil, ""},
		{"no translator", false, nil, `{}`, nil, ""},
		{"undecodable body", false, stubTranslator{tokens: sdkusage.Tokens{"input": 1}, err: errors.New("bad body")}, `{}`, nil, ""},
	} {
		lc := lifecycle.NewContext("req", "pipeline", eventStart)
		if tc.recorded {
			lc.SetResponseSummary(recorded)
		}
		if tc.tr != nil {
			lc.Translator = tc.tr
		}
		ev := buildEvent(lc, 200, "", "", []byte(tc.body), nil)
		if diff := cmp.Diff(tc.wantTokens, ev.Tokens); diff != "" || ev.FinishReason != tc.wantFinish {
			t.Errorf("%s: finish %q, want %q; tokens (-want +got):\n%s", tc.name, ev.FinishReason, tc.wantFinish, diff)
		}
	}
}

// Unpriced is never a stamped zero: the cost is present only when a sheet
// resolved and the tokens matched its meters. The pricing slug is kept either way.
func TestBuildEvent_Cost(t *testing.T) {
	nanos := func(n int64) *int64 { return &n }
	tokens := sdkusage.Tokens{"input": 1000, "output": 200}
	for _, tc := range []struct {
		name      string
		pricer    *Pricer
		pricingID string
		tokens    sdkusage.Tokens
		tier      string
		want      *int64
		breakdown map[string]int64
	}{
		{"priced", testPricer(), "pr-1", tokens, "", nanos(10_000_000), map[string]int64{"tokens.input": 5_000_000, "tokens.output": 5_000_000}},
		{"priced at the served tier", testPricer(), "pr-1", tokens, "priority", nanos(15_000_000), map[string]int64{"tokens.input": 10_000_000, "tokens.output": 5_000_000}},
		{"tier the sheet does not price", testPricer(), "pr-1", tokens, "flex", nanos(10_000_000), map[string]int64{"tokens.input": 5_000_000, "tokens.output": 5_000_000}},
		{"zero counts are a real zero", testPricer(), "pr-1", sdkusage.Tokens{"input": 0, "output": 0}, "", nanos(0), map[string]int64{"tokens.input": 0, "tokens.output": 0}},
		{"no meter matches", testPricer(), "pr-1", sdkusage.Tokens{"images": 3}, "", nil, nil},
		{"no pricing stamped", testPricer(), "", tokens, "", nil, nil},
		{"no tokens", testPricer(), "pr-1", nil, "", nil, nil},
		{"sheet gone from the snapshot", testPricer(), "gone", tokens, "", nil, nil},
		{"nil pricer", nil, "pr-1", tokens, "", nil, nil},
		{"pricer without a lookup", NewPricer(nil), "pr-1", tokens, "", nil, nil},
	} {
		lc := lifecycle.NewContext("req", "pipeline", eventStart)
		lc.PricingID, lc.PricingName = tc.pricingID, "sheet-slug"
		lc.SetResponseSummary(v1.Summary{Tokens: tc.tokens, ServiceTier: tc.tier})
		ev := buildEvent(lc, 200, "", "", nil, tc.pricer)
		if diff := cmp.Diff(tc.want, ev.CostNanos); diff != "" {
			t.Errorf("%s: cost (-want +got):\n%s", tc.name, diff)
		}
		if diff := cmp.Diff(tc.breakdown, ev.CostBreakdown); diff != "" {
			t.Errorf("%s: breakdown (-want +got):\n%s", tc.name, diff)
		}
		if ev.Pricing != "sheet-slug" || ev.Extras[ExtrasKeyServiceTier] != tc.tier {
			t.Errorf("%s: pricing slug %q, service tier %q", tc.name, ev.Pricing, ev.Extras[ExtrasKeyServiceTier])
		}
	}
}

// Stored counts keep the provider's meaning (input and output include their
// parts), but a part with its own rate is charged once, at that rate.
func TestBuildEvent_PartsChargedOnce(t *testing.T) {
	sheet := &pricing.Pricing{Spec: pricing.Spec{Currency: "USD", Rates: []pricing.Rate{
		{Meter: pricing.MeterTokensInput, Unit: pricing.UnitPerMillion, Amount: 0.43},
		{Meter: pricing.MeterTokensOutput, Unit: pricing.UnitPerMillion, Amount: 1.66},
		{Meter: pricing.MeterTokensAudioInput, Unit: pricing.UnitPerMillion, Amount: 3.81},
		{Meter: pricing.MeterTokensAudioOutput, Unit: pricing.UnitPerMillion, Amount: 15.11},
	}}}
	tokens := sdkusage.Tokens{"input": 1200, "output": 500, "reasoning": 40, "audio_input": 200, "audio_output": 300}
	lc := lifecycle.NewContext("req-parts", "pipeline", eventStart)
	lc.PricingID = "pr-omni"
	lc.SetResponseSummary(v1.Summary{Tokens: tokens})
	ev := buildEvent(lc, 200, "", "", nil, NewPricer(func(string) (*pricing.Pricing, bool) { return sheet, true }))

	if diff := cmp.Diff(tokens, ev.Tokens); diff != "" {
		t.Errorf("tokens (-want +got):\n%s", diff)
	}
	// 1000 text input × 430 + 200 audio input × 3810 + 200 text output × 1660
	// (reasoning has no rate, so it stays in output) + 300 audio output × 15110.
	wantBreakdown := map[string]int64{
		"tokens.input": 430_000, "tokens.audio_input": 762_000,
		"tokens.output": 332_000, "tokens.audio_output": 4_533_000,
	}
	if ev.CostNanos == nil || *ev.CostNanos != 6_057_000 {
		t.Fatalf("cost = %v, want 6057000", ev.CostNanos)
	}
	if diff := cmp.Diff(wantBreakdown, ev.CostBreakdown); diff != "" {
		t.Errorf("breakdown (-want +got):\n%s", diff)
	}
}

// An upstream status of 400 or above always carries the upstream-error kind; a kind the runner already set is kept, and a status below 400 gets none. The response body never reaches the message.
func TestUsageHook_ErrorStatusCarriesUpstreamErrorKind(t *testing.T) {
	cases := []struct {
		name     string
		ev       lifecycle.PostFlightEvent
		wantKind string
		wantMsg  string
		logOnly  bool
	}{
		{"success", lifecycle.PostFlightEvent{Status: 200}, "", "", false},
		{"redirect", lifecycle.PostFlightEvent{Status: 399}, "", "", false},
		{"passed-through 400", lifecycle.PostFlightEvent{Status: 400, ResponseBody: []byte(`{"error":{"message":"prompt: secret text"}}`)}, usage.ErrorKindUpstream, "upstream returned 400", false},
		{"passed-through 503", lifecycle.PostFlightEvent{Status: 503}, usage.ErrorKindUpstream, "upstream returned 503", false},
		{"runner-classified failure", lifecycle.PostFlightEvent{Status: 429, ErrorKind: "upstream_error", ErrorMessage: "upstream returned 429: slow down"}, usage.ErrorKindUpstream, "upstream returned 429: slow down", false},
		{"runner kind without a message", lifecycle.PostFlightEvent{Status: 502, ErrorKind: "bad_gateway"}, "bad_gateway", "", false},
		{"rejected before upstream", lifecycle.PostFlightEvent{ErrorKind: "rate_limited", ErrorMessage: "quota"}, "rate_limited", "quota", true},
	}
	hook := NewUsageHook(nil, "")
	for _, tc := range cases {
		v, err := hook.Fill(lifecycle.NewContext("req", "pipeline", eventStart), &tc.ev)
		if err != nil {
			t.Fatalf("%s: Fill: %v", tc.name, err)
		}
		got := v.(*Event)
		if got.Status != tc.ev.Status || got.ErrorKind != tc.wantKind || got.ErrorMessage != tc.wantMsg || got.LogOnly() != tc.logOnly {
			t.Errorf("%s: status=%d kind=%q message=%q logOnly=%v, want status=%d kind=%q message=%q logOnly=%v",
				tc.name, got.Status, got.ErrorKind, got.ErrorMessage, got.LogOnly(), tc.ev.Status, tc.wantKind, tc.wantMsg, tc.logOnly)
		}
	}
}
