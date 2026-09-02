package catalog

import (
	"sort"

	"github.com/wyolet/relay/app/key"
)

// System group subjects every data-plane principal carries. They are not
// Group rows — an operator binds a role to them without maintaining
// membership.
const (
	SubjectAuthenticated   = "group:system:authenticated"
	SubjectServiceAccounts = "group:system:serviceaccounts"
)

// keySubjects renders the subject list a key's principal acts under:
// identity, then the groups it belongs to, then the catch-all. Computed at
// snapshot build so the request path only copies a slice header.
func keySubjects(s *Snapshot, k *key.Key) []string {
	switch k.Spec.Principal.Kind {
	case key.PrincipalServiceAccount:
		subs := []string{"serviceaccount:" + k.Spec.Principal.ID, SubjectServiceAccounts}
		if sa, ok := s.serviceAccountsByID[k.Spec.Principal.ID]; ok {
			if p, ok := s.projectsByID[sa.Spec.ProjectID]; ok {
				subs = append(subs, SubjectServiceAccounts+":"+p.Meta.Name)
			}
		}
		return append(subs, SubjectAuthenticated)
	case key.PrincipalUser:
		return UserSubjects(k.Spec.Principal.ID, s.groupsByUser[k.Spec.Principal.ID], nil)
	}
	return []string{SubjectAuthenticated}
}

// UserSubjects renders a user's subjects from their local group names and
// any groups an identity provider asserted. The union is deduplicated and
// sorted so two callers with the same membership produce the same list.
// Tokens build theirs per request (a token's claims are its own source);
// keys read the precomputed list off the snapshot.
func UserSubjects(userID string, localGroups, idpGroups []string) []string {
	seen := make(map[string]struct{}, len(localGroups)+len(idpGroups))
	names := make([]string, 0, len(localGroups)+len(idpGroups))
	for _, g := range localGroups {
		if _, dup := seen[g]; dup || g == "" {
			continue
		}
		seen[g] = struct{}{}
		names = append(names, g)
	}
	for _, g := range idpGroups {
		if _, dup := seen[g]; dup || g == "" {
			continue
		}
		seen[g] = struct{}{}
		names = append(names, g)
	}
	sort.Strings(names)

	subs := make([]string, 0, len(names)+2)
	subs = append(subs, "user:"+userID)
	for _, g := range names {
		subs = append(subs, "group:"+g)
	}
	return append(subs, SubjectAuthenticated)
}

// reindexKeySubjects recomputes subjectsByKey for every key whose principal
// matches kind+id. Keys are few and the walk only runs on a group, service
// account or project write, never on the request path.
func reindexKeySubjects(s *Snapshot, kind key.PrincipalKind, id string) {
	for _, k := range s.keysByID {
		if k.Spec.Principal.Kind != kind || k.Spec.Principal.ID != id {
			continue
		}
		s.subjectsByKey[k.Meta.ID] = keySubjects(s, k)
	}
}

// reindexMemberSubjects recomputes the subjects of every key whose
// principal is one of these users.
func reindexMemberSubjects(s *Snapshot, userIDs []string) {
	for _, uid := range userIDs {
		reindexKeySubjects(s, key.PrincipalUser, uid)
	}
}

// reindexAllKeySubjects recomputes every key's subjects. Used when a write
// can change many keys' project slug at once (project rename, service
// account moved between projects).
func reindexAllKeySubjects(s *Snapshot) {
	for _, k := range s.keysByID {
		s.subjectsByKey[k.Meta.ID] = keySubjects(s, k)
	}
}
