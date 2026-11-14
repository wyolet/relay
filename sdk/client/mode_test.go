package client

import (
	"context"
	"net/http"
	"testing"

	"github.com/wyolet/relay/sdk/telemetry"
)

func clearModeEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{EnvMode, EnvBaseURL, EnvAPIKey, EnvHeaders, EnvUsage, EnvTimeout, EnvOpenAIKey, telemetry.EnvOTLPEndpoint, telemetry.EnvOTLPHeaders} {
		t.Setenv(k, "")
	}
}

func TestForMode_Direct(t *testing.T) {
	clearModeEnv(t)
	t.Setenv(EnvOpenAIKey, "sk-env")
	sink := newExportSink(t)
	for name, opts := range map[string][]ModeOption{
		"by default without relay configured":  nil,
		"WR_MODE=direct with relay configured": nil,
		"WithMode beats WR_MODE":               {WithMode(ModeDirect), WithEmitter(sink.emitter(t, false))},
	} {
		t.Run(name, func(t *testing.T) {
			switch name {
			case "WR_MODE=direct with relay configured":
				t.Setenv(EnvMode, "direct")
				t.Setenv(EnvBaseURL, "http://relay.example")
				t.Setenv(EnvAPIKey, "rk")
			case "WithMode beats WR_MODE":
				t.Setenv(EnvMode, "telemetry")
			}
			c, err := ForMode("gpt-4o@openai", opts...)
			if err != nil {
				t.Fatal(err)
			}
			if c.telemetry != nil || c.ownsTelemetry || c.relay != nil || c.apiKey != "sk-env" || c.target.host != "openai" || c.baseURL == "http://relay.example" {
				t.Errorf("telemetry %v relay %v key %q host %q base %q; want a keyed provider client with no telemetry", c.telemetry, c.relay, c.apiKey, c.target.host, c.baseURL)
			}
			if err := c.Close(); err != nil {
				t.Errorf("Close = %v", err)
			}
		})
	}

	c, err := ForMode("gpt-4o@openai", WithMode(ModeDirect), WithProviderKey("sk-explicit"))
	if err != nil || c.apiKey != "sk-explicit" {
		t.Errorf("WithProviderKey: key %q err %v, want the explicit key over the environment", c.apiKey, err)
	}
}

// With relay configured and no mode named, calls go through relay carrying the ref as the model, and no telemetry is attached.
func TestForMode_Proxy(t *testing.T) {
	clearModeEnv(t)
	rec := newRecorder(t, http.StatusOK, canonicalReply)
	t.Setenv(EnvBaseURL, rec.srv.URL)
	t.Setenv(EnvAPIKey, "rk")
	sink := newExportSink(t)
	c, err := ForMode("gpt-4o@openai", WithEmitter(sink.emitter(t, false)), WithClientOptions(WithHeader("X-Team", "red")))
	if err != nil {
		t.Fatal(err)
	}
	if c.telemetry != nil || c.relay == nil {
		t.Errorf("telemetry %v relay %v, want a relay client without telemetry", c.telemetry, c.relay)
	}
	req := sampleReq()
	req.Model = nil
	if _, err := c.Generate(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	got := rec.only(t)
	if got.Path != "/v1/generate" || got.Header.Get("Authorization") != "Bearer rk" || got.Header.Get("X-Team") != "red" || sentModel(t, got.Body) != "gpt-4o@openai" {
		t.Errorf("sent to %q headers %v body %s; want /v1/generate with the ref as the model", got.Path, got.Header, got.Body)
	}

	// The ref replaces a model the caller set, too.
	rec.mu.Lock()
	rec.reqs = nil
	rec.mu.Unlock()
	if _, err := c.Generate(context.Background(), sampleReq()); err != nil {
		t.Fatal(err)
	}
	if model := sentModel(t, rec.only(t).Body); model != "gpt-4o@openai" {
		t.Errorf("model sent = %q, want the ref", model)
	}

	t.Setenv(EnvAPIKey, "")
	if c, err := ForMode("gpt-4o@openai", WithMode(ModeProxy)); err != nil || c.configErr == nil {
		t.Errorf("proxy mode without a relay key: client error %v, construction error %v; want the config error held for the first call", c.configErr, err)
	}
}

func TestForMode_Telemetry(t *testing.T) {
	clearModeEnv(t)
	sink := newExportSink(t)
	t.Setenv(EnvMode, string(ModeTelemetry))
	t.Setenv(EnvBaseURL, sink.srv.URL)
	t.Setenv(EnvAPIKey, "rk")

	owned, err := ForMode("gpt-4o@openai", WithProviderKey("sk-explicit"))
	if err != nil {
		t.Fatal(err)
	}
	if owned.telemetry == nil || owned.telemetry.Err() != nil || !owned.ownsTelemetry || owned.apiKey != "sk-explicit" || owned.relay != nil {
		t.Fatalf("telemetry %v owned %v key %q relay %v; want a provider client owning an emitter built from the environment", owned.telemetry, owned.ownsTelemetry, owned.apiKey, owned.relay)
	}
	if err := owned.Close(); err != nil {
		t.Errorf("Close = %v", err)
	}

	e := sink.emitter(t, false)
	given, err := ForMode("gpt-4o@openai", WithEmitter(e))
	if err != nil {
		t.Fatal(err)
	}
	if given.telemetry != e || given.ownsTelemetry || given.Telemetry() != e {
		t.Error("a given emitter must be used and left to its owner")
	}
	if err := given.Close(); err != nil || e.Err() != nil {
		t.Errorf("Close = %v, emitter err %v; closing the client must leave a given emitter usable", err, e.Err())
	}
}

func TestForMode_Errors(t *testing.T) {
	clearModeEnv(t)
	t.Setenv(EnvMode, "relay")
	if c, err := ForMode("gpt-4o"); c != nil || errText(err) != `relay client: invalid mode "relay" (want proxy, telemetry or direct)` {
		t.Errorf("unknown WR_MODE: %v, %q", c, errText(err))
	}
	if c, err := ForMode("gpt-4o", WithMode("Proxy")); c != nil || errText(err) != `relay client: invalid mode "Proxy" (want proxy, telemetry or direct)` {
		t.Errorf("unknown WithMode: %v, %q", c, errText(err))
	}
	for _, mode := range []Mode{ModeDirect, ModeTelemetry} {
		if c, err := ForMode("no-such-model-anywhere", WithMode(mode)); c != nil || err == nil {
			t.Errorf("%s with an unresolvable ref: %v, %v; want the catalog's error", mode, c, err)
		}
	}
}
