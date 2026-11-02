package manifest

import "fmt"

func dispatchKind(env *rawEnvelope) (Document, error) {
	if doc, ok, err := decodeRelayAuthKind(env); ok {
		return doc, err
	}
	switch env.Kind {
	case "Provider":
		var spec ProviderSpec
		if err := env.Spec.Decode(&spec); err != nil {
			return Document{}, err
		}
		return Document{Provider: &ProviderDTO{APIVersion: env.APIVersion, Kind: env.Kind, Metadata: env.Metadata, Spec: spec}}, nil

	case "Host":
		var spec HostSpec
		if err := env.Spec.Decode(&spec); err != nil {
			return Document{}, err
		}
		return Document{Host: &HostDTO{APIVersion: env.APIVersion, Kind: env.Kind, Metadata: env.Metadata, Spec: spec}}, nil

	case "Model":
		var spec ModelSpec
		if err := env.Spec.Decode(&spec); err != nil {
			return Document{}, err
		}
		return Document{Model: &ModelDTO{APIVersion: env.APIVersion, Kind: env.Kind, Metadata: env.Metadata, Spec: spec}}, nil

	case "HostKey":
		var spec HostKeySpec
		if err := decodeSpec(env, &spec); err != nil {
			return Document{}, err
		}
		return Document{HostKey: &HostKeyDTO{APIVersion: env.APIVersion, Kind: env.Kind, Metadata: env.Metadata, Spec: spec}}, nil

	case "Policy":
		var spec PolicySpec
		if err := decodeSpec(env, &spec); err != nil {
			return Document{}, err
		}
		return Document{Policy: &PolicyDTO{APIVersion: env.APIVersion, Kind: env.Kind, Metadata: env.Metadata, Spec: spec}}, nil

	case "Account":
		var spec AccountSpec
		if err := env.Spec.Decode(&spec); err != nil {
			return Document{}, err
		}
		return Document{Account: &AccountDTO{APIVersion: env.APIVersion, Kind: env.Kind, Metadata: env.Metadata, Spec: spec}}, nil

	case "Pool":
		var spec PoolSpec
		if err := env.Spec.Decode(&spec); err != nil {
			return Document{}, err
		}
		return Document{Pool: &PoolDTO{APIVersion: env.APIVersion, Kind: env.Kind, Metadata: env.Metadata, Spec: spec}}, nil

	case "Budget":
		var spec BudgetSpec
		if err := env.Spec.Decode(&spec); err != nil {
			return Document{}, err
		}
		return Document{Budget: &BudgetDTO{APIVersion: env.APIVersion, Kind: env.Kind, Metadata: env.Metadata, Spec: spec}}, nil

	case "RateLimit":
		var spec RateLimitSpec
		if err := env.Spec.Decode(&spec); err != nil {
			return Document{}, err
		}
		return Document{RateLimit: &RateLimitDTO{APIVersion: env.APIVersion, Kind: env.Kind, Metadata: env.Metadata, Spec: spec}}, nil

	case "Key":
		var spec KeySpec
		if err := decodeSpec(env, &spec); err != nil {
			return Document{}, err
		}
		return Document{Key: &KeyDTO{APIVersion: env.APIVersion, Kind: env.Kind, Metadata: env.Metadata, Spec: spec}}, nil

	case "Pricing":
		var spec PricingSpec
		if err := env.Spec.Decode(&spec); err != nil {
			return Document{}, err
		}
		return Document{Pricing: &PricingDTO{APIVersion: env.APIVersion, Kind: env.Kind, Metadata: env.Metadata, Spec: spec}}, nil

	case "HostBinding":
		var spec HostBindingSpec
		if err := env.Spec.Decode(&spec); err != nil {
			return Document{}, err
		}
		return Document{HostBinding: &HostBindingDTO{APIVersion: env.APIVersion, Kind: env.Kind, Metadata: env.Metadata, Spec: spec}}, nil

	case "PolicyBinding":
		var spec PolicyBindingSpec
		if err := decodeSpec(env, &spec); err != nil {
			return Document{}, err
		}
		return Document{PolicyBinding: &PolicyBindingDTO{APIVersion: env.APIVersion, Kind: env.Kind, Metadata: env.Metadata, Spec: spec}}, nil

	case "Overlay":
		var spec OverlaySpec
		if err := env.Spec.Decode(&spec); err != nil {
			return Document{}, err
		}
		return Document{Overlay: &OverlayDTO{APIVersion: env.APIVersion, Kind: env.Kind, Metadata: env.Metadata, Spec: spec}}, nil

	case "Setting":
		// Spec stays a raw node — its shape is per-section and is validated
		// when the settings store decodes it against the named section.
		return Document{Setting: &SettingDTO{APIVersion: env.APIVersion, Kind: env.Kind, Metadata: env.Metadata, Spec: env.Spec}}, nil

	default:
		return Document{}, fmt.Errorf("unknown kind %q", env.Kind)
	}
}

// isForeignKind reports kinds owned by sibling subsystems that the catalog
// manifest loader should pass through silently. Keep this list narrow —
// silent skips can mask typos in catalog YAML.
func isForeignKind(kind string) bool {
	switch kind {
	case "User":
		return true
	}
	return false
}
