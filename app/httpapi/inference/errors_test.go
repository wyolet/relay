package inference

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/app/routing"
)

func TestMapRoutingErr_ModelNotInPolicy_NamesModel(t *testing.T) {
	rec := httptest.NewRecorder()
	mapRoutingErr(rec, routing.ErrModelNotInPolicy, "gpt-4o", "pol_123")

	if rec.Code != 403 {
		t.Fatalf("status: %d", rec.Code)
	}
	var env httpapi.OpenAIError
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Err.Code != "model_not_allowed" {
		t.Errorf("code: %q", env.Err.Code)
	}
	if !strings.Contains(env.Err.Message, `"gpt-4o"`) {
		t.Errorf("message should name the model: %q", env.Err.Message)
	}
	// the policy id is log-only — it must never reach the client body.
	if strings.Contains(rec.Body.String(), "pol_123") {
		t.Errorf("policy id leaked into client body: %s", rec.Body.String())
	}
}

func TestMapRoutingErr_EmptyModel_FallsBackToGeneric(t *testing.T) {
	rec := httptest.NewRecorder()
	mapRoutingErr(rec, routing.ErrModelNotInPolicy, "", "")

	var env httpapi.OpenAIError
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Err.Message != "model is not allowed by this policy" {
		t.Errorf("generic fallback expected, got: %q", env.Err.Message)
	}
}
