package postgres

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/wyolet/relay/auth/authz"
	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/auth/rbac"
	"github.com/wyolet/relay/auth/store/postgres/internal/gen"
)

// The literals are the documents a product sharing these tables writes today; the module must write the same bytes and read them back unchanged.

func TestMetadataGolden(t *testing.T) {
	tests := []struct {
		name string
		meta directory.Metadata
		want string
	}{
		{"empty", directory.Metadata{}, `{"owner":{}}`},
		{"full", directory.Metadata{Description: "the core team", Owner: authz.Owner{Kind: "system"},
			Labels: map[string]string{"tier": "gold"}, Annotations: map[string]string{"note": "seeded"}, Dirty: true},
			`{"description":"the core team","owner":{"kind":"system"},"labels":{"tier":"gold"},"annotations":{"note":"seeded"},"dirty":true}`},
		{"owner with id, empty labels", directory.Metadata{Owner: authz.Owner{Kind: "team", ID: "t-1"}, Labels: map[string]string{}},
			`{"owner":{"kind":"team","id":"t-1"}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := encodeMetadata(tt.meta)
			if err != nil || string(got) != tt.want {
				t.Fatalf("encode = %s, %v; want %s", got, err, tt.want)
			}
			back, err := decodeMetadata("id", "name", "Name", got, ts(), ts(), 3)
			if err != nil {
				t.Fatal(err)
			}
			again, _ := encodeMetadata(back)
			if string(again) != tt.want {
				t.Fatalf("round trip = %s, want %s", again, tt.want)
			}
			if back.ID != "id" || back.Name != "name" || back.DisplayName != "Name" || back.ResourceVersion != "3" {
				t.Fatalf("columns not carried: %+v", back)
			}
		})
	}
}

func TestSpecGolden(t *testing.T) {
	no := false
	budget := json.RawMessage(`{"amount":"10.00","period":"month"}`)
	tests := []struct {
		name  string
		write func() ([]byte, error)
		want  string
		// product is the product's document when its key order differs from the module's; the two must hold the same JSON.
		product string
		read    func(raw []byte) (any, error)
		wantRow any
	}{
		{name: "team", want: `{}`,
			write:   func() ([]byte, error) { return encodeSpec(enabledDoc{}, nil, teamSpecKeys) },
			read:    func(raw []byte) (any, error) { return readTeamSpec(raw) },
			wantRow: directory.TeamSpec{}},
		{name: "team with product field", want: `{"enabled":false,"budget":{"amount":"10.00","period":"month"}}`,
			write: func() ([]byte, error) {
				return encodeSpec(enabledDoc{Enabled: &no}, map[string]json.RawMessage{"budget": budget}, teamSpecKeys)
			},
			read:    func(raw []byte) (any, error) { return readTeamSpec(raw) },
			wantRow: directory.TeamSpec{Enabled: &no, Attributes: map[string]json.RawMessage{"budget": budget}}},
		{name: "project", want: `{"teamId":"t-1"}`,
			write: func() ([]byte, error) { return encodeSpec(projectSpecDoc{TeamID: "t-1"}, nil, projectSpecKeys) }},
		{name: "project with product field", want: `{"teamId":"t-1","enabled":false,"budget":{"amount":"5","period":"week","onExceed":"warn"}}`,
			write: func() ([]byte, error) {
				return encodeSpec(projectSpecDoc{TeamID: "t-1", Enabled: &no},
					map[string]json.RawMessage{"budget": json.RawMessage(`{"amount": "5", "period": "week", "onExceed": "warn"}`)}, projectSpecKeys)
			}},
		{name: "service account", want: `{"projectId":"p-1"}`,
			write: func() ([]byte, error) {
				return encodeSpec(serviceAccountSpecDoc{ProjectID: "p-1"}, nil, serviceAccountSpecKeys)
			}},
		{name: "service account with product field", want: `{"projectId":"p-1","enabled":false,"policyId":"pol-1"}`,
			product: `{"projectId":"p-1","policyId":"pol-1","enabled":false}`,
			write: func() ([]byte, error) {
				return encodeSpec(serviceAccountSpecDoc{ProjectID: "p-1", Enabled: &no},
					map[string]json.RawMessage{"policyId": json.RawMessage(`"pol-1"`)}, serviceAccountSpecKeys)
			}},
		{name: "group", want: `{}`,
			write: func() ([]byte, error) { return encodeSpec(enabledDoc{}, nil, groupSpecKeys) }},
		{name: "group disabled", want: `{"enabled":false}`,
			write: func() ([]byte, error) { return encodeSpec(enabledDoc{Enabled: &no}, nil, groupSpecKeys) }},
		{name: "role", want: `{"rules":[{"kinds":["documents"],"verbs":["get","list"]}]}`,
			write: func() ([]byte, error) {
				return encodeSpec(roleSpecDoc{Rules: []ruleDoc{{Kinds: []string{"documents"}, Verbs: []string{"get", "list"}}}}, nil, roleSpecKeys)
			}},
		{name: "role without rules", want: `{"rules":null}`,
			write: func() ([]byte, error) { return encodeSpec(roleSpecDoc{}, nil, roleSpecKeys) }},
		{name: "role binding", want: `{"roleId":"","scope":{},"subjects":null}`,
			write: func() ([]byte, error) { return encodeSpec(bindingSpecDoc{}, nil, bindingSpecKeys) }},
		{name: "role binding disabled", want: `{"roleId":"","scope":{},"subjects":null,"enabled":false}`,
			write: func() ([]byte, error) { return encodeSpec(bindingSpecDoc{Enabled: &no}, nil, bindingSpecKeys) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.write()
			if err != nil || string(got) != tt.want {
				t.Fatalf("encode = %s, %v; want %s", got, err, tt.want)
			}
			if tt.product != "" && !sameJSON(t, got, []byte(tt.product)) {
				t.Fatalf("encode = %s does not hold the same JSON as %s", got, tt.product)
			}
			if tt.read != nil {
				row, err := tt.read(got)
				if err != nil || !reflect.DeepEqual(row, tt.wantRow) {
					t.Fatalf("decode = %+v, %v; want %+v", row, err, tt.wantRow)
				}
			}
		})
	}
}

func readTeamSpec(raw []byte) (directory.TeamSpec, error) {
	var doc enabledDoc
	attrs, err := decodeSpec(raw, &doc, teamSpecKeys)
	return directory.TeamSpec{Enabled: doc.Enabled, Attributes: attrs}, err
}

// TestSpecRoundTripFromProduct reads documents as Postgres returns them (spaced, keys reordered) and checks the module writes the same JSON back.
func TestSpecRoundTripFromProduct(t *testing.T) {
	stored := []byte(`{"enabled": false, "policyId": "pol-1", "projectId": "p-1"}`)
	var doc serviceAccountSpecDoc
	attrs, err := decodeSpec(stored, &doc, serviceAccountSpecKeys)
	if err != nil {
		t.Fatal(err)
	}
	if doc.ProjectID != "p-1" || doc.Enabled == nil || *doc.Enabled || string(attrs["policyId"]) != `"pol-1"` || len(attrs) != 1 {
		t.Fatalf("decode = %+v %s", doc, attrs)
	}
	back, err := encodeSpec(doc, attrs, serviceAccountSpecKeys)
	if err != nil || !sameJSON(t, back, stored) {
		t.Fatalf("round trip = %s, %v; want the JSON of %s", back, err, stored)
	}

	legacy := []byte(`{"roleId": "r-1", "scope": {"kind": "team", "id": "t-1"}, "subjects": [{"kind": "user", "id": "u-1"}]}`)
	var bdoc bindingSpecDoc
	if attrs, err := decodeSpec(legacy, &bdoc, bindingSpecKeys); err != nil || attrs != nil {
		t.Fatalf("binding placeholders read as attributes: %v, %v", attrs, err)
	}
	var gdoc enabledDoc
	if attrs, err := decodeSpec([]byte(`{"memberIds": ["u-1"]}`), &gdoc, groupSpecKeys); err != nil || attrs != nil {
		t.Fatalf("group members read as attributes: %v, %v", attrs, err)
	}
}

func TestSpecRefusals(t *testing.T) {
	if _, err := encodeSpec(enabledDoc{}, map[string]json.RawMessage{"enabled": json.RawMessage(`true`)}, teamSpecKeys); !errors.Is(err, directory.ErrInvalid) {
		t.Fatalf("attribute shadowing a field: err = %v", err)
	}
	if _, err := encodeSpec(enabledDoc{}, map[string]json.RawMessage{"x": json.RawMessage(`{`)}, teamSpecKeys); !errors.Is(err, directory.ErrInvalid) {
		t.Fatalf("attribute that is not JSON: err = %v", err)
	}
	if _, err := decodeSpec([]byte(`[`), &enabledDoc{}, teamSpecKeys); err == nil {
		t.Fatal("decode of broken JSON succeeded")
	}
	if _, err := decodeMetadata("", "", "", []byte(`[`), ts(), ts(), 1); err == nil {
		t.Fatal("decode of broken metadata succeeded")
	}
}

func TestIdentityColumn(t *testing.T) {
	tests := []struct {
		id     directory.Identity
		column string
	}{
		{directory.Identity{}, ""},
		{directory.Identity{Issuer: "https://idp.example.com", Subject: "248289761001"}, "https://idp.example.com|248289761001"},
		{directory.Identity{Issuer: "https://idp.example.com", Subject: "a|b"}, "https://idp.example.com|a|b"},
	}
	for _, tt := range tests {
		if got := encodeIdentity(tt.id); got != tt.column {
			t.Fatalf("encode %+v = %q, want %q", tt.id, got, tt.column)
		}
		if got := decodeIdentity(tt.column); got != tt.id {
			t.Fatalf("decode %q = %+v, want %+v", tt.column, got, tt.id)
		}
	}
	if got := decodeIdentity("bare"); got != (directory.Identity{Subject: "bare"}) {
		t.Fatalf("decode without separator = %+v", got)
	}
}

func TestBindingScopeColumns(t *testing.T) {
	b, err := bindingFromRow(bindingRow(rbac.Scope{Kind: "system"}))
	if err != nil {
		t.Fatal(err)
	}
	if b.Spec.Scope != directory.Global || b.Meta.Owner != (authz.Owner{Kind: "system"}) {
		t.Fatalf("global scope read as %+v, owner %+v", b.Spec.Scope, b.Meta.Owner)
	}
}

func ts() pgtype.Timestamptz { return pgtype.Timestamptz{Time: time.Unix(1, 0), Valid: true} }

func bindingRow(scope rbac.Scope) gen.RoleBinding {
	return gen.RoleBinding{ID: "b", Name: "b", RoleID: "r", ScopeKind: scope.Kind, ScopeID: text(scope.ID),
		Metadata: []byte(`{"owner":{"kind":"system"}}`), Spec: []byte(`{"roleId":"","scope":{},"subjects":null}`),
		CreatedAt: ts(), UpdatedAt: ts(), ResourceVersion: 1}
}

func sameJSON(t *testing.T, a, b []byte) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatal(err)
	}
	return reflect.DeepEqual(x, y)
}
