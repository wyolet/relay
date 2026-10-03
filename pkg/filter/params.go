package filter

import "fmt"

// Param is a framework-agnostic description of one accepted query
// parameter, derived from the schema. The HTTP layer maps these to its
// router's parameter type (e.g. huma.Param) so the OpenAPI spec — and any
// client generated from it — sees exactly the params the engine accepts.
// Keeping this pure (no web-framework import) preserves the single-source-
// of-truth property: params are derived from the same Field list that
// drives matching.
type Param struct {
	Name        string
	Type        string // "string" | "integer" | "boolean"
	Repeatable  bool   // array-valued query param (explode)
	Enum        []string
	Description string
}

// Params enumerates every query parameter this schema accepts, expanding
// range fields into their _min/_max or _from/_to pair and appending the
// engine-owned params (q, label, sort, limit, offset) when applicable.
// Order is deterministic (field order, then engine params) so generated
// specs are stable.
func (s Schema[T]) Params() []Param {
	var out []Param
	for _, f := range s.Fields {
		switch f.Kind {
		case String:
			out = append(out, Param{Name: f.Name, Type: "string", Repeatable: f.Repeat, Enum: f.Enum,
				Description: stringParamDoc(f)})
		case Bool:
			out = append(out, Param{Name: f.Name, Type: "boolean", Description: "Match " + f.Name + " (true/false)."})
		case Int:
			out = append(out,
				Param{Name: f.Name + "_min", Type: "integer", Description: "Minimum " + f.Name + " (inclusive)."},
				Param{Name: f.Name + "_max", Type: "integer", Description: "Maximum " + f.Name + " (inclusive)."})
		case Time:
			out = append(out,
				Param{Name: f.Name + "_from", Type: "string", Description: f.Name + " lower bound (RFC3339)."},
				Param{Name: f.Name + "_to", Type: "string", Description: f.Name + " upper bound (RFC3339)."})
		}
	}
	if s.Q != nil {
		out = append(out, Param{Name: "q", Type: "string", Description: "Free-text search."})
	}
	if s.Labels != nil {
		out = append(out, Param{Name: "label", Type: "string", Repeatable: true,
			Description: "Label selector key=value (repeatable, all must match)."})
	}
	if sortable := s.sortableNames(); len(sortable) > 0 {
		// Enumerate the "-" descending variants too: generated clients
		// (openapi-typescript) type the param from this enum, and a
		// missing "-name" makes valid requests untypeable.
		enum := make([]string, 0, len(sortable)*2)
		for _, n := range sortable {
			enum = append(enum, n, "-"+n)
		}
		out = append(out, Param{Name: "sort", Type: "string", Enum: enum,
			Description: "Sort field; prefix with '-' for descending."})
	}
	limitDoc := "Max items to return (page size)."
	if s.DefaultLimit > 0 {
		limitDoc = fmt.Sprintf("Max items to return (page size). Defaults to %d when absent; pass 0 for the full set.", s.DefaultLimit)
	}
	out = append(out,
		Param{Name: "limit", Type: "integer", Description: limitDoc},
		Param{Name: "offset", Type: "integer", Description: "Items to skip before the page."})
	return out
}

func (s Schema[T]) sortableNames() []string {
	var names []string
	for _, f := range s.Fields {
		if f.Sortable {
			names = append(names, f.Name)
		}
	}
	return names
}

func stringParamDoc[T any](f Field[T]) string {
	switch {
	case f.MatchAll:
		return "Match " + f.Name + " (repeatable; item must have ALL given values)."
	case f.Repeat:
		return "Match any of the given " + f.Name + " values."
	default:
		return "Match " + f.Name + " exactly."
	}
}
