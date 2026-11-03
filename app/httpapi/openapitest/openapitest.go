// Package openapitest checks a published OpenAPI document for what clients generated from it need: every operation identified, summarised, described and tagged, and every parameter and schema property described. It also finds where a property name appears, so tests can keep retired fields out.
//
// Test support only: nothing in the server imports it.
package openapitest

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/danielgtaylor/huma/v2"
)

var methods = []string{"get", "put", "post", "delete", "patch"}

// Decode renders doc as the JSON clients read, so checks see exactly what is published.
func Decode(doc *huma.OpenAPI) (map[string]any, error) {
	b, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	return out, json.Unmarshal(b, &out)
}

// Problems lists every gap in doc: an operation without operationId, summary, description or tags, a parameter without a description, and a schema property without one (huma's own $schema link excepted).
func Problems(doc map[string]any) []string {
	var out []string
	paths, _ := doc["paths"].(map[string]any)
	for path, item := range paths {
		ops, _ := item.(map[string]any)
		for _, m := range methods {
			op, ok := ops[m].(map[string]any)
			if !ok {
				continue
			}
			at := strings.ToUpper(m) + " " + path
			for _, field := range []string{"operationId", "summary", "description"} {
				if s, _ := op[field].(string); s == "" {
					out = append(out, at+": no "+field)
				}
			}
			if tags, _ := op["tags"].([]any); len(tags) == 0 {
				out = append(out, at+": no tags")
			}
			params, _ := op["parameters"].([]any)
			for _, p := range params {
				pm, _ := p.(map[string]any)
				if d, _ := pm["description"].(string); d == "" {
					out = append(out, fmt.Sprintf("%s: parameter %v has no description", at, pm["name"]))
				}
			}
		}
	}
	walk(doc, "", func(at string, props map[string]any) {
		for name, p := range props {
			pm, _ := p.(map[string]any)
			if d, _ := pm["description"].(string); d == "" && name != "$schema" {
				out = append(out, at+"."+name+": no description")
			}
		}
	})
	sort.Strings(out)
	return out
}

// PropertyPaths returns where a property named name is declared, as JSON paths of the schemas holding it.
func PropertyPaths(doc map[string]any, name string) []string {
	var out []string
	walk(doc, "", func(at string, props map[string]any) {
		if _, ok := props[name]; ok {
			out = append(out, at)
		}
	})
	sort.Strings(out)
	return out
}

// walk calls fn with the properties of every schema in v, wherever it is nested.
func walk(v any, at string, fn func(at string, props map[string]any)) {
	switch t := v.(type) {
	case map[string]any:
		if props, ok := t["properties"].(map[string]any); ok {
			fn(at, props)
		}
		for k, child := range t {
			walk(child, at+"."+k, fn)
		}
	case []any:
		for i, child := range t {
			walk(child, fmt.Sprintf("%s[%d]", at, i), fn)
		}
	}
}
