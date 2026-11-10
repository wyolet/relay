// Wording for scope refusals: rows and scopes are named the way a person picks them (display names, "your personal pool"), never by id or owner kind, so a refusal reads as what to change.

package refcheck

import (
	"context"
	"fmt"
	"strings"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/pkg/meta"
)

// kindWord renders an API kind as words: "host-key" is "host key".
func kindWord(kind string) string { return strings.ReplaceAll(kind, "-", " ") }

// kindWords is the plural of kindWord.
func kindWords(kind string) string {
	w := kindWord(kind)
	if strings.HasSuffix(w, "y") {
		return strings.TrimSuffix(w, "y") + "ies"
	}
	return w + "s"
}

// label is a row's display name, or its name when it has none.
func label(m meta.Metadata) string {
	if m.DisplayName != "" {
		return m.DisplayName
	}
	return m.Name
}

// rowText names a row with its owner: `your personal pool "anthropic"`, `pool "anthropic" of team "Platform"`, `system pool "anthropic"`. A row the caller may not see is named by kind alone.
func (c Checker) rowText(ctx context.Context, kind string, m meta.Metadata) string {
	if !Visible(ctx, c.Authz, kind, m.ID, m.Owner) {
		return hiddenRowText(kind)
	}
	name := fmt.Sprintf("%s %q", kindWord(kind), label(m))
	switch m.Owner.Kind {
	case meta.OwnerSystem:
		return "system " + name
	case meta.OwnerUser:
		return c.userPossessive(ctx, m.Owner.ID) + " personal " + name
	}
	return name + " of " + c.scopeText(ctx, m.Owner)
}

// hiddenRowText names a row the caller may not see without confirming anything about it.
func hiddenRowText(kind string) string {
	w := kindWord(kind)
	if strings.ContainsRune("aeiou", rune(w[0])) {
		return "an " + w + " you cannot see"
	}
	return "a " + w + " you cannot see"
}

// scopeText names a scope: `team "Platform"`, `project "Web"`, "your personal scope", "the system".
func (c Checker) scopeText(ctx context.Context, o meta.Owner) string {
	switch o.Kind {
	case meta.OwnerSystem, "":
		return "the system"
	case meta.OwnerUser:
		return c.userPossessive(ctx, o.ID) + " personal scope"
	case meta.OwnerTeam:
		if c.Rows.Team != nil {
			if t := c.Rows.Team(ctx, o.ID); t != nil && Visible(ctx, c.Authz, "team", t.Meta.ID, t.Meta.Owner) {
				return fmt.Sprintf("team %q", label(t.Meta))
			}
		}
		return "another team"
	case meta.OwnerProject:
		if c.Rows.Project != nil {
			if p := c.Rows.Project(ctx, o.ID); p != nil && Visible(ctx, c.Authz, "project", p.Meta.ID, p.Meta.Owner) {
				return fmt.Sprintf("project %q", label(p.Meta))
			}
		}
		return "another project"
	case meta.OwnerHost:
		if c.Rows.Host != nil {
			if h := c.Rows.Host(ctx, o.ID); h != nil && Visible(ctx, c.Authz, "host", h.Meta.ID, h.Meta.Owner) {
				return fmt.Sprintf("host %q", label(h.Meta))
			}
		}
		return "a host"
	}
	return "a " + string(o.Kind)
}

// userPossessive is "your" for the caller, the user's name otherwise ("alice's"), or "another user's" when no name is known.
func (c Checker) userPossessive(ctx context.Context, userID string) string {
	if a := actor.From(ctx); a != nil && a.UserID != "" && a.UserID == userID {
		return "your"
	}
	if name := c.userName(ctx, userID); name != "" {
		return name + "'s"
	}
	return "another user's"
}

// userSubject is "you" for the caller, the user's name otherwise, or "its owner" when no name is known.
func (c Checker) userSubject(ctx context.Context, userID string) string {
	if a := actor.From(ctx); a != nil && a.UserID != "" && a.UserID == userID {
		return "you"
	}
	if name := c.userName(ctx, userID); name != "" {
		return name
	}
	return "its owner"
}

// userName is another user's username, only for a caller who may list users (an admin, or anyone without a scoping authorizer): a scoped caller does not learn who else is on the relay.
func (c Checker) userName(ctx context.Context, userID string) string {
	if c.Rows.UserName == nil || userID == "" || RequireAdmin(ctx, c.Authz, "") != nil {
		return ""
	}
	return c.Rows.UserName(ctx, userID)
}

// reachText says which scopes' rows a row owned by ref may draw on: "the project, its team or the system".
func reachText(ref meta.Owner) string {
	switch ref.Kind {
	case meta.OwnerProject:
		return "the project, its team or the system"
	case meta.OwnerTeam:
		return "the team or the system"
	case meta.OwnerUser:
		return "the same user or the system"
	}
	return "the system"
}

// homeText names where a row usable from ref can be created: "the project or its team".
func homeText(ref meta.Owner) string {
	switch ref.Kind {
	case meta.OwnerProject:
		return "the project or its team"
	case meta.OwnerTeam:
		return "the team"
	case meta.OwnerUser:
		return "the same personal scope"
	}
	return "the system"
}

// scopeArticle renders a scope kind as the noun a reach sentence opens with: "a project's", "a team's", "a personal", "a system".
func scopeArticle(ref meta.Owner) string {
	switch ref.Kind {
	case meta.OwnerProject:
		return "a project's"
	case meta.OwnerTeam:
		return "a team's"
	case meta.OwnerUser:
		return "a personal"
	}
	return "a system"
}
