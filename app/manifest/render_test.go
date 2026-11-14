package manifest

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/overlay"
)

func TestRenderSchemaHeaderUsesPublicURL(t *testing.T) {
	docs := []Document{
		{Team: &TeamDTO{APIVersion: APIVersion, Kind: "Team", Metadata: WireMeta{Name: "platform"}}},
		{Project: &ProjectDTO{APIVersion: APIVersion, Kind: "Project", Metadata: WireMeta{Name: "ml"}, Spec: ProjectSpec{Team: "platform"}}},
	}

	out, err := Render(docs, "https://relay.example.com")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	body := string(out)
	for _, want := range []string{
		"# yaml-language-server: $schema=https://relay.example.com/api/schemas/v1alpha3/Team.schema.json",
		"# yaml-language-server: $schema=https://relay.example.com/api/schemas/v1alpha3/Project.schema.json",
		"\n---\n",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("rendered bundle missing %q:\n%s", want, body)
		}
	}
	if !strings.HasPrefix(body, "# yaml-language-server:") {
		t.Fatalf("first document is not preceded by its schema directive:\n%s", body)
	}

	relative, err := Render(docs[:1], "")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.HasPrefix(string(relative), "# yaml-language-server: $schema=/api/schemas/v1alpha3/Team.schema.json") {
		t.Fatalf("empty base must render a relative reference:\n%s", relative)
	}
}

// A rendered bundle must parse back into the same documents — that identity
// is what makes an export re-appliable.
func TestRenderRoundTripsThroughParse(t *testing.T) {
	docs := []Document{
		{Team: &TeamDTO{APIVersion: APIVersion, Kind: "Team", Metadata: WireMeta{
			ID: "019200aa-0000-7000-8000-000000000001", Name: "platform", DisplayName: "Platform",
			Labels: map[string]string{"env": "prod"},
		}}},
		{Key: &KeyDTO{APIVersion: APIVersion, Kind: "Key", Metadata: WireMeta{Name: "ci"},
			Spec: KeySpec{
				Principal: PrincipalDTO{Kind: "serviceaccount", Name: "ci-deployer"},
				KeyHash:   strings.Repeat("a", 64), Prefix: "rk_abc123",
			}}},
	}
	out, err := Render(docs, "")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	back, err := Parse(strings.NewReader(string(out)))
	if err != nil {
		t.Fatalf("Parse rendered bundle: %v\n%s", err, out)
	}
	if len(back) != 2 {
		t.Fatalf("parsed %d documents, want 2", len(back))
	}
	if back[0].Team == nil || back[0].Team.Metadata.ID != docs[0].Team.Metadata.ID {
		t.Fatalf("team id lost in round trip: %+v", back[0].Team)
	}
	if back[1].Key == nil || back[1].Key.Spec.KeyHash != docs[1].Key.Spec.KeyHash {
		t.Fatalf("key hash lost in round trip: %+v", back[1].Key)
	}
}

func TestOverlayRoundTrip(t *testing.T) {
	o := &overlay.Overlay{
		Kind:       overlay.KindModel,
		ResourceID: "m-1",
		Patch:      json.RawMessage(`{"family":"custom","aliases":["team-fast"]}`),
	}
	rev := MapReverseResolver{Models: map[string]string{"m-1": "gpt-4o"}}
	dto, err := FromOverlay(o, rev)
	if err != nil {
		t.Fatalf("FromOverlay: %v", err)
	}
	if dto.Metadata.Name != "gpt-4o" || dto.Spec.Target != overlay.KindModel {
		t.Fatalf("unexpected DTO: %+v", dto)
	}

	idx := MapResolver{Models: map[string]string{"gpt-4o": "m-1"}}
	back, err := ToOverlay(dto, idx)
	if err != nil {
		t.Fatalf("ToOverlay: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(back.Patch, &got); err != nil {
		t.Fatalf("patch %s: %v", back.Patch, err)
	}
	want := map[string]any{"family": "custom", "aliases": []any{"team-fast"}}
	if diff := cmp.Diff(want, got); diff != "" || back.ResourceID != "m-1" || back.Kind != overlay.KindModel {
		t.Errorf("overlay %s %q, patch (-want +got):\n%s", back.Kind, back.ResourceID, diff)
	}

	if _, err := ToOverlay(dto, MapResolver{}); err == nil || err.Error() != `overlay: model "gpt-4o" not found` {
		t.Errorf("an overlay naming an unknown model: err = %v", err)
	}
}
