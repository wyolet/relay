package manifest

import (
	"gopkg.in/yaml.v3"

	authmanifest "github.com/wyolet/relay/auth/manifest"
	catalogmanifest "github.com/wyolet/relay/pkg/catalogkind/manifest"
)

// authKind reports a document the auth module decodes, strictly: one of its kinds under its own apiVersion. The same kinds under relay's apiVersion keep relay's lenient decode (registerRelayAuthKinds), and a User under any other apiVersion stays the identity loader's (see isForeignKind).
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

// registerRelayAuthKinds registers the auth module's kinds written under relay's apiVersion, decoded as relay always has: stray fields are ignored and metadata.owner is accepted.
func registerRelayAuthKinds(p *catalogmanifest.Parser) {
	p.Register("Team", catalogmanifest.KindDecoder{
		Decode: func(env *catalogmanifest.Envelope) (catalogmanifest.Document, error) {
			var spec TeamSpec
			if err := env.Spec.Decode(&spec); err != nil {
				return catalogmanifest.Document{}, err
			}
			return toParsed(Document{Team: &TeamDTO{APIVersion: env.APIVersion, Kind: env.Kind, Metadata: env.Metadata, Spec: spec}}), nil
		},
		RemovedFields: removedFields["Team"],
	})
	p.Register("Project", catalogmanifest.KindDecoder{
		Decode: func(env *catalogmanifest.Envelope) (catalogmanifest.Document, error) {
			var spec ProjectSpec
			if err := env.Spec.Decode(&spec); err != nil {
				return catalogmanifest.Document{}, err
			}
			return toParsed(Document{Project: &ProjectDTO{APIVersion: env.APIVersion, Kind: env.Kind, Metadata: env.Metadata, Spec: spec}}), nil
		},
		RemovedFields: removedFields["Project"],
	})
	p.Register("ServiceAccount", catalogmanifest.KindDecoder{
		Decode: func(env *catalogmanifest.Envelope) (catalogmanifest.Document, error) {
			var spec ServiceAccountSpec
			if err := env.Spec.Decode(&spec); err != nil {
				return catalogmanifest.Document{}, err
			}
			return toParsed(Document{ServiceAccount: &ServiceAccountDTO{APIVersion: env.APIVersion, Kind: env.Kind, Metadata: env.Metadata, Spec: spec}}), nil
		},
		RemovedFields: removedFields["ServiceAccount"],
	})
	p.Register("Group", catalogmanifest.KindDecoder{Decode: func(env *catalogmanifest.Envelope) (catalogmanifest.Document, error) {
		var spec GroupSpec
		if err := env.Spec.Decode(&spec); err != nil {
			return catalogmanifest.Document{}, err
		}
		return toParsed(Document{Group: &GroupDTO{APIVersion: env.APIVersion, Kind: env.Kind, Metadata: env.Metadata, Spec: spec}}), nil
	}})
	p.Register("Role", catalogmanifest.KindDecoder{Decode: func(env *catalogmanifest.Envelope) (catalogmanifest.Document, error) {
		var spec RoleSpec
		if err := env.Spec.Decode(&spec); err != nil {
			return catalogmanifest.Document{}, err
		}
		return toParsed(Document{Role: &RoleDTO{APIVersion: env.APIVersion, Kind: env.Kind, Metadata: env.Metadata, Spec: spec}}), nil
	}})
	p.Register("RoleBinding", catalogmanifest.KindDecoder{Decode: func(env *catalogmanifest.Envelope) (catalogmanifest.Document, error) {
		var spec RoleBindingSpec
		if err := env.Spec.Decode(&spec); err != nil {
			return catalogmanifest.Document{}, err
		}
		return toParsed(Document{RoleBinding: &RoleBindingDTO{APIVersion: env.APIVersion, Kind: env.Kind, Metadata: env.Metadata, Spec: spec}}), nil
	}})
}
