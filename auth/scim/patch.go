package scim

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strings"
)

// maxPatchOps bounds the operations of one PATCH request.
const maxPatchOps = 1000

const (
	opAdd     = "add"
	opReplace = "replace"
	opRemove  = "remove"
)

type patchOp struct {
	op   string
	path string
	// value is nil when the operation carries none or a null one.
	value json.RawMessage
}

// decodePatch reads a PatchOp body. Operation names are case-insensitive: Microsoft Entra sends "Replace" and "Add".
func decodePatch(raw []byte) ([]patchOp, error) {
	var body struct {
		Schemas    []string `json:"schemas"`
		Operations []struct {
			Op    string          `json:"op"`
			Path  string          `json:"path"`
			Value json.RawMessage `json:"value"`
		} `json:"Operations"`
	}
	if err := decodeBody(raw, &body); err != nil {
		return nil, err
	}
	if !slices.ContainsFunc(body.Schemas, func(s string) bool { return strings.EqualFold(s, schemaPatchOp) }) {
		return nil, badRequest(typeInvalidSyntax, "the request is not a %s message", schemaPatchOp)
	}
	if len(body.Operations) == 0 {
		return nil, badRequest(typeInvalidSyntax, "Operations must hold at least one operation")
	}
	if len(body.Operations) > maxPatchOps {
		return nil, errorf(http.StatusRequestEntityTooLarge, typeTooMany, "a request may hold at most %d operations", maxPatchOps)
	}
	ops := make([]patchOp, 0, len(body.Operations))
	for _, o := range body.Operations {
		op := strings.ToLower(o.Op)
		if op != opAdd && op != opReplace && op != opRemove {
			return nil, badRequest(typeInvalidSyntax, "unknown operation %q", o.Op)
		}
		v := o.Value
		if bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			v = nil
		}
		ops = append(ops, patchOp{op: op, path: strings.TrimSpace(o.Path), value: v})
	}
	return ops, nil
}

// patchPath is a PATCH target: an attribute, an optional value filter over its elements, and an optional sub-attribute.
type patchPath struct {
	name, sub string
	filter    expr
	// ext marks an attribute of a schema the handler does not hold, such as the enterprise extension.
	ext bool
}

// parsePatchPath parses an RFC 7644 PATCH path: attrPath, or valuePath with an optional ".subAttr" after it, as in emails[type eq "work"].value.
func parsePatchPath(s string, schema *resourceSchema) (patchPath, error) {
	pp, err := parsePatchPathTokens(s, schema)
	if e, ok := errors.AsType[*Error](err); ok && e.Type == typeInvalidFilter {
		e.Type = typeInvalidPath
	}
	return pp, err
}

func parsePatchPathTokens(s string, schema *resourceSchema) (patchPath, error) {
	toks, err := lex(s)
	if err != nil {
		return patchPath{}, err
	}
	if toks[0].kind != tokWord {
		return patchPath{}, badRequest(typeInvalidPath, "path %q names no attribute", s)
	}
	name, sub, ext, err := schema.splitPath(toks[0].text)
	if err != nil || ext {
		return patchPath{ext: ext}, err
	}
	pp := patchPath{name: name, sub: sub}
	i := 1
	if toks[i].kind == tokLBracket {
		def := schema.attrs[name]
		if def == nil || !def.multi || sub != "" {
			return patchPath{}, badRequest(typeInvalidPath, "%q cannot take a value filter", name)
		}
		p := &filterParser{toks: toks, i: i + 1, schema: schema, attrs: def.subs, inside: name}
		if pp.filter, err = p.parseOr(1); err != nil {
			return patchPath{}, err
		}
		if t := p.next(); t.kind != tokRBracket {
			return patchPath{}, badRequest(typeInvalidPath, "missing ] in path %q", s)
		}
		i = p.i
		if t := toks[i]; t.kind == tokWord && strings.HasPrefix(t.text, ".") {
			pp.sub = strings.ToLower(t.text[1:])
			if !validAttrName(pp.sub) {
				return patchPath{}, badRequest(typeInvalidPath, "%q is not a sub-attribute", t.text)
			}
			i++
		}
	}
	if toks[i].kind != tokEOF {
		return patchPath{}, badRequest(typeInvalidPath, "unexpected %q in path %q", toks[i].text, s)
	}
	return pp, nil
}

// patchTarget is a resource being patched, held as the fields the handler keeps.
type patchTarget interface {
	// apply runs one operation on one known attribute of the target's schema.
	apply(op string, p patchPath, value json.RawMessage) error
}

// applyPatch runs ops in order on t. An attribute of another schema, one the store cannot hold, or a read-only one a client echoes (id, meta, schemas) is skipped; any other unknown attribute is refused.
func applyPatch(ops []patchOp, schema *resourceSchema, t patchTarget) error {
	for _, op := range ops {
		if op.path != "" {
			if op.op != opRemove && op.value == nil {
				return badRequest(typeInvalidValue, "%s %q needs a value", op.op, op.path)
			}
			if err := applyAt(op.op, op.path, op.value, schema, t); err != nil {
				return err
			}
			continue
		}
		if op.op == opRemove {
			return badRequest(typeNoTarget, "remove needs a path")
		}
		if err := applyPathless(op.op, op.value, schema, t); err != nil {
			return err
		}
	}
	return nil
}

// applyPathless runs an add or replace with no path, whose value object names each target as a key, as Okta and Microsoft Entra send for active and for several attributes at once. Keys run in sorted order so the result does not depend on how the object was read.
func applyPathless(op string, value json.RawMessage, schema *resourceSchema, t patchTarget) error {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(value, &members); err != nil {
		return badRequest(typeInvalidValue, "%s without a path needs an object value", op)
	}
	for _, key := range slices.Sorted(maps.Keys(members)) {
		v := members[key]
		if strings.EqualFold(key, schema.urn) {
			if err := applyPathless(op, v, schema, t); err != nil {
				return err
			}
			continue
		}
		if err := applyAt(op, key, v, schema, t); err != nil {
			return err
		}
	}
	return nil
}

func applyAt(op, path string, value json.RawMessage, schema *resourceSchema, t patchTarget) error {
	pp, err := parsePatchPath(path, schema)
	if err != nil {
		return err
	}
	switch {
	case pp.ext, schema.ignored[pp.name], pp.name == "id", pp.name == "meta", pp.name == "schemas":
		return nil
	case schema.attrs[pp.name] == nil:
		return badRequest(typeInvalidPath, "%q is not an attribute of %s", path, schema.urn)
	}
	return t.apply(op, pp, value)
}

func decodeString(v json.RawMessage, attr string) (string, error) {
	var s string
	if err := json.Unmarshal(v, &s); err != nil {
		return "", badRequest(typeInvalidValue, "%s must be a string", attr)
	}
	return s, nil
}

func decodeBool(v json.RawMessage, attr string) (bool, error) {
	var x any
	if err := json.Unmarshal(v, &x); err != nil {
		return false, badRequest(typeInvalidValue, "%s must be a boolean", attr)
	}
	return parseBool(x)
}

// decodeOneOrMany reads a value that is an array of T or a single T.
func decodeOneOrMany[T any](v json.RawMessage, attr string) ([]T, error) {
	trimmed := bytes.TrimSpace(v)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		var one T
		if err := json.Unmarshal(trimmed, &one); err != nil {
			return nil, badRequest(typeInvalidValue, "%s: %s", attr, err.Error())
		}
		return []T{one}, nil
	}
	var many []T
	if err := json.Unmarshal(trimmed, &many); err != nil {
		return nil, badRequest(typeInvalidValue, "%s must be an object or an array of objects", attr)
	}
	return many, nil
}
