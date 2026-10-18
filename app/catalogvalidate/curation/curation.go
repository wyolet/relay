// Package curation holds a catalog repo's data conventions, layered on
// catalogvalidate.ValidateGraph: the named Rules, the families.yaml family
// and product-line checks, and the featured.yaml index check. These are
// curation choices for a published catalog, not schema invariants, so the
// seed and the control plane never run them.
//
// Rule names are stable kebab-case ids, used by --skip and in reports;
// don't rename one.
package curation

import "github.com/wyolet/relay/app/catalogvalidate"

// Rules is the curation rule set run through catalogvalidate.RunRules.
var Rules = []catalogvalidate.Rule{
	hostHasTierPolicy,
	pricingTargetMustBindToHost,
	providerIconSet,
}
