// Package manifest reads the catalog kinds (Provider, Host, Model, Pricing, HostBinding, Policy, RateLimit) from their YAML wire form, which references entities by name, and translates them to the pkg/catalogkind domain structs, which reference them by id.
//
// A Parser owns the document envelope and decodes the kinds registered on it; NewParser registers the catalog kinds and a caller adds its own with Register. Rendering, and the kinds beyond the catalog's (tenancy, credentials, accounts), belong to the caller (relay's app/manifest), never to this package.
package manifest

import "fmt"

// APIVersion is the catalog schema version every relay-aware YAML declares.
// v1alpha3 reshapes Policy (pools, rateLimits, budgets), HostKey (account
// instead of a tier policy), Key (an account, no policy) and PolicyBinding
// (accounts), and adds Account, Pool and Budget.
const APIVersion = "relay.wyolet.dev/v1alpha3"

// PreviousAPIVersion is still accepted for the kinds v1alpha3 left unchanged;
// a reshaped kind under it is refused with a pointer to the new shape, since
// the move is manual and a silently dropped field would widen access.
const PreviousAPIVersion = "relay.wyolet.dev/v1alpha2"

// checkAPIVersion accepts APIVersion for every kind and PreviousAPIVersion
// for a kind registered without a reshape hint.
func (p *Parser) checkAPIVersion(apiVersion, kind string) error {
	switch apiVersion {
	case APIVersion:
		return nil
	case PreviousAPIVersion:
		hint := p.kinds[kind].ReshapedHint
		if hint == "" {
			return nil
		}
		return fmt.Errorf("%s under %s is the pre-v0.20 shape; rewrite it as %s (%s)", kind, apiVersion, APIVersion, hint)
	}
	return fmt.Errorf("unsupported apiVersion %q (want %q)", apiVersion, APIVersion)
}
