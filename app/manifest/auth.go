// The tenancy, RBAC and account kinds are the auth module's manifests. Relay registers its rule vocabulary with the module, and keeps its wire DTOs and translate functions as thin adapters over the module's.

package manifest

import (
	"errors"
	"fmt"
	"strings"

	"github.com/wyolet/relay/app/role"
	authmanifest "github.com/wyolet/relay/auth/manifest"
)

// AuthManifests is relay's registration with the auth module's manifests.
var AuthManifests = authmanifest.MustRegistry(authmanifest.Config{
	RuleKinds: role.Kinds,
	RuleVerbs: role.Verbs,
})

// authRefs presents relay's Resolver to the module, by document kind.
type authRefs struct{ r Resolver }

func (a authRefs) ID(kind, name string) (string, bool) {
	switch kind {
	case authmanifest.KindTeam:
		return a.r.TeamID(name)
	case authmanifest.KindProject:
		return a.r.ProjectID(name)
	case authmanifest.KindServiceAccount:
		return a.r.ServiceAccountID(name)
	case authmanifest.KindRole:
		return a.r.RoleID(name)
	case authmanifest.KindGroup:
		return a.r.GroupID(name)
	case authmanifest.KindUser:
		return a.r.UserID(name)
	case "Policy":
		return a.r.PolicyID(name)
	}
	return "", false
}

// authNames presents relay's ReverseResolver to the module.
type authNames struct{ r ReverseResolver }

func (a authNames) Name(kind, id string) (string, bool) {
	switch kind {
	case authmanifest.KindTeam:
		return a.r.TeamName(id)
	case authmanifest.KindProject:
		return a.r.ProjectName(id)
	case authmanifest.KindServiceAccount:
		return a.r.ServiceAccountName(id)
	case authmanifest.KindRole:
		return a.r.RoleName(id)
	case authmanifest.KindGroup:
		return a.r.GroupName(id)
	case authmanifest.KindUser:
		return a.r.Username(id)
	case "Policy":
		return a.r.PolicyName(id)
	}
	return "", false
}

// fromAuthError words a module translate error the way relay always has ("project \"x\": team \"y\" not found"), a name that resolves to nothing staying a RefNotFoundError.
func fromAuthError(err error) error {
	var de *authmanifest.Error
	if !errors.As(err, &de) {
		return err
	}
	prefix := strings.ToLower(de.Kind)
	if strings.HasPrefix(de.Field, "spec.subjects") {
		prefix = "binding"
	}
	var ref *authmanifest.RefError
	if errors.As(err, &ref) {
		return refNotFound("%s %q: %s %q not found", prefix, de.Name, ref.Kind, ref.Name)
	}
	return fmt.Errorf("%s %q: %w", prefix, de.Name, de.Err)
}
