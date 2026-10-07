package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wyolet/relay/sdk/telemetry"
	v1 "github.com/wyolet/relay/sdk/v1"
)

func clearModeEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{EnvMode, EnvBaseURL, EnvAPIKey, EnvOpenAIKey, telemetry.EnvOTLPEndpoint, telemetry.EnvOTLPHeaders} {
		t.Setenv(k, "")
	}
}

func TestForModeDefaultsToDirectWithoutRelay(t *testing.T) {
	clearModeEnv(t)
	t.Setenv(EnvOpenAIKey, "sk-env")
	c, err := ForMode("gpt-4o")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.telemetry != nil || c.target.adapter.translator == nil || c.apiKey != "sk-env" {
		t.Errorf("telemetry %v key %q, want a direct client with the vendor key from the environment", c.telemetry, c.apiKey)
	}
}

func TestForModeDefaultsToProxyWhenRelayIsConfigured(t *testing.T) {
	clearModeEnv(t)
	var model string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(raw, &req)
		model = req.Model
		b, _ := json.Marshal(&v1.Response{ID: "resp_1", Object: "response", Status: v1.StatusCompleted})
		_, _ = w.Write(b)
	}))
	defer srv.Close()
	t.Setenv(EnvBaseURL, srv.URL)
	t.Setenv(EnvAPIKey, "rk")
	sink := newExportSink(t)
	c, err := ForMode("gpt-4o@openai", WithEmitter(sink.emitter(t, false)))
	if err != nil {
		t.Fatal(err)
	}
	if c.telemetry != nil {
		t.Error("telemetry is on in proxy mode")
	}
	req := sampleReq()
	req.Model = nil
	if _, err := c.Generate(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if model != "gpt-4o@openai" {
		t.Errorf("relay got model %q, want the ref", model)
	}
}

func TestForModeTelemetryBuildsAnOwnedEmitterFromTheEnvironment(t *testing.T) {
	clearModeEnv(t)
	sink := newExportSink(t)
	t.Setenv(EnvMode, string(ModeTelemetry))
	t.Setenv(EnvBaseURL, sink.srv.URL)
	t.Setenv(EnvAPIKey, "rk")
	c, err := ForMode("gpt-4o", WithProviderKey("sk-explicit"))
	if err != nil {
		t.Fatal(err)
	}
	if c.telemetry == nil || c.telemetry.Err() != nil || !c.ownsTelemetry || c.apiKey != "sk-explicit" {
		t.Fatalf("telemetry %v owned %v key %q", c.telemetry, c.ownsTelemetry, c.apiKey)
	}
	if err := c.Close(); err != nil {
		t.Errorf("Close = %v", err)
	}
}

func TestForModeOptionOverridesTheEnvironment(t *testing.T) {
	clearModeEnv(t)
	t.Setenv(EnvMode, string(ModeTelemetry))
	sink := newExportSink(t)
	e := sink.emitter(t, false)
	c, err := ForMode("gpt-4o", WithMode(ModeDirect), WithEmitter(e))
	if err != nil {
		t.Fatal(err)
	}
	if c.telemetry != nil {
		t.Error("telemetry is on in direct mode")
	}
	c, err = ForMode("gpt-4o", WithMode(ModeTelemetry), WithEmitter(e))
	if err != nil {
		t.Fatal(err)
	}
	if c.telemetry != e || c.ownsTelemetry {
		t.Error("a given emitter must be used and left to its owner")
	}
}

func TestForModeRejectsAnUnknownMode(t *testing.T) {
	clearModeEnv(t)
	t.Setenv(EnvMode, "relay")
	if _, err := ForMode("gpt-4o"); err == nil {
		t.Error("want an error for an unknown WR_MODE")
	}
}
