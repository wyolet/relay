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

// principalKey is the keysByPrincipal index key. Kind is part of it so a
// service account and a user can never collide on an id.
func principalKey(kind key.PrincipalKind, id string) string {
	if id == "" {
		return ""
	}
	return string(kind) + ":" + id
}

// indexKeyPrincipal / deindexKeyPrincipal keep keysByPrincipal in step with
// keysByID.
func indexKeyPrincipal(s *Snapshot, k *key.Key) {
	pk := principalKey(k.Spec.Principal.Kind, k.Spec.Principal.ID)
	if pk == "" {
		return
	}
	// A fresh slice, never append-in-place: clones share these headers, and
	// appending into spare capacity would write through to a published
	// snapshot.
	old := s.keysByPrincipal[pk]
	next := make([]*key.Key, len(old), len(old)+1)
	copy(next, old)
	s.keysByPrincipal[pk] = append(next, k)
}

func deindexKeyPrincipal(s *Snapshot, k *key.Key) {
	pk := principalKey(k.Spec.Principal.Kind, k.Spec.Principal.ID)
	if pk == "" {
		return
	}
	list := s.keysByPrincipal[pk]
	for i, have := range list {
		if have.Meta.ID != k.Meta.ID {
			continue
		}
		out := make([]*key.Key, 0, len(list)-1)
		out = append(out, list[:i]...)
		out = append(out, list[i+1:]...)
		if len(out) == 0 {
			delete(s.keysByPrincipal, pk)
			return
		}
		s.keysByPrincipal[pk] = out
		return
	}
}

// reindexKeySubjects recomputes subjectsByKey for every key whose principal
// matches kind+id. Indexed by principal so a group write costs its own
// members' keys, not a walk of every key in the deployment.
func reindexKeySubjects(s *Snapshot, kind key.PrincipalKind, id string) {
	for _, k := range s.keysByPrincipal[principalKey(kind, id)] {
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
