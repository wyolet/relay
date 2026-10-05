package main

import (
	"log/slog"

	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/otlpreceiver"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/app/usagelog"
	"github.com/wyolet/relay/internal/config"
	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/otlp"
	"github.com/wyolet/relay/pkg/otlp/genai"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

// otlpProviderHints maps gen_ai.provider.name values (and the gen_ai.system values that preceded them) to catalog slugs, for the names that differ from the catalog's or that name a serving host. A name equal to its catalog provider slug needs no entry, and a name with no catalog counterpart is left out: its models still resolve by bare name.
var otlpProviderHints = map[string]otlpreceiver.ProviderHint{
	"gcp.gemini":      {Provider: "google", Host: "google"},
	"gemini":          {Provider: "google", Host: "google"},
	"gcp.gen_ai":      {Provider: "google"},
	"azure.ai.openai": {Provider: "openai"},
	"az.ai.openai":    {Provider: "openai"},
	"x_ai":            {Provider: "xai", Host: "xai"},
	"mistral_ai":      {Provider: "mistral", Host: "mistral"},
	"moonshot_ai":     {Provider: "moonshot", Host: "moonshotai"},
	"ibm.watsonx.ai":  {Provider: "ibm"},
}

// buildOTLPReceiver wires the OpenTelemetry receiver onto the usage emitter proxied traffic uses, so reported model calls land in the same store. The telemetry conventions it understands are registered here; a span is offered to each in order.
func buildOTLPReceiver(cfg *config.Config, cat *appcatalog.Catalog, usageCtl *usagelog.Controller, kvStore kv.Store, limiter *pkgratelimit.Limiter) *otlpreceiver.Handler {
	var markers *otlpreceiver.Markers
	if scripter, ok := kvStore.(kv.Scripter); ok {
		markers = otlpreceiver.NewMarkers(scripter)
	} else {
		slog.Warn("otlp receiver: kv store runs no scripts; resent exports will be recorded again")
	}
	return otlpreceiver.New(otlpreceiver.Options{
		Enabled:  func() bool { return settings.OTLPReceiverFrom(cat).Enabled },
		Snapshot: cat.Current,
		Usage:    usageCtl.Emitter(),
		Markers:  markers,
		Limiter:  limiter,
		Pricer: usagelog.NewPricer(func(id string) (*pricing.Pricing, bool) {
			return cat.Current().Pricing(id)
		}),
		Mappers:       []otlp.SpanMapper{genai.Mapper{}},
		ProviderHints: otlpProviderHints,
		InstanceID:    cfg.InstanceID,
		MaxBodyBytes:  cfg.MaxRequestBytes,
	})
}
