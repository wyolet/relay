// Package identity owns the User catalog kind and the SecretRef resolution
// machinery used to pull credential material out of the environment, files,
// or (discouraged) inline literals — pydantic-SecretStr style.
//
// User objects are loaded from the same YAML tree as the rest of the catalog
// but kept in their own snapshot. Postgres tables for identity do not exist
// yet; seed simply validates and reports what it found so the schema can
// stabilise before the storage layer lands.
package identity

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

const APIVersion = "relay.wyolet.dev/v1"
const KindUser = "User"

type Metadata struct {
	Name   string            `yaml:"name"             json:"name"`
	Labels map[string]string `yaml:"labels,omitempty" json:"labels,omitempty"`
}

type User struct {
	APIVersion string   `yaml:"apiVersion" json:"apiVersion,omitempty"`
	Kind       string   `yaml:"kind"       json:"kind,omitempty"`
	Metadata   Metadata `yaml:"metadata"   json:"metadata"`
	Spec       UserSpec `yaml:"spec"       json:"spec"`
}

type UserSpec struct {
	Username SecretRef `yaml:"username" json:"username"`
	Email    SecretRef `yaml:"email"    json:"email"`
	Password SecretRef `yaml:"password" json:"password"`
	Roles    []string  `yaml:"roles,omitempty" json:"roles,omitempty"`
}

// SecretRef carries either an inline value or a reference to one of several
// out-of-band sources. Exactly one of Value or ValueFrom must be set.
//
// Resolved is populated by Resolve() and is never marshalled — callers read
// the cleartext via Get().
type SecretRef struct {
	Value     string         `yaml:"value,omitempty"     json:"-"`
	ValueFrom *ValueFromSpec `yaml:"valueFrom,omitempty" json:"valueFrom,omitempty"`

	resolved string
	source   string // "env:VAR", "file:/path", "literal", "" (unresolved)
}

// UnmarshalYAML accepts two shapes:
//
//	username: admin                         # plain scalar, equivalent to {value: admin}
//	password: {valueFrom: {env: PW}}        # full mapping
//
// The scalar form is the ergonomic default for non-sensitive fields. The
// mapping form is required when reading from env or file.
func (s *SecretRef) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		s.Value = node.Value
		return nil
	}
	type raw struct {
		Value     string         `yaml:"value,omitempty"`
		ValueFrom *ValueFromSpec `yaml:"valueFrom,omitempty"`
	}
	var r raw
	if err := node.Decode(&r); err != nil {
		return fmt.Errorf("SecretRef: expected string or {value,valueFrom} mapping: %w", err)
	}
	s.Value = r.Value
	s.ValueFrom = r.ValueFrom
	return nil
}

// ValueFromSpec is the discriminated union of supported indirect sources.
// Exactly one of Env or File may be non-empty.
type ValueFromSpec struct {
	Env  string `yaml:"env,omitempty"  json:"env,omitempty"`
	File string `yaml:"file,omitempty" json:"file,omitempty"`
}

// Get returns the resolved cleartext. Panics if Resolve has not been called —
// this is a programmer error, not a runtime condition.
func (s *SecretRef) Get() string {
	if s.source == "" {
		panic("identity.SecretRef.Get: not resolved (call Resolve first)")
	}
	return s.resolved
}

// Source describes where the value came from. Useful for log lines that
// must not leak the value itself.
func (s *SecretRef) Source() string { return s.source }

// IsLiteral reports whether the value was set inline. Operators can use this
// to gate warnings about cleartext credentials in YAML.
func (s *SecretRef) IsLiteral() bool { return s.source == "literal" }

// Resolve fills the cleartext. fieldPath is used in error messages only.
func (s *SecretRef) Resolve(fieldPath string) error {
	hasInline := s.Value != ""
	hasFrom := s.ValueFrom != nil && (s.ValueFrom.Env != "" || s.ValueFrom.File != "")

	switch {
	case hasInline && hasFrom:
		return fmt.Errorf("%s: set either value or valueFrom, not both", fieldPath)
	case !hasInline && !hasFrom:
		return fmt.Errorf("%s: missing value or valueFrom", fieldPath)
	case hasInline:
		s.resolved = s.Value
		s.source = "literal"
		return nil
	}

	switch {
	case s.ValueFrom.Env != "" && s.ValueFrom.File != "":
		return fmt.Errorf("%s: valueFrom must specify exactly one of env or file", fieldPath)
	case s.ValueFrom.Env != "":
		v, ok := os.LookupEnv(s.ValueFrom.Env)
		if !ok || v == "" {
			return fmt.Errorf("%s: env var %q not set or empty", fieldPath, s.ValueFrom.Env)
		}
		s.resolved = v
		s.source = "env:" + s.ValueFrom.Env
	case s.ValueFrom.File != "":
		b, err := os.ReadFile(s.ValueFrom.File)
		if err != nil {
			return fmt.Errorf("%s: read file %q: %w", fieldPath, s.ValueFrom.File, err)
		}
		// Trim a single trailing newline, the universal "echo into a file" artefact.
		// Don't TrimSpace — passwords can legitimately contain leading/trailing
		// whitespace and we shouldn't second-guess the operator on that.
		s.resolved = strings.TrimRight(string(b), "\n")
		s.source = "file:" + s.ValueFrom.File
	}
	return nil
}
