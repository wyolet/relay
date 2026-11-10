package manifest

import (
	"gopkg.in/yaml.v3"

	catalogmanifest "github.com/wyolet/relay/pkg/catalogkind/manifest"
)

// newRelayParser is the catalog parser plus relay's own kinds, the auth module's documents and the foreign kinds.
func newRelayParser() *catalogmanifest.Parser {
	p := catalogmanifest.NewParser()
	p.Intercept(interceptAuthAndForeign)
	p.Register("HostKey", catalogmanifest.KindDecoder{
		Decode: func(env *catalogmanifest.Envelope) (catalogmanifest.Document, error) {
			var spec HostKeySpec
			if err := env.Spec.Decode(&spec); err != nil {
				return catalogmanifest.Document{}, err
			}
			return toParsed(Document{HostKey: &HostKeyDTO{APIVersion: env.APIVersion, Kind: env.Kind, Metadata: env.Metadata, Spec: spec}}), nil
		},
		RemovedFields: removedFields["HostKey"],
		ReshapedHint:  "policyId is gone: name the provider account in spec.account and bind its tier policy with a PolicyBinding",
	})
	p.Register("Account", catalogmanifest.KindDecoder{Decode: func(env *catalogmanifest.Envelope) (catalogmanifest.Document, error) {
		var spec AccountSpec
		if err := env.Spec.Decode(&spec); err != nil {
			return catalogmanifest.Document{}, err
		}
		return toParsed(Document{Account: &AccountDTO{APIVersion: env.APIVersion, Kind: env.Kind, Metadata: env.Metadata, Spec: spec}}), nil
	}})
	p.Register("Pool", catalogmanifest.KindDecoder{Decode: func(env *catalogmanifest.Envelope) (catalogmanifest.Document, error) {
		var spec PoolSpec
		if err := env.Spec.Decode(&spec); err != nil {
			return catalogmanifest.Document{}, err
		}
		return toParsed(Document{Pool: &PoolDTO{APIVersion: env.APIVersion, Kind: env.Kind, Metadata: env.Metadata, Spec: spec}}), nil
	}})
	p.Register("Budget", catalogmanifest.KindDecoder{Decode: func(env *catalogmanifest.Envelope) (catalogmanifest.Document, error) {
		var spec BudgetSpec
		if err := env.Spec.Decode(&spec); err != nil {
			return catalogmanifest.Document{}, err
		}
		return toParsed(Document{Budget: &BudgetDTO{APIVersion: env.APIVersion, Kind: env.Kind, Metadata: env.Metadata, Spec: spec}}), nil
	}})
	p.Register("Key", catalogmanifest.KindDecoder{
		Decode: func(env *catalogmanifest.Envelope) (catalogmanifest.Document, error) {
			var spec KeySpec
			if err := env.Spec.Decode(&spec); err != nil {
				return catalogmanifest.Document{}, err
			}
			return toParsed(Document{Key: &KeyDTO{APIVersion: env.APIVersion, Kind: env.Kind, Metadata: env.Metadata, Spec: spec}}), nil
		},
		RemovedFields: removedFields["Key"],
		ReshapedHint:  "policy is gone: bind the key's principal with a PolicyBinding",
	})
	p.Register("PolicyBinding", catalogmanifest.KindDecoder{
		Decode: func(env *catalogmanifest.Envelope) (catalogmanifest.Document, error) {
			var spec PolicyBindingSpec
			if err := env.Spec.Decode(&spec); err != nil {
				return catalogmanifest.Document{}, err
			}
			return toParsed(Document{PolicyBinding: &PolicyBindingDTO{APIVersion: env.APIVersion, Kind: env.Kind, Metadata: env.Metadata, Spec: spec}}), nil
		},
		RemovedFields: removedFields["PolicyBinding"],
		ReshapedHint:  "a binding governs only requests inside its owner's scope and binds a subject once; re-check owners and subjects",
	})
	p.Register("Overlay", catalogmanifest.KindDecoder{Decode: func(env *catalogmanifest.Envelope) (catalogmanifest.Document, error) {
		var spec OverlaySpec
		if err := env.Spec.Decode(&spec); err != nil {
			return catalogmanifest.Document{}, err
		}
		return toParsed(Document{Overlay: &OverlayDTO{APIVersion: env.APIVersion, Kind: env.Kind, Metadata: env.Metadata, Spec: spec}}), nil
	}})
	// Spec stays a raw node — its shape is per-section and is validated
	// when the settings store decodes it against the named section.
	p.Register("Setting", catalogmanifest.KindDecoder{Decode: func(env *catalogmanifest.Envelope) (catalogmanifest.Document, error) {
		return toParsed(Document{Setting: &SettingDTO{APIVersion: env.APIVersion, Kind: env.Kind, Metadata: env.Metadata, Spec: env.Spec}}), nil
	}})
	registerRelayAuthKinds(p)
	return p
}

// interceptAuthAndForeign takes the auth module's documents and the foreign kinds before the apiVersion check: each carries its own apiVersion.
func interceptAuthAndForeign(index int, env *catalogmanifest.Envelope, node *yaml.Node) (catalogmanifest.Document, bool, error) {
	if authKind(env.APIVersion, env.Kind) {
		doc, err := parseAuthDoc(index, node)
		if err != nil {
			return catalogmanifest.Document{}, true, err
		}
		return toParsed(doc), true, nil
	}
	// Identity (User) lives in config/users/ alongside catalog YAML and
	// carries its own apiVersion — the catalog seeder must walk past it
	// without enforcing the manifest schema version.
	if isForeignKind(env.Kind) {
		return catalogmanifest.Document{Foreign: env.Kind}, true, nil
	}
	return catalogmanifest.Document{}, false, nil
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
