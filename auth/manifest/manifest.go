// Package manifest reads and writes the directory's rows as YAML documents of apiVersion auth.wyolet.dev/v1alpha1: Team, Project, ServiceAccount, Role, User, Group and RoleBinding. Documents refer to other rows by name; the package resolves names to ids on the way in and renders ids back to names on the way out.
//
// A product registers its vocabulary (the kinds and verbs a role rule may name) and any spec fields of its own through a Registry. Parse decodes strictly: an unknown field, a rule term outside the vocabulary or a plain-text password is an error naming the document and the field. Plan diffs the documents against the stored rows, Apply writes the difference through directory.Directory so its validation and grant checks run, and prune deletes only rows carrying the labels that mark the manifest-owned set. JSONSchema describes each kind for editors and agents.
//
// Out of scope: reading files or directories, HTTP handlers, a CLI, authorizing the caller to apply at all (the product does that before Apply), and the product's own kinds.
package manifest

import "encoding/json"

// APIVersion is the apiVersion every document of this package declares.
const APIVersion = "auth.wyolet.dev/v1alpha1"

// The kinds a document may carry.
const (
	KindTeam           = "Team"
	KindProject        = "Project"
	KindServiceAccount = "ServiceAccount"
	KindRole           = "Role"
	KindUser           = "User"
	KindGroup          = "Group"
	KindRoleBinding    = "RoleBinding"
)

// Kinds returns every kind in the order Apply writes them: each kind only refers to kinds before it.
func Kinds() []string {
	return []string{KindTeam, KindProject, KindServiceAccount, KindRole, KindUser, KindGroup, KindRoleBinding}
}

// IsKind reports whether kind is one of this package's kinds.
func IsKind(kind string) bool {
	for _, k := range Kinds() {
		if k == kind {
			return true
		}
	}
	return false
}

// Document is one decoded manifest document. Exactly one spec pointer is set, the one Kind names.
type Document struct {
	// Index is the document's position in the stream it came from, counting empty documents, from 0.
	Index      int
	APIVersion string
	Kind       string
	Metadata   Metadata

	Team           *TeamSpec
	Project        *ProjectSpec
	ServiceAccount *ServiceAccountSpec
	Role           *RoleSpec
	User           *UserSpec
	Group          *GroupSpec
	RoleBinding    *RoleBindingSpec

	// Fields are the product's spec fields as JSON, keyed by field name.
	Fields map[string]json.RawMessage
}

// Name is the document's metadata.name.
func (d *Document) Name() string { return d.Metadata.Name }

// Metadata is the identity block every document carries. A User document takes only name.
type Metadata struct {
	// ID is informational: ids are minted by the store, and a document whose id differs from the row its name resolves to is reported, not obeyed.
	ID          string `yaml:"id,omitempty"          json:"id,omitempty"`
	Name        string `yaml:"name"                  json:"name"`
	DisplayName string `yaml:"displayName,omitempty" json:"displayName,omitempty"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
	// Owner is accepted so documents written for older loaders still parse, and ignored: every kind's owner is derived (the system for Team, Role and Group, the parent for Project and ServiceAccount, the scope for RoleBinding).
	Owner       *Owner            `yaml:"owner,omitempty"       json:"owner,omitempty"`
	Labels      map[string]string `yaml:"labels,omitempty"      json:"labels,omitempty"`
	Annotations map[string]string `yaml:"annotations,omitempty" json:"annotations,omitempty"`
}

// Owner is the shape metadata.owner takes.
type Owner struct {
	Kind string `yaml:"kind,omitempty" json:"kind,omitempty"`
	Name string `yaml:"name,omitempty" json:"name,omitempty"`
	ID   string `yaml:"id,omitempty"   json:"id,omitempty"`
}

// TeamSpec is a team. Enabled nil means enabled, here and in every spec below.
type TeamSpec struct {
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
}

// ProjectSpec names the team the project lives in.
type ProjectSpec struct {
	Team    string `yaml:"team"              json:"team"`
	Enabled *bool  `yaml:"enabled,omitempty" json:"enabled,omitempty"`
}

// ServiceAccountSpec names the project the account lives in.
type ServiceAccountSpec struct {
	Project string `yaml:"project"           json:"project"`
	Enabled *bool  `yaml:"enabled,omitempty" json:"enabled,omitempty"`
}

// RoleSpec holds the rules; each term is in the product's vocabulary or the wildcard.
type RoleSpec struct {
	Rules   []Rule `yaml:"rules"             json:"rules"`
	Enabled *bool  `yaml:"enabled,omitempty" json:"enabled,omitempty"`
}

// Rule grants every verb on every kind it lists.
type Rule struct {
	Kinds []string `yaml:"kinds" json:"kinds"`
	Verbs []string `yaml:"verbs" json:"verbs"`
}

// GroupSpec lists members by username.
type GroupSpec struct {
	Members []string `yaml:"members,omitempty" json:"members,omitempty"`
	Enabled *bool    `yaml:"enabled,omitempty" json:"enabled,omitempty"`
}

// RoleBindingSpec grants a role, by name, to subjects at a scope.
type RoleBindingSpec struct {
	Role     string    `yaml:"role"              json:"role"`
	Scope    Scope     `yaml:"scope"             json:"scope"`
	Subjects []Subject `yaml:"subjects"          json:"subjects"`
	Enabled  *bool     `yaml:"enabled,omitempty" json:"enabled,omitempty"`
}

// Scope is where a binding applies. A team or project scope is named; the system scope carries no name; any other kind is the product's and passes its id (or name, when no id is given) through unresolved.
type Scope struct {
	Kind string `yaml:"kind"           json:"kind"`
	Name string `yaml:"name,omitempty" json:"name,omitempty"`
	ID   string `yaml:"id,omitempty"   json:"id,omitempty"`
}

// ref is the identifier the document gave; an id wins over a name.
func (s Scope) ref() string {
	if s.ID != "" {
		return s.ID
	}
	return s.Name
}

// Subject is a grantee by name: a username, a service account's name, or a group's name (stored or asserted by an identity provider).
type Subject struct {
	Kind string `yaml:"kind" json:"kind"`
	Name string `yaml:"name" json:"name"`
}
