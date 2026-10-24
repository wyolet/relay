package anthropic

import (
	"encoding/json"
	"testing"
)

func TestOverloadedError(t *testing.T) {
	var env struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(OverloadedError("busy"), &env); err != nil {
		t.Fatal(err)
	}
	if env.Type != "error" || env.Error.Type != "overloaded_error" || env.Error.Message != "busy" {
		t.Fatalf("envelope = %+v", env)
	}
}
