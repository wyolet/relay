package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/danielgtaylor/huma/v2"
)

// A 422 is only actionable if it names the field that failed, so the
// per-field errors huma passes to NewError must survive the rewrite into
// the OpenAI envelope.
func TestOpenAIErrorCarriesFieldErrors(t *testing.T) {
	Install()
	err := huma.NewError(http.StatusUnprocessableEntity, "validation failed",
		&huma.ErrorDetail{Message: "expected string", Location: "body.project", Value: 5})

	raw, mErr := json.Marshal(err)
	if mErr != nil {
		t.Fatalf("marshal: %v", mErr)
	}
	var got struct {
		Error struct {
			Message string              `json:"message"`
			Details []*huma.ErrorDetail `json:"details"`
		} `json:"error"`
	}
	if uErr := json.Unmarshal(raw, &got); uErr != nil {
		t.Fatalf("unmarshal %s: %v", raw, uErr)
	}
	if len(got.Error.Details) != 1 {
		t.Fatalf("details = %v, want the one field error", got.Error.Details)
	}
	if got.Error.Details[0].Location != "body.project" {
		t.Errorf("detail = %+v, want the field location", got.Error.Details[0])
	}
}
