package manifest

import (
	"fmt"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"
)

// unknownField reports a key the kind does not define, so a typo fails instead of vanishing.
type unknownField struct{ key string }

func (e unknownField) Error() string { return fmt.Sprintf("unknown field %q", e.key) }

// fieldError is a problem at a path below the node checkKnown started from.
type fieldError struct {
	path string
	err  error
}

func (e *fieldError) Error() string { return e.path + ": " + e.err.Error() }
func (e *fieldError) Unwrap() error { return e.err }

// nodeType is a raw node a later step checks with its own rules.
var nodeType = reflect.TypeFor[yaml.Node]()

// checkKnown walks n against t and refuses any mapping key t has no field for. Shape mismatches are left to the decoder, which reports them with a line number.
func checkKnown(n *yaml.Node, t reflect.Type, path string) error {
	if n == nil {
		return nil
	}
	if n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nodeType {
		return nil
	}
	switch t.Kind() {
	case reflect.Struct:
		if n.Kind != yaml.MappingNode {
			return nil
		}
		for i := 0; i+1 < len(n.Content); i += 2 {
			key := n.Content[i].Value
			ft, ok := yamlField(t, key)
			if !ok {
				return &fieldError{path: join(path, key), err: unknownField{key}}
			}
			if err := checkKnown(n.Content[i+1], ft, join(path, key)); err != nil {
				return err
			}
		}
	case reflect.Slice:
		if n.Kind != yaml.SequenceNode {
			return nil
		}
		for i, el := range n.Content {
			if err := checkKnown(el, t.Elem(), item(path, i)); err != nil {
				return err
			}
		}
	case reflect.Map:
		if n.Kind != yaml.MappingNode {
			return nil
		}
		for i := 0; i+1 < len(n.Content); i += 2 {
			if err := checkKnown(n.Content[i+1], t.Elem(), join(path, n.Content[i].Value)); err != nil {
				return err
			}
		}
	}
	return nil
}

// yamlField returns the type of the struct field t decodes key into.
func yamlField(t reflect.Type, key string) (reflect.Type, bool) {
	for i := range t.NumField() {
		f := t.Field(i)
		if yamlKey(f) == key {
			return f.Type, true
		}
	}
	return nil, false
}

// yamlKeys lists the keys t decodes, in field order.
func yamlKeys(t reflect.Type) []string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	keys := make([]string, 0, t.NumField())
	for i := range t.NumField() {
		if k := yamlKey(t.Field(i)); k != "" {
			keys = append(keys, k)
		}
	}
	return keys
}

func yamlKey(f reflect.StructField) string {
	if !f.IsExported() {
		return ""
	}
	name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
	switch name {
	case "-":
		return ""
	case "":
		return strings.ToLower(f.Name)
	}
	return name
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// mappingValue returns the value under key in mapping n, or nil.
func mappingValue(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

// specTypes is the spec struct of each kind.
var specTypes = map[string]reflect.Type{
	KindTeam:           reflect.TypeFor[TeamSpec](),
	KindProject:        reflect.TypeFor[ProjectSpec](),
	KindServiceAccount: reflect.TypeFor[ServiceAccountSpec](),
	KindRole:           reflect.TypeFor[RoleSpec](),
	KindUser:           reflect.TypeFor[UserSpec](),
	KindGroup:          reflect.TypeFor[GroupSpec](),
	KindRoleBinding:    reflect.TypeFor[RoleBindingSpec](),
}

// specKeys lists the built-in spec keys of kind.
func specKeys(kind string) []string { return yamlKeys(specTypes[kind]) }
