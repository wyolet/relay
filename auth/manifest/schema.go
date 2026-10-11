package manifest

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/auth/rbac"
)

// JSONSchemaDraft is the JSON Schema dialect JSONSchema writes.
const JSONSchemaDraft = "https://json-schema.org/draft/2020-12/schema"

// JSONSchema describes one kind's documents under this registry: the product's rule vocabulary as enums and its fields as spec properties, unknown fields refused as Parse refuses them.
func (r *Registry) JSONSchema(kind string) ([]byte, error) {
	spec, ok := r.specSchema(kind)
	if !ok {
		return nil, fmt.Errorf("manifest: unknown kind %q", kind)
	}
	versions := append([]string{APIVersion}, r.cfg.APIVersions...)
	doc := map[string]any{
		"$schema": JSONSchemaDraft,
		"title":   kind,
		"type":    "object",
		"properties": map[string]any{
			"apiVersion": map[string]any{"type": "string", "enum": versions},
			"kind":       map[string]any{"const": kind},
			"metadata":   metadataSchema(kind),
			"spec":       spec,
		},
		"required":             []string{"apiVersion", "kind", "metadata"},
		"additionalProperties": false,
	}
	return json.MarshalIndent(doc, "", "  ")
}

// JSONSchemas returns JSONSchema for every kind, keyed by kind.
func (r *Registry) JSONSchemas() (map[string][]byte, error) {
	out := make(map[string][]byte, len(Kinds()))
	for _, k := range Kinds() {
		b, err := r.JSONSchema(k)
		if err != nil {
			return nil, err
		}
		out[k] = b
	}
	return out, nil
}

func str() map[string]any     { return map[string]any{"type": "string"} }
func boolean() map[string]any { return map[string]any{"type": "boolean"} }

func array(items map[string]any) map[string]any {
	return map[string]any{"type": "array", "items": items}
}

func objectSchema(props map[string]any, required ...string) map[string]any {
	o := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		o["required"] = required
	}
	return o
}

func stringMap() map[string]any {
	return map[string]any{"type": "object", "additionalProperties": str()}
}

func metadataSchema(kind string) map[string]any {
	if kind == KindUser {
		return objectSchema(map[string]any{"id": str(), "name": described(str(), "The username.")}, "name")
	}
	name := str()
	name["pattern"] = `^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`
	owner := objectSchema(map[string]any{"kind": str(), "name": str(), "id": str()})
	return objectSchema(map[string]any{
		"id": str(), "name": name, "displayName": str(), "description": str(),
		"owner":  described(owner, "Ignored: the owner is derived from the kind and its spec."),
		"labels": stringMap(), "annotations": stringMap(),
	}, "name")
}

func described(s map[string]any, text string) map[string]any {
	s["description"] = text
	return s
}

// terms is a rule-term schema: the vocabulary and the wildcard, or any string when the product registered none.
func terms(vocabulary []string) map[string]any {
	item := str()
	if vocabulary != nil {
		set := append([]string{rbac.Wildcard}, vocabulary...)
		slices.Sort(set)
		item["enum"] = slices.Compact(set)
	}
	a := array(item)
	a["minItems"] = 1
	return a
}

func (r *Registry) specSchema(kind string) (map[string]any, bool) {
	var props map[string]any
	var required []string
	switch kind {
	case KindTeam:
		props = map[string]any{"enabled": boolean()}
	case KindProject:
		props, required = map[string]any{"team": described(str(), "The team's name."), "enabled": boolean()}, []string{"team"}
	case KindServiceAccount:
		props, required = map[string]any{"project": described(str(), "The project's name."), "enabled": boolean()}, []string{"project"}
	case KindRole:
		rules := array(objectSchema(map[string]any{"kinds": terms(r.cfg.RuleKinds), "verbs": terms(r.cfg.RuleVerbs)}, "kinds", "verbs"))
		rules["minItems"] = 1
		props, required = map[string]any{"rules": rules, "enabled": boolean()}, []string{"rules"}
	case KindGroup:
		props = map[string]any{"members": described(array(str()), "Usernames."), "enabled": boolean()}
	case KindRoleBinding:
		subject := objectSchema(map[string]any{
			"kind": map[string]any{"enum": []string{string(directory.SubjectUser), string(directory.SubjectGroup), string(directory.SubjectServiceAccount)}},
			"name": str(),
		}, "kind", "name")
		subjects := array(subject)
		subjects["minItems"] = 1
		props = map[string]any{
			"role":     described(str(), "The role's name."),
			"scope":    described(objectSchema(map[string]any{"kind": str(), "name": str(), "id": str()}, "kind"), "system (no name), team or project by name, or a product scope."),
			"subjects": subjects, "enabled": boolean(),
		}
		required = []string{"role", "scope", "subjects"}
	case KindUser:
		props = userSchema()
	default:
		return nil, false
	}
	props = maps.Clone(props)
	for _, f := range r.fields(kind) {
		s := f.Schema
		if s == nil {
			s = map[string]any{}
		}
		props[f.Name] = s
	}
	return objectSchema(props, required...), true
}

func userSchema() map[string]any {
	hash := described(str(), "A bcrypt hash.")
	hash["pattern"] = `^\$2[aby]\$`
	env := objectSchema(map[string]any{"env": str()}, "env")
	password := objectSchema(map[string]any{"hash": hash, "valueFrom": described(env, "An environment variable holding a password or a bcrypt hash.")})
	password["oneOf"] = []any{map[string]any{"required": []string{"hash"}}, map[string]any{"required": []string{"valueFrom"}}}
	return map[string]any{
		"email":    str(),
		"password": described(password, "A hash or an environment variable; plain text is refused."),
		"identity": objectSchema(map[string]any{"issuer": str(), "subject": str()}, "issuer", "subject"),
		"roles":    array(str()),
		"disabled": boolean(),
	}
}
