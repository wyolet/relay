package group

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/wyolet/relay/auth/directory"
)

// A managed group's source reaches the API read; a local group's JSON is as before, and no write can set a source.
func TestExternalIsReadOnly(t *testing.T) {
	row := fix("engineering").ToDirectory()
	row.Spec.External = directory.External{Source: "okta", ID: "00g1"}
	g := FromDirectory(row)
	raw, err := json.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"external":{"source":"okta","id":"00g1"}`) {
		t.Fatalf("managed group JSON = %s", raw)
	}
	if !g.ToDirectory().Spec.External.IsZero() {
		t.Fatal("ToDirectory carried the source, so a write could hand a group to one")
	}
	local, err := json.Marshal(fix("ops"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(local), "external") {
		t.Fatalf("a local group's JSON gained a field: %s", local)
	}
}
