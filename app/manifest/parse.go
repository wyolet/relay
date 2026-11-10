package manifest

import (
	"io"

	authmanifest "github.com/wyolet/relay/auth/manifest"
	catalogmanifest "github.com/wyolet/relay/pkg/catalogkind/manifest"
)

// Document is a discriminated union over all supported wire kinds. Exactly one
// of the fields below is non-nil after successful parsing.
type Document struct {
	Provider    *ProviderDTO
	Host        *HostDTO
	Model       *ModelDTO
	HostKey     *HostKeyDTO
	Policy      *PolicyDTO
	Account     *AccountDTO
	Pool        *PoolDTO
	Budget      *BudgetDTO
	RateLimit   *RateLimitDTO
	Key         *KeyDTO
	Pricing     *PricingDTO
	HostBinding *HostBindingDTO
	Setting     *SettingDTO
	Team        *TeamDTO
	Project     *ProjectDTO

	// Foreign carries a kind owned by a sibling subsystem (User) that shares
	// the config tree. LoadDir drops these; a caller parsing a submitted
	// bundle sees them, so it can refuse rather than ignore them silently.
	Foreign string

	// User is an account under the auth module's apiVersion; relay has no
	// wire DTO of its own for it.
	User *authmanifest.Document

	ServiceAccount *ServiceAccountDTO
	Group          *GroupDTO
	Role           *RoleDTO
	RoleBinding    *RoleBindingDTO
	PolicyBinding  *PolicyBindingDTO
	Overlay        *OverlayDTO
}

// Kind returns the kind string of the contained document.
func (d Document) Kind() string {
	switch {
	case d.Provider != nil:
		return "Provider"
	case d.Host != nil:
		return "Host"
	case d.Model != nil:
		return "Model"
	case d.HostKey != nil:
		return "HostKey"
	case d.Policy != nil:
		return "Policy"
	case d.Account != nil:
		return "Account"
	case d.Pool != nil:
		return "Pool"
	case d.Budget != nil:
		return "Budget"
	case d.RateLimit != nil:
		return "RateLimit"
	case d.Key != nil:
		return "Key"
	case d.Pricing != nil:
		return "Pricing"
	case d.HostBinding != nil:
		return "HostBinding"
	case d.Setting != nil:
		return "Setting"
	case d.Team != nil:
		return "Team"
	case d.Project != nil:
		return "Project"
	case d.ServiceAccount != nil:
		return "ServiceAccount"
	case d.Group != nil:
		return "Group"
	case d.Role != nil:
		return "Role"
	case d.RoleBinding != nil:
		return "RoleBinding"
	case d.PolicyBinding != nil:
		return "PolicyBinding"
	case d.Overlay != nil:
		return "Overlay"
	case d.User != nil:
		return authmanifest.KindUser
	case d.Foreign != "":
		return d.Foreign
	default:
		return ""
	}
}

// relayParser reads every kind relay knows: the catalog's and the ones newRelayParser registers.
var relayParser = newRelayParser()

// Parse reads one or more YAML documents from r and returns a Document slice.
// Multi-doc YAML (--- separated) is fully supported. The auth module's kinds
// are decoded by the module, strictly, under either apiVersion.
func Parse(r io.Reader) ([]Document, error) {
	docs, err := relayParser.Parse(r)
	return fromParsed(docs), err
}

// LoadDir walks dir for .yaml / .yml files and parses each, skipping drafts;
// see catalogmanifest.Parser.LoadDir. Foreign kinds are dropped.
func LoadDir(dir string) ([]Document, error) {
	docs, err := relayParser.LoadDir(dir)
	return fromParsed(docs), err
}

// fromParsed lifts parsed documents into relay's Document: the kinds this package registers carry theirs in Ext.
func fromParsed(docs []catalogmanifest.Document) []Document {
	if docs == nil {
		return nil
	}
	out := make([]Document, len(docs))
	for i, d := range docs {
		if ext, ok := d.Ext.(Document); ok {
			out[i] = ext
			continue
		}
		out[i] = Document{
			Provider:    d.Provider,
			Host:        d.Host,
			Model:       d.Model,
			Policy:      d.Policy,
			RateLimit:   d.RateLimit,
			Pricing:     d.Pricing,
			HostBinding: d.HostBinding,
			Foreign:     d.Foreign,
		}
	}
	return out
}

// toParsed wraps a Document of a kind this package registers for the catalog parser.
func toParsed(d Document) catalogmanifest.Document {
	return catalogmanifest.Document{Ext: d}
}
