package manifest

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// removedFields are spec fields v1alpha3 dropped, per kind, with what
// replaces each. The lenient decode would otherwise ignore them, and a
// dropped restriction (a rate limit, a narrowed key list) widens access.
var removedFields = map[string]map[string]string{
	"Policy": {
		"hostKeys":              "pools",
		"rateLimit":             "rateLimits: [{ref: <name>}]",
		"rlBindings":            "rateLimits: [{ref: <name>, models: [...]}]",
		"keySelection":          "a Pool's selection",
		"modelIds":              "models (model refs)",
		"credentials":           "pools (a Pool of the one host key)",
		"includeDeprecated":     "nothing: models grant deprecated models like any other",
		"payloadLoggingEnabled": "payloadLogging on the accounts the policy is bound to",
	},
	"PolicyBinding": {
		"project":  "metadata.owner",
		"subjects": "accounts (account names)",
	},
	"HostKey":        {"policyId": "account, plus a PolicyBinding of the account's tier policy"},
	"Team":           {"budget": "a Budget linked by the policy bound to the team"},
	"Project":        {"budget": "a Budget linked by the policy bound to the project"},
	"Key":            {"policy": "a PolicyBinding on the key's principal"},
	"ServiceAccount": {"policy": "a PolicyBinding on the service account"},
}

// decodeSpec decodes the spec node of env into dst after refusing any field
// the kind no longer has.
func decodeSpec(env *rawEnvelope, dst any) error {
	if err := refuseRemovedFields(env.Kind, &env.Spec); err != nil {
		return err
	}
	return env.Spec.Decode(dst)
}

func refuseRemovedFields(kind string, spec *yaml.Node) error {
	removed := removedFields[kind]
	if len(removed) == 0 || spec.Kind != yaml.MappingNode {
		return nil
	}
	var found []string
	for i := 0; i+1 < len(spec.Content); i += 2 {
		k := spec.Content[i].Value
		if hint, ok := removed[k]; ok {
			found = append(found, fmt.Sprintf("spec.%s (use %s)", k, hint))
		}
	}
	if len(found) == 0 {
		return nil
	}
	sort.Strings(found)
	return fmt.Errorf("%s no longer has %s", kind, strings.Join(found, ", "))
}
