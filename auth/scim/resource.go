package scim

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/wyolet/relay/auth/directory"
)

// emailType is the type every email is rendered with: the store holds one address per account.
const emailType = "work"

type metaOut struct {
	ResourceType string `json:"resourceType"`
	Created      string `json:"created,omitempty"`
	LastModified string `json:"lastModified,omitempty"`
	Location     string `json:"location,omitempty"`
}

type emailOut struct {
	Value   string `json:"value"`
	Type    string `json:"type"`
	Primary bool   `json:"primary"`
}

// ref is a member of a group or a group of a user.
type ref struct {
	Value string `json:"value"`
	Ref   string `json:"$ref,omitempty"`
}

type userResource struct {
	Schemas    []string   `json:"schemas"`
	ID         string     `json:"id"`
	ExternalID string     `json:"externalId,omitempty"`
	UserName   string     `json:"userName"`
	Active     bool       `json:"active"`
	Emails     []emailOut `json:"emails,omitempty"`
	Groups     []ref      `json:"groups,omitempty"`
	Meta       metaOut    `json:"meta"`
}

type groupResource struct {
	Schemas     []string `json:"schemas"`
	ID          string   `json:"id"`
	ExternalID  string   `json:"externalId,omitempty"`
	DisplayName string   `json:"displayName"`
	Members     []ref    `json:"members,omitempty"`
	Meta        metaOut  `json:"meta"`
}

func timestamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// location is the resource's URL under the handler's base URL, empty when none is configured.
func (h *Handler) location(endpoint, id string) string {
	if h.cfg.BaseURL == "" {
		return ""
	}
	return strings.TrimRight(h.cfg.BaseURL, "/") + "/" + endpoint + "/" + id
}

func (h *Handler) renderUser(u *directory.User, groups []*directory.Group) userResource {
	out := userResource{
		Schemas: []string{schemaUser}, ID: u.ID, ExternalID: u.External.ID, UserName: u.Username, Active: !u.Disabled,
		Meta: metaOut{ResourceType: "User", Created: timestamp(u.CreatedAt), LastModified: timestamp(u.UpdatedAt), Location: h.location("Users", u.ID)},
	}
	if u.Email != "" {
		out.Emails = []emailOut{{Value: u.Email, Type: emailType, Primary: true}}
	}
	for _, g := range groups {
		out.Groups = append(out.Groups, ref{Value: g.Meta.ID, Ref: h.location("Groups", g.Meta.ID)})
	}
	return out
}

func (h *Handler) renderGroup(g *directory.Group) groupResource {
	out := groupResource{
		Schemas: []string{schemaGroup}, ID: g.Meta.ID, ExternalID: g.Spec.External.ID, DisplayName: g.Meta.DisplayName,
		Meta: metaOut{ResourceType: "Group", Created: timestamp(g.Meta.CreatedAt), LastModified: timestamp(g.Meta.UpdatedAt),
			Location: h.location("Groups", g.Meta.ID)},
	}
	for _, id := range g.Spec.MemberIDs {
		out.Members = append(out.Members, ref{Value: id, Ref: h.location("Users", id)})
	}
	return out
}

// userDoc is the user as filters see it.
func userDoc(u *directory.User) map[string]any {
	d := map[string]any{"id": u.ID, "username": u.Username, "active": !u.Disabled}
	if u.External.ID != "" {
		d["externalid"] = u.External.ID
	}
	if u.Email != "" {
		d["emails"] = []map[string]any{{"value": u.Email, "type": emailType, "primary": true}}
	}
	return d
}

func groupDoc(g *directory.Group) map[string]any {
	d := map[string]any{"id": g.Meta.ID, "displayname": g.Meta.DisplayName}
	if g.Spec.External.ID != "" {
		d["externalid"] = g.Spec.External.ID
	}
	if len(g.Spec.MemberIDs) > 0 {
		members := make([]map[string]any, len(g.Spec.MemberIDs))
		for i, id := range g.Spec.MemberIDs {
			members[i] = map[string]any{"value": id}
		}
		d["members"] = members
	}
	return d
}

// flexBool reads a JSON boolean, or the strings "true" and "false" in any case, which Microsoft Entra sends for active.
type flexBool bool

func (b *flexBool) UnmarshalJSON(raw []byte) error {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	parsed, err := parseBool(v)
	if err != nil {
		return err
	}
	*b = flexBool(parsed)
	return nil
}

func parseBool(v any) (bool, error) {
	switch x := v.(type) {
	case bool:
		return x, nil
	case string:
		switch strings.ToLower(strings.TrimSpace(x)) {
		case "true":
			return true, nil
		case "false":
			return false, nil
		}
	}
	return false, badRequest(typeInvalidValue, "%v is not a boolean", v)
}

type emailIn struct {
	Value   string   `json:"value"`
	Type    string   `json:"type"`
	Primary flexBool `json:"primary"`
}

// primaryEmail picks the address the store keeps: the one marked primary, else the first.
func primaryEmail(emails []emailIn) string {
	for _, e := range emails {
		if e.Primary && e.Value != "" {
			return e.Value
		}
	}
	for _, e := range emails {
		if e.Value != "" {
			return e.Value
		}
	}
	return ""
}

// userIn is a POST or PUT body. Attributes the store cannot hold (name, displayName, phoneNumbers, …) and read-only ones (id, groups, meta) are not read.
type userIn struct {
	UserName   *string    `json:"userName"`
	ExternalID *string    `json:"externalId"`
	Active     *flexBool  `json:"active"`
	Emails     *[]emailIn `json:"emails"`
}

type memberIn struct {
	Value string `json:"value"`
}

// groupIn is a POST or PUT body.
type groupIn struct {
	DisplayName *string     `json:"displayName"`
	ExternalID  *string     `json:"externalId"`
	Members     *[]memberIn `json:"members"`
}

// memberIDs returns the members' ids in order, each once.
func memberIDs(ms []memberIn) ([]string, error) {
	out := make([]string, 0, len(ms))
	seen := make(map[string]bool, len(ms))
	for _, m := range ms {
		if m.Value == "" {
			return nil, badRequest(typeInvalidValue, "a member needs a value")
		}
		if !seen[m.Value] {
			seen[m.Value] = true
			out = append(out, m.Value)
		}
	}
	return out, nil
}

func decodeBody(raw []byte, v any) error {
	if err := json.Unmarshal(raw, v); err != nil {
		if e, ok := errors.AsType[*Error](err); ok {
			return e
		}
		return badRequest(typeInvalidSyntax, "%s", err.Error())
	}
	return nil
}
