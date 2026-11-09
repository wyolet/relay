// Package wire handles conversion between the operator-facing YAML/JSON wire
// format and the domain structs in the app/* entity packages.
//
// Wire format uses entity *names* (DNS-1123 slugs) for cross-references.
// Domain structs use *ids* (UUIDv7). This package bridges the gap.
//
// Typical flow:
//
//  1. Parse raw YAML via Parse or LoadDir → []Document
//  2. Build a Resolver from your name→id index (snapshot, seed index, etc.)
//  3. Call ToXxx(dto, resolver) → domain struct ready for Store.Upsert
//
// Reverse direction (GET responses):
//
//  1. Receive domain struct from store
//  2. Build a ReverseResolver from your id→name index
//  3. Call FromXxx(domain, rev) → wire DTO suitable for JSON/YAML output

// Package manifest converts between the catalog's YAML/JSON wire format, which references entities by name, and the app/* domain structs, which reference them by id.
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

// reshapedKinds are the kinds whose v1alpha2 shape v1alpha3 no longer reads,
// each with the hint the refusal carries.
var reshapedKinds = map[string]string{
	"Policy":        "hostKeys, rateLimit, rlBindings and keySelection are gone: link pools or credentials, rateLimits: [{ref, models}] and budgets",
	"HostKey":       "policyId is gone: name the provider account in spec.account and bind its tier policy with a PolicyBinding",
	"Key":           "policy is gone: bind the key's principal with a PolicyBinding",
	"PolicyBinding": "a binding governs only requests inside its owner's scope and binds a subject once; re-check owners and subjects",
}

// checkAPIVersion accepts APIVersion for every kind and PreviousAPIVersion
// for the kinds v1alpha3 left unchanged.
func checkAPIVersion(apiVersion, kind string) error {
	switch apiVersion {
	case APIVersion:
		return nil
	case PreviousAPIVersion:
		hint, reshaped := reshapedKinds[kind]
		if !reshaped {
			return nil
		}
		return fmt.Errorf("%s under %s is the pre-v0.20 shape; rewrite it as %s (%s)", kind, apiVersion, APIVersion, hint)
	}
	return fmt.Errorf("unsupported apiVersion %q (want %q)", apiVersion, APIVersion)
}
