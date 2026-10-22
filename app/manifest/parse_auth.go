package manifest

import (
	"gopkg.in/yaml.v3"

	authmanifest "github.com/wyolet/relay/auth/manifest"
)

// authKind reports a document the auth module decodes, strictly: one of its kinds under its own apiVersion. The same kinds under relay's apiVersion keep relay's lenient decode (decodeRelayAuthKind), and a User under any other apiVersion stays the identity loader's (see isForeignKind).
func authKind(apiVersion, kind string) bool {
	return apiVersion == authmanifest.APIVersion && authmanifest.IsKind(kind)
}

func parseAuthDoc(index int, node *yaml.Node) (Document, error) {
	d, err := AuthManifests.Decode(index, node)
	if err != nil {
		return Document{}, err
	}
	doc, err := fromAuthDoc(&d)
	if err != nil {
		return Document{}, &authmanifest.Error{Doc: index, Kind: d.Kind, Name: d.Metadata.Name, Err: err}
	}
	return doc, nil
}

// decodeRelayAuthKind decodes the auth module's kinds written under relay's apiVersion as relay always has: stray fields are ignored and metadata.owner is accepted. ok is false for any other kind.
func decodeRelayAuthKind(env *rawEnvelope) (doc Document, ok bool, err error) {
	switch env.Kind {
	case "Team":
		var spec TeamSpec
		if err = refuseOnExceed(&env.Spec); err == nil {
			err = env.Spec.Decode(&spec)
		}
		doc = Document{Team: &TeamDTO{APIVersion: env.APIVersion, Kind: env.Kind, Metadata: env.Metadata, Spec: spec}}
	case "Project":
		var spec ProjectSpec
		if err = refuseOnExceed(&env.Spec); err == nil {
			err = env.Spec.Decode(&spec)
		}
		doc = Document{Project: &ProjectDTO{APIVersion: env.APIVersion, Kind: env.Kind, Metadata: env.Metadata, Spec: spec}}
	case "ServiceAccount":
		var spec ServiceAccountSpec
		err = env.Spec.Decode(&spec)
		doc = Document{ServiceAccount: &ServiceAccountDTO{APIVersion: env.APIVersion, Kind: env.Kind, Metadata: env.Metadata, Spec: spec}}
	case "Group":
		var spec GroupSpec
		err = env.Spec.Decode(&spec)
		doc = Document{Group: &GroupDTO{APIVersion: env.APIVersion, Kind: env.Kind, Metadata: env.Metadata, Spec: spec}}
	case "Role":
		var spec RoleSpec
		err = env.Spec.Decode(&spec)
		doc = Document{Role: &RoleDTO{APIVersion: env.APIVersion, Kind: env.Kind, Metadata: env.Metadata, Spec: spec}}
	case "RoleBinding":
		var spec RoleBindingSpec
		err = env.Spec.Decode(&spec)
		doc = Document{RoleBinding: &RoleBindingDTO{APIVersion: env.APIVersion, Kind: env.Kind, Metadata: env.Metadata, Spec: spec}}
	default:
		return Document{}, false, nil
	}
	if err != nil {
		return Document{}, true, err
	}
	return doc, true, nil
}

// refuseOnExceed fails a spec that sets budget.onExceed, which the lenient decode would otherwise drop unnoticed.
func refuseOnExceed(spec *yaml.Node) error {
	var probe struct {
		Budget map[string]any `yaml:"budget"`
	}
	// A spec that fails to decode is reported by the decode that follows.
	_ = spec.Decode(&probe)
	if _, ok := probe.Budget["onExceed"]; ok {
		return errOnExceed
	}
	return nil
}
