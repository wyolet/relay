package httpapi

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/hostkey"
)

// TestHostKeySpec_EmitsValueField guards against the cleartext-write
// field disappearing from the OpenAPI surface. UI flows that POST a
// stored-mode key depend on this property being present.
func TestHostKeySpec_EmitsValueField(t *testing.T) {
	reg := NewRegistry()
	reg.Schema(reflect.TypeOf(hostkey.Spec{}), false, "")
	raw, err := json.Marshal(reg.Map()["HostKeySpec"])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"value"`) {
		t.Fatalf("HostKeySpec missing \"value\" field:\n%s", raw)
	}
}

var _ = huma.NewMapRegistry // anchor import