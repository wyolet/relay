// The tenancy, RBAC and account kinds are the auth module's manifests. Relay registers its rule vocabulary and its own spec fields with the module, accepts its relay.wyolet.dev apiVersion for them, and keeps its wire DTOs and translate functions as thin adapters over the module's.

package manifest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/team"
	authmanifest "github.com/wyolet/relay/auth/manifest"
)

// AuthManifests is relay's registration with the auth module's manifests.
var AuthManifests = authmanifest.MustRegistry(authmanifest.Config{
	RuleKinds:   role.Kinds,
	RuleVerbs:   role.Verbs,
	APIVersions: []string{APIVersion},
	Fields: map[string][]authmanifest.Field{
		authmanifest.KindTeam:           {budgetField},
		authmanifest.KindProject:        {budgetField},
		authmanifest.KindServiceAccount: {policyField},
	},
})

// budgetField is the spend cap on a Team or a Project, stored as team.Budget with its default period filled.
var budgetField = authmanifest.Field{
	Name: "budget",
	Schema: map[string]any{
		"type": "object", "additionalProperties": false, "required": []string{"amount"},
		"properties": map[string]any{
			"amount":   map[string]any{"type": "string"},
			"period":   map[string]any{"enum": []string{"month", "week", "day"}},
			"onExceed": map[string]any{"enum": []string{"block", "warn"}},
		},
	},
	ToAttribute: func(value json.RawMessage, _ authmanifest.Resolver) (json.RawMessage, error) {
		var b *BudgetDTO
		if err := strictJSON(value, &b); err != nil {
			return nil, err
		}
		if b == nil {
			return nil, nil
		}
		return json.Marshal(toBudget(b))
	},
	// A value that is no budget renders as stored, so a From call never fails on it.
	FromAttribute: func(stored json.RawMessage, _ authmanifest.Namer) (json.RawMessage, error) {
		var b *team.Budget
		if json.Unmarshal(stored, &b) != nil {
			return stored, nil
		}
		if b == nil {
			return nil, nil
		}
		return json.Marshal(fromBudget(b))
	},
}

// policyField is a service account's policy override: a policy name in YAML, its id in the row.
var policyField = authmanifest.Field{
	Name: "policy", Attribute: "policyId",
	Schema: map[string]any{"type": "string", "description": "The policy's name."},
	ToAttribute: func(value json.RawMessage, refs authmanifest.Resolver) (json.RawMessage, error) {
		var name string
		if err := json.Unmarshal(value, &name); err != nil {
			return nil, err
		}
		if name == "" {
			return nil, nil
		}
		id, ok := refs.ID("Policy", name)
		if !ok {
			return nil, &authmanifest.RefError{Kind: "policy", Name: name}
		}
		return json.Marshal(id)
	},
	FromAttribute: func(stored json.RawMessage, names authmanifest.Namer) (json.RawMessage, error) {
		var id string
		if json.Unmarshal(stored, &id) != nil {
			return stored, nil
		}
		if id == "" {
			return nil, nil
		}
		if n, ok := names.Name("Policy", id); ok {
			return json.Marshal(n)
		}
		return json.Marshal(id)
	},
}

// authKind reports a document the auth module decodes: one of its kinds under its apiVersion or relay's. A User under any other apiVersion stays the identity loader's (see isForeignKind).
func authKind(apiVersion, kind string) bool {
	if !authmanifest.IsKind(kind) {
		return false
	}
	if kind == authmanifest.KindUser {
		return apiVersion == authmanifest.APIVersion
	}
	return AuthManifests.AcceptsAPIVersion(apiVersion)
}

func parseAuthDoc(index int, node *yaml.Node) (Document, error) {
	d, err := AuthManifests.Decode(index, node)
	if err != nil {
		return Document{}, err
	}
	doc, err := fromAuthDoc(&d)
	if err != nil {
		return Document{}, &authmanifest.Error{Doc: index, Kind: d.Kind, Name: d.Metadata.Name, Err: err}
	}
	return doc, nil
}

func strictJSON(raw json.RawMessage, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

// authRefs presents relay's Resolver to the module, by document kind.
type authRefs struct{ r Resolver }

func (a authRefs) ID(kind, name string) (string, bool) {
	switch kind {
	case authmanifest.KindTeam:
		return a.r.TeamID(name)
	case authmanifest.KindProject:
		return a.r.ProjectID(name)
	case authmanifest.KindServiceAccount:
		return a.r.ServiceAccountID(name)
	case authmanifest.KindRole:
		return a.r.RoleID(name)
	case authmanifest.KindGroup:
		return a.r.GroupID(name)
	case authmanifest.KindUser:
		return a.r.UserID(name)
	case "Policy":
		return a.r.PolicyID(name)
	}
	return "", false
}

// authNames presents relay's ReverseResolver to the module.
type authNames struct{ r ReverseResolver }

func (a authNames) Name(kind, id string) (string, bool) {
	switch kind {
	case authmanifest.KindTeam:
		return a.r.TeamName(id)
	case authmanifest.KindProject:
		return a.r.ProjectName(id)
	case authmanifest.KindServiceAccount:
		return a.r.ServiceAccountName(id)
	case authmanifest.KindRole:
		return a.r.RoleName(id)
	case authmanifest.KindGroup:
		return a.r.GroupName(id)
	case authmanifest.KindUser:
		return a.r.Username(id)
	case "Policy":
		return a.r.PolicyName(id)
	}
	return "", false
}

// fromAuthError words a module translate error the way relay always has ("project \"x\": team \"y\" not found"), a name that resolves to nothing staying a RefNotFoundError.
func fromAuthError(err error) error {
	var de *authmanifest.Error
	if !errors.As(err, &de) {
		return err
	}
	prefix := strings.ToLower(de.Kind)
	if strings.HasPrefix(de.Field, "spec.subjects") {
		prefix = "binding"
	}
	var ref *authmanifest.RefError
	if errors.As(err, &ref) {
		return refNotFound("%s %q: %s %q not found", prefix, de.Name, ref.Kind, ref.Name)
	}
	return fmt.Errorf("%s %q: %w", prefix, de.Name, de.Err)
}
