package v1

// OwnerKind says what kind of party a request is billed to.
type OwnerKind string

// Owner kinds a request can be billed to.
const (
	OwnerUser    OwnerKind = "user"
	OwnerTeam    OwnerKind = "team"
	OwnerProject OwnerKind = "project"
)

// Owner names who a request is billed to. It mirrors the wire shape of the
// server's owner but is defined here because the SDK imports nothing from
// app/ or internal/.
type Owner struct {
	Kind OwnerKind `json:"kind,omitempty" yaml:"kind,omitempty"`
	ID   string    `json:"id,omitempty"   yaml:"id,omitempty"`
}

// Tenant reports whether the owner is a user, team or project.
func (o Owner) Tenant() bool {
	switch o.Kind {
	case OwnerUser, OwnerTeam, OwnerProject:
		return true
	}
	return false
}
