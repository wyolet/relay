package scim

import (
	"net/http"
	"strings"
)

// serviceProviderConfig is RFC 7643 section 5: PATCH and filters, no bulk, sorting, ETags or password changes.
func serviceProviderConfig() map[string]any {
	return map[string]any{
		"schemas":          []string{schemaSPConfig},
		"documentationUri": "https://datatracker.ietf.org/doc/html/rfc7644",
		"patch":            map[string]any{"supported": true},
		"bulk":             map[string]any{"supported": false, "maxOperations": 0, "maxPayloadSize": 0},
		"filter":           map[string]any{"supported": true, "maxResults": maxCount},
		"changePassword":   map[string]any{"supported": false},
		"sort":             map[string]any{"supported": false},
		"etag":             map[string]any{"supported": false},
		"authenticationSchemes": []map[string]any{{
			"type": "oauthbearertoken", "name": "Bearer token", "primary": true,
			"description": "The token issued for this SCIM client, sent as Authorization: Bearer <token>.",
		}},
		"meta": map[string]any{"resourceType": "ServiceProviderConfig"},
	}
}

func resourceTypes(h *Handler) []map[string]any {
	rt := func(name, endpoint, schema, desc string) map[string]any {
		m := map[string]any{
			"schemas": []string{schemaResourceType}, "id": name, "name": name,
			"endpoint": endpoint, "schema": schema, "description": desc,
			"meta": map[string]any{"resourceType": "ResourceType"},
		}
		if h.cfg.BaseURL != "" {
			m["meta"].(map[string]any)["location"] = h.location("ResourceTypes", name)
		}
		return m
	}
	return []map[string]any{
		rt("User", "/Users", schemaUser, "An account."),
		rt("Group", "/Groups", schemaGroup, "A set of accounts that role bindings can name."),
	}
}

type attrSpec struct {
	Name          string     `json:"name"`
	Type          string     `json:"type"`
	MultiValued   bool       `json:"multiValued"`
	Description   string     `json:"description"`
	Required      bool       `json:"required"`
	CaseExact     bool       `json:"caseExact"`
	Mutability    string     `json:"mutability"`
	Returned      string     `json:"returned"`
	Uniqueness    string     `json:"uniqueness"`
	SubAttributes []attrSpec `json:"subAttributes,omitempty"`
}

func simple(name, typ, desc string, required, caseExact bool, mutability, uniqueness string) attrSpec {
	return attrSpec{Name: name, Type: typ, Description: desc, Required: required, CaseExact: caseExact,
		Mutability: mutability, Returned: "default", Uniqueness: uniqueness}
}

// schemaDefinitions is RFC 7643 section 7 for the attributes the handler keeps. Every other core attribute is accepted on write and not stored.
func schemaDefinitions() []map[string]any {
	ref := simple("$ref", "reference", "The URI of the referenced resource.", false, true, "readOnly", "none")
	user := []attrSpec{
		simple("userName", "string", "Unique login name.", true, false, "readWrite", "server"),
		simple("externalId", "string", "The client's own id for the account.", false, true, "readWrite", "none"),
		simple("active", "boolean", "false disables the account, ending its sessions and tokens.", false, false, "readWrite", "none"),
		{Name: "emails", Type: "complex", MultiValued: true, Mutability: "readWrite", Returned: "default", Uniqueness: "none",
			Description: "One address is kept: the primary one, else the first.",
			SubAttributes: []attrSpec{
				simple("value", "string", "The address.", false, false, "readWrite", "none"),
				simple("type", "string", "Always work.", false, false, "readWrite", "none"),
				simple("primary", "boolean", "Always true.", false, false, "readWrite", "none"),
			}},
		{Name: "groups", Type: "complex", MultiValued: true, Mutability: "readOnly", Returned: "default", Uniqueness: "none",
			Description:   "The client's groups the account is a member of.",
			SubAttributes: []attrSpec{simple("value", "string", "The group's id.", false, true, "readOnly", "none"), ref}},
	}
	group := []attrSpec{
		simple("displayName", "string", "The group's name as the client shows it.", true, false, "readWrite", "none"),
		simple("externalId", "string", "The client's own id for the group.", false, true, "readWrite", "none"),
		{Name: "members", Type: "complex", MultiValued: true, Mutability: "readWrite", Returned: "default", Uniqueness: "none",
			Description:   "The group's accounts; each must be one the client provisioned.",
			SubAttributes: []attrSpec{simple("value", "string", "The account's id.", false, true, "immutable", "none"), ref}},
	}
	return []map[string]any{
		{"schemas": []string{schemaSchema}, "id": schemaUser, "name": "User", "description": "User account", "attributes": user,
			"meta": map[string]any{"resourceType": "Schema"}},
		{"schemas": []string{schemaSchema}, "id": schemaGroup, "name": "Group", "description": "Group", "attributes": group,
			"meta": map[string]any{"resourceType": "Schema"}},
	}
}

func (h *Handler) serveDiscovery(rq *request, v any) error {
	if rq.r.Method != http.MethodGet {
		return methodNotAllowed()
	}
	return rq.reply(http.StatusOK, v)
}

// serveCatalog answers a list of discovery resources, or the one whose id is rest[0].
func (h *Handler) serveCatalog(rq *request, rest []string, items []map[string]any) error {
	if rq.r.Method != http.MethodGet {
		return methodNotAllowed()
	}
	if len(rest) == 0 {
		all := make([]any, len(items))
		for i, it := range items {
			all[i] = it
		}
		return rq.reply(http.StatusOK, page(all, listQuery{startIndex: 1, count: len(all)}))
	}
	for _, it := range items {
		if id, _ := it["id"].(string); len(rest) == 1 && strings.EqualFold(id, rest[0]) {
			return rq.reply(http.StatusOK, it)
		}
	}
	return errorf(http.StatusNotFound, "", "no such resource")
}
