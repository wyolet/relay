package otlpreceiver_test

import (
	"net/http"
	"testing"

	"github.com/wyolet/relay/pkg/httpheader"
	"github.com/wyolet/relay/pkg/otlp"
)

func TestEveryAcceptedExportAnswersWhetherContentIsStored(t *testing.T) {
	yes, no := true, false
	for _, tc := range []struct {
		name                           string
		opts                           fixtureOptions
		receiverOff, payloadLoggingOff bool
		want                           string
	}{
		{name: "policy stores", opts: fixtureOptions{policyCaptures: &yes}, want: httpheader.ContentCaptureStore},
		{name: "policy does not store", opts: fixtureOptions{policyCaptures: &no}, want: httpheader.ContentCaptureDrop},
		{name: "policy does not store, key flag set", opts: fixtureOptions{policyCaptures: &no, keyCaptures: true}, want: httpheader.ContentCaptureDrop},
		{name: "no policy", want: httpheader.ContentCaptureClient},
		{name: "receiver switch off", opts: fixtureOptions{policyCaptures: &yes}, receiverOff: true, want: httpheader.ContentCaptureDrop},
		{name: "payload logging off", payloadLoggingOff: true, want: httpheader.ContentCaptureDrop},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newFixtureWith(t, tc.opts)
			fx.captureContent = !tc.receiverOff
			fx.payloads.enabled = !tc.payloadLoggingOff

			// An empty logs export is how a client asks before it has calls to send.
			rec := fx.postLogs(t, otlp.MediaTypeJSON, []byte(`{"resourceLogs":[]}`))
			if rec.Code != http.StatusOK || rec.Header().Get(httpheader.HeaderContentCapture) != tc.want {
				t.Errorf("empty logs export: status %d %s = %q, want 200 and %q", rec.Code, httpheader.HeaderContentCapture, rec.Header().Get(httpheader.HeaderContentCapture), tc.want)
			}
			rec = fx.post(t, otlp.MediaTypeProtobuf, export(t, chatSpan(1, "acme-large")))
			if rec.Code != http.StatusOK || rec.Header().Get(httpheader.HeaderContentCapture) != tc.want {
				t.Errorf("trace export: status %d %s = %q, want 200 and %q", rec.Code, httpheader.HeaderContentCapture, rec.Header().Get(httpheader.HeaderContentCapture), tc.want)
			}
		})
	}
}

func TestARefusedExportCarriesNoAnswer(t *testing.T) {
	fx := newFixture(t)
	fx.capacity = 0
	rec := fx.post(t, otlp.MediaTypeProtobuf, export(t, chatSpan(1, "acme-large")))
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get(httpheader.HeaderContentCapture) != "" {
		t.Errorf("status %d answer %q, want 503 without an answer", rec.Code, rec.Header().Get(httpheader.HeaderContentCapture))
	}
}

func TestTheReportedHostPricesTheCall(t *testing.T) {
	record := func(t *testing.T, provider, modelName, host string) (pricing string, cost int64) {
		t.Helper()
		fx := newFixture(t)
		rec := fx.post(t, otlp.MediaTypeProtobuf, export(t, pbSpan(1,
			str("gen_ai.operation.name", "chat"),
			str("gen_ai.provider.name", provider),
			str("gen_ai.request.model", modelName),
			str("wyolet.relay.host", host),
			num("gen_ai.usage.input_tokens", 1000),
			num("gen_ai.usage.output_tokens", 300),
		)))
		if rec.Code != http.StatusOK || len(fx.events) != 1 || fx.events[0].CostNanos == nil {
			t.Fatalf("status = %d events = %d", rec.Code, len(fx.events))
		}
		return fx.events[0].Pricing, *fx.events[0].CostNanos
	}

	if pricing, cost := record(t, "acme", "acme-large", "a-reseller"); pricing != "reseller-markup" || cost != 75_000_000 {
		t.Errorf("reported reseller: pricing %q cost %d, want reseller-markup / 75000000", pricing, cost)
	}
	// The reported host goes before a provider hint's host.
	if pricing, _ := record(t, "acme.cloud", "acme-large", "acme"); pricing != "acme-large-list" {
		t.Errorf("reported own host over the hinted reseller: pricing %q", pricing)
	}
	if pricing, _ := record(t, "acme", "acme-large", "no-such-host"); pricing != "acme-large-list" {
		t.Errorf("unknown host: pricing %q, want the provider's own host", pricing)
	}
	if pricing, _ := record(t, "acme.cloud", "acme-large", "no-such-host"); pricing != "reseller-markup" {
		t.Errorf("unknown host with a provider hint: pricing %q, want the hinted host", pricing)
	}
}
