package manifest

// removedFields are spec fields v1alpha3 dropped from relay's own kinds, with
// what replaces each; the catalog parser refuses a document carrying one.
// Policy's are registered with the catalog kinds.
var removedFields = map[string]map[string]string{
	"PolicyBinding": {
		"project":  "metadata.owner",
		"subjects": "accounts (account names)",
	},
	"HostKey":        {"policyId": "account, plus a PolicyBinding of the account's tier policy"},
	"Team":           {"budget": "a Budget linked by the policy bound to the team"},
	"Project":        {"budget": "a Budget linked by the policy bound to the project"},
	"Key":            {"policy": "a PolicyBinding on the account the key runs in"},
	"ServiceAccount": {"policy": "a PolicyBinding on the service account's account"},
}
