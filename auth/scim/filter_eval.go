package scim

import (
	"strings"
)

// attrRef is a resolved attribute: a top-level name and an optional sub-attribute, both lowercase.
type attrRef struct {
	name, sub string
	def       *attrDef
}

// values returns every value the attribute holds in doc: one for a simple attribute, one per element for a sub-attribute of a multi-valued one.
func (a attrRef) values(doc map[string]any) []any {
	v, ok := doc[a.name]
	if !ok {
		return nil
	}
	if a.sub == "" {
		return []any{v}
	}
	var out []any
	if els, ok := v.([]map[string]any); ok {
		for _, el := range els {
			if x, ok := el[a.sub]; ok {
				out = append(out, x)
			}
		}
	}
	return out
}

type presentExpr struct{ attr attrRef }

func (e presentExpr) eval(doc map[string]any) bool {
	for _, v := range e.attr.values(doc) {
		if s, ok := v.(string); !ok || s != "" {
			return true
		}
	}
	return false
}

var compareOps = map[string]bool{
	"eq": true, "ne": true, "co": true, "sw": true, "ew": true, "gt": true, "ge": true, "lt": true, "le": true,
}

type compareExpr struct {
	attr  attrRef
	op    string
	value any
}

// newCompare refuses an operator or value the attribute's type has no meaning for.
func newCompare(ref attrRef, op string, value any) (expr, error) {
	switch ref.def.typ {
	case typeBool:
		if _, ok := value.(bool); !ok || (op != "eq" && op != "ne") {
			return nil, badRequest(typeInvalidFilter, "%q is a boolean: only eq and ne with true or false apply", ref.name)
		}
	case typeString:
		s, ok := value.(string)
		if !ok {
			return nil, badRequest(typeInvalidFilter, "%q is a string: compare it with a quoted value", ref.name)
		}
		if !ref.def.caseExact {
			value = strings.ToLower(s)
		}
	}
	return compareExpr{attr: ref, op: op, value: value}, nil
}

// eval is true when any value matches; ne is true when none equals, so an absent attribute is not equal to anything.
func (e compareExpr) eval(doc map[string]any) bool {
	if e.op == "ne" {
		return !e.any(doc, "eq")
	}
	return e.any(doc, e.op)
}

func (e compareExpr) any(doc map[string]any, op string) bool {
	for _, v := range e.attr.values(doc) {
		if b, ok := v.(bool); ok {
			if b == e.value {
				return true
			}
			continue
		}
		s, _ := v.(string)
		if !e.attr.def.caseExact {
			s = strings.ToLower(s)
		}
		if matchString(op, s, e.value.(string)) {
			return true
		}
	}
	return false
}

func matchString(op, have, want string) bool {
	switch op {
	case "eq":
		return have == want
	case "co":
		return strings.Contains(have, want)
	case "sw":
		return strings.HasPrefix(have, want)
	case "ew":
		return strings.HasSuffix(have, want)
	case "gt":
		return have > want
	case "ge":
		return have >= want
	case "lt":
		return have < want
	case "le":
		return have <= want
	}
	return false
}
