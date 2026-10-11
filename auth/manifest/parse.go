package manifest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"

	"gopkg.in/yaml.v3"
)

// Parse decodes a stream of YAML documents (--- separated); empty documents are skipped but counted, so Index matches the stream. Every document must be one of this package's kinds under an accepted apiVersion.
func (r *Registry) Parse(rd io.Reader) ([]Document, error) {
	dec := yaml.NewDecoder(rd)
	var docs []Document
	for i := 0; ; i++ {
		var node yaml.Node
		err := dec.Decode(&node)
		if errors.Is(err, io.EOF) {
			return docs, nil
		}
		if err != nil {
			return nil, &Error{Doc: i, Err: err}
		}
		if isEmpty(&node) {
			continue
		}
		d, err := r.Decode(i, &node)
		if err != nil {
			return nil, err
		}
		docs = append(docs, d)
	}
}

// Decode decodes one document node, for a product that reads the stream itself and hands this package the documents of its kinds. index is the document's position in that stream.
func (r *Registry) Decode(index int, node *yaml.Node) (Document, error) {
	if node.Kind == yaml.DocumentNode && len(node.Content) == 1 {
		node = node.Content[0]
	}
	d := Document{Index: index}
	if node.Kind != yaml.MappingNode {
		return d, &Error{Doc: index, Err: errors.New("a document must be a mapping")}
	}
	var env struct {
		APIVersion string    `yaml:"apiVersion"`
		Kind       string    `yaml:"kind"`
		Metadata   yaml.Node `yaml:"metadata"`
		Spec       yaml.Node `yaml:"spec"`
	}
	if err := checkKnown(node, reflect.TypeOf(env), ""); err != nil {
		return d, pathError(&d, err)
	}
	if err := node.Decode(&env); err != nil {
		return d, &Error{Doc: index, Err: err}
	}
	d.APIVersion, d.Kind = env.APIVersion, env.Kind
	if !IsKind(d.Kind) {
		return d, &Error{Doc: index, Field: "kind", Err: fmt.Errorf("unknown kind %q", d.Kind)}
	}
	if !r.AcceptsAPIVersion(d.APIVersion) {
		return d, &Error{Doc: index, Kind: d.Kind, Field: "apiVersion", Err: fmt.Errorf("unsupported apiVersion %q (want %q)", d.APIVersion, APIVersion)}
	}
	if err := r.decodeMetadata(&d, &env.Metadata); err != nil {
		return d, err
	}
	if err := r.decodeSpec(&d, &env.Spec); err != nil {
		return d, err
	}
	if err := r.check(&d); err != nil {
		return d, err
	}
	return d, nil
}

// userMetadata is the only metadata a User takes: an account has no labels, annotations or display name to store them in.
type userMetadata struct {
	ID   string `yaml:"id,omitempty"`
	Name string `yaml:"name"`
}

func (r *Registry) decodeMetadata(d *Document, n *yaml.Node) error {
	t := reflect.TypeFor[Metadata]()
	if d.Kind == KindUser {
		t = reflect.TypeFor[userMetadata]()
	}
	if err := checkKnown(n, t, "metadata"); err != nil {
		return pathError(d, err)
	}
	if n.Kind != 0 {
		if err := n.Decode(&d.Metadata); err != nil {
			return &Error{Doc: d.Index, Kind: d.Kind, Field: "metadata", Err: err}
		}
	}
	if d.Metadata.Name == "" {
		return &Error{Doc: d.Index, Kind: d.Kind, Field: "metadata.name", Err: errors.New("required")}
	}
	return nil
}

func (r *Registry) decodeSpec(d *Document, n *yaml.Node) error {
	t := specTypes[d.Kind]
	if d.Kind == KindUser {
		if err := refusePlainPassword(d, n); err != nil {
			return err
		}
	}
	builtin := specKeys(d.Kind)
	if n.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(n.Content); i += 2 {
			key, value := n.Content[i].Value, n.Content[i+1]
			if slices.Contains(builtin, key) {
				ft, _ := yamlField(t, key)
				if err := checkKnown(value, ft, "spec."+key); err != nil {
					return pathError(d, err)
				}
				continue
			}
			if !r.hasField(d.Kind, key) {
				return &Error{Doc: d.Index, Kind: d.Kind, Name: d.Metadata.Name, Field: "spec." + key, Err: unknownField{key}}
			}
			raw, err := nodeJSON(value)
			if err != nil {
				return &Error{Doc: d.Index, Kind: d.Kind, Name: d.Metadata.Name, Field: "spec." + key, Err: err}
			}
			if d.Fields == nil {
				d.Fields = map[string]json.RawMessage{}
			}
			d.Fields[key] = raw
		}
	}
	spec := reflect.New(t)
	if n.Kind != 0 {
		if err := n.Decode(spec.Interface()); err != nil {
			return &Error{Doc: d.Index, Kind: d.Kind, Name: d.Metadata.Name, Field: "spec", Err: err}
		}
	}
	setSpec(d, spec.Interface())
	return nil
}

func (r *Registry) hasField(kind, name string) bool {
	for _, f := range r.fields(kind) {
		if f.Name == name {
			return true
		}
	}
	return false
}

// refusePlainPassword gives a password written in clear text its own error rather than an unknown-field one.
func refusePlainPassword(d *Document, spec *yaml.Node) error {
	p := mappingValue(spec, "password")
	if p == nil {
		return nil
	}
	if p.Kind == yaml.ScalarNode && p.Tag != "!!null" {
		return &Error{Doc: d.Index, Kind: d.Kind, Name: d.Metadata.Name, Field: "spec.password", Err: ErrPlainPassword}
	}
	if mappingValue(p, "value") != nil {
		return &Error{Doc: d.Index, Kind: d.Kind, Name: d.Metadata.Name, Field: "spec.password.value", Err: ErrPlainPassword}
	}
	return nil
}

func setSpec(d *Document, spec any) {
	switch s := spec.(type) {
	case *TeamSpec:
		d.Team = s
	case *ProjectSpec:
		d.Project = s
	case *ServiceAccountSpec:
		d.ServiceAccount = s
	case *RoleSpec:
		d.Role = s
	case *UserSpec:
		d.User = s
	case *GroupSpec:
		d.Group = s
	case *RoleBindingSpec:
		d.RoleBinding = s
	}
}

// nodeJSON renders a YAML value as JSON, the form product fields are handed over in.
func nodeJSON(n *yaml.Node) (json.RawMessage, error) {
	var v any
	if err := n.Decode(&v); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("not representable as JSON: %w", err)
	}
	return raw, nil
}

func pathError(d *Document, err error) error {
	var fe *fieldError
	if errors.As(err, &fe) {
		return &Error{Doc: d.Index, Kind: d.Kind, Name: d.Metadata.Name, Field: fe.path, Err: fe.err}
	}
	return &Error{Doc: d.Index, Kind: d.Kind, Name: d.Metadata.Name, Err: err}
}

func isEmpty(n *yaml.Node) bool {
	if n.Kind == yaml.DocumentNode && len(n.Content) == 1 {
		n = n.Content[0]
	}
	return n.Kind == 0 || (n.Kind == yaml.ScalarNode && n.Tag == "!!null")
}
