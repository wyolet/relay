package main

import (
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/otlpreceiver"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/app/usagelog"
	"github.com/wyolet/relay/internal/config"
	"github.com/wyolet/relay/pkg/otlp"
	"github.com/wyolet/relay/pkg/otlp/genai"
)

// buildOTLPReceiver wires the OpenTelemetry receiver onto the usage emitter proxied traffic uses, so reported model calls land in the same store. The telemetry conventions it understands are registered here; a span is offered to each in order.
func buildOTLPReceiver(cfg *config.Config, cat *appcatalog.Catalog, usageCtl *usagelog.Controller) *otlpreceiver.Handler {
	return otlpreceiver.New(otlpreceiver.Options{
		Enabled:  func() bool { return settings.OTLPReceiverFrom(cat).Enabled },
		Snapshot: cat.Current,
		Emit:     usageCtl.Emitter().Emit,
		Pricer: usagelog.NewPricer(func(id string) (*pricing.Pricing, bool) {
			return cat.Current().Pricing(id)
		}),
		Mappers:      []otlp.SpanMapper{genai.Mapper{}},
		InstanceID:   cfg.InstanceID,
		MaxBodyBytes: cfg.MaxRequestBytes,
	})
}
