package inference

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/usagelog"
	"github.com/wyolet/relay/pkg/httpheader"
	"github.com/wyolet/relay/pkg/lifecycle"
)

func TestApplyObsHeaders(t *testing.T) {
	mk := func() *lifecycle.Context {
		return lifecycle.NewContext("req", "pipeline", time.Now())
	}
	h := http.Header{}
	h.Set(httpheader.HeaderEventTime, "2025-11-03T09:30:00.5Z")
	h.Set(httpheader.HeaderRequestTags, `{"session_id":"s1"}`)

	// Flag off: event time ignored, tags still captured raw (unparsed).
	lc := mk()
	applyObsHeaders(lc, h, false)
	if !lc.EventTime.IsZero() {
		t.Fatalf("flag off: want zero EventTime, got %v", lc.EventTime)
	}
	if lc.Metadata[usagelog.MetadataKeyRequestTags] != `{"session_id":"s1"}` {
		t.Fatalf("tags metadata: %+v", lc.Metadata)
	}

	// Flag on: RFC3339Nano parsed; Timing.Start untouched.
	lc = mk()
	start := lc.Timing.Start
	applyObsHeaders(lc, h, true)
	want := time.Date(2025, 11, 3, 9, 30, 0, 500_000_000, time.UTC)
	if !lc.EventTime.Equal(want) {
		t.Fatalf("EventTime: want %v, got %v", want, lc.EventTime)
	}
	if !lc.Timing.Start.Equal(start) {
		t.Fatal("Timing.Start must never change")
	}

	// Unparsable value is silently ignored even with the flag on.
	bad := http.Header{}
	bad.Set(httpheader.HeaderEventTime, "yesterday at noon")
	lc = mk()
	applyObsHeaders(lc, bad, true)
	if !lc.EventTime.IsZero() {
		t.Fatalf("bad value: want zero EventTime, got %v", lc.EventTime)
	}

	// Oversized tags header is never copied.
	big := http.Header{}
	big.Set(httpheader.HeaderRequestTags, `{"k":"`+strings.Repeat("v", usagelog.MaxTagsHeaderBytes)+`"}`)
	lc = mk()
	applyObsHeaders(lc, big, false)
	if _, ok := lc.Metadata[usagelog.MetadataKeyRequestTags]; ok {
		t.Fatal("oversized tags header must not be copied")
	}
}

// Both observability headers must sit inside the X-WR-* strip denylist so
// they never reach the upstream.
func TestObsHeadersAreStripped(t *testing.T) {
	for _, name := range []string{httpheader.HeaderEventTime, httpheader.HeaderRequestTags} {
		if !httpheader.Match(name, httpheader.StripDenylist) {
			t.Fatalf("%s is not covered by the strip denylist", name)
		}
	}
}

// applyRouteIdentity stamps ids and the denormalized slugs together: the innermost caller policy, the model, and the first hop's host and pricing.
func TestApplyRouteIdentity_IDsAndSlugs(t *testing.T) {
	lc := lifecycle.NewContext("req", "pipeline", time.Now())
	route := &plan.Route{Model: &model.Model{Meta: meta.Metadata{ID: "mid", Name: "gpt-4o"}}, Provider: "openai"}
	hops := []plan.RouteHop{{
		Host:    &host.Host{Meta: meta.Metadata{ID: "hid", Name: "openai"}},
		Pricing: &pricing.Pricing{Meta: meta.Metadata{ID: "prid", Name: "openai-gpt-4o"}},
	}}
	pol := &policy.Policy{Meta: meta.Metadata{ID: "pid", Name: "default"}}
	cp := &plan.CallerPlan{}
	cp.Rungs[2].Policy = &plan.ExpandedPolicy{Policy: pol}
	p := &Principal{Caller: cp}

	applyRouteIdentity(lc, route, hops, p)

	if lc.PolicyID != "pid" || lc.PolicyName != "default" {
		t.Fatalf("policy: %q/%q", lc.PolicyID, lc.PolicyName)
	}
	if lc.ModelID != "mid" || lc.ModelName != "gpt-4o" {
		t.Fatalf("model: %q/%q", lc.ModelID, lc.ModelName)
	}
	if lc.HostID != "hid" || lc.HostName != "openai" {
		t.Fatalf("host: %q/%q", lc.HostID, lc.HostName)
	}
	if lc.ProviderName != "openai" {
		t.Fatalf("provider: %q", lc.ProviderName)
	}
	if lc.PricingID != "prid" || lc.PricingName != "openai-gpt-4o" {
		t.Fatalf("pricing: %q/%q", lc.PricingID, lc.PricingName)
	}
	if _, ok := lc.Metadata["resolved_via"]; ok {
		t.Fatal("resolved_via stamped without alias resolution")
	}

	// An alias-resolved route stamps the resolved_via tag (read by the usage hook into Event.Extras).
	lcA := lifecycle.NewContext("req-a", "pipeline", time.Now())
	routeA := *route
	routeA.ResolvedVia = "alias:gpt-4o[1m]"
	applyRouteIdentity(lcA, &routeA, hops, &Principal{Caller: &plan.CallerPlan{}})
	if got := lcA.Metadata["resolved_via"]; got != "alias:gpt-4o[1m]" {
		t.Fatalf("resolved_via: %v", got)
	}
	if lcA.PolicyID != "" {
		t.Fatalf("policy stamped with no caller policy: %q", lcA.PolicyID)
	}
	applyRouteIdentity(nil, nil, nil, nil)
}
