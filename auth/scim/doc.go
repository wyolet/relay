// Package scim is a SCIM 2.0 service provider (RFC 7643, RFC 7644) over a directory.Directory: a plain http.Handler serving /Users, /Groups, /ServiceProviderConfig, /ResourceTypes and /Schemas, so an identity provider such as Microsoft Entra ID or Okta can create, update, deactivate and delete the accounts and groups it manages.
//
// # Trust model
//
// A SCIM client is a provisioning source. It authenticates with a bearer token of its own (TokenPrefix followed by random bytes, stored only as its apikey hash in a TokenStore), and its name is the source every row it creates is stamped with (external_source). All of its work goes through a directory.Provisioner for that source, which:
//
//   - reads, changes and deletes only the users and groups stamped with its source, so it never reaches a locally created row or another client's;
//   - never sets roles, passwords or sign-in identities, and has no operation on roles, bindings, teams, projects or service accounts;
//   - skips the grant check on membership of its own groups: the identity provider decides who is in the groups it manages, and the role bindings an operator makes on those groups decide what membership grants;
//   - names a new group so that no stored group holds the name and no binding names it, so a new group inherits no grant made for another.
//
// Deactivating an account (active=false) disables it through the directory, which ends its sessions and kills its tokens; DELETE deletes it, or disables it with Config.DeleteDisables.
//
// # What the store holds
//
// A user keeps userName, one email (the primary one, else the first), active and externalId; groups is read-only. A group keeps displayName, externalId and members. Every other core attribute (name, displayName and nickName of a user, phoneNumbers, addresses, roles, password, …) and every extension attribute is accepted on write and not stored, so a client's default attribute mapping works unchanged. A group's name, which bindings refer to, is derived from its first displayName and does not follow later renames.
//
// Filters support eq, ne, co, sw, ew, gt, ge, lt, le, pr, and, or, not, grouping and value paths, over id, userName, externalId and emails (value, type, primary) for users, and id, displayName, externalId and members.value for groups; anything else is invalidFilter. PATCH supports add, replace and remove with attribute paths, value paths with an optional sub-attribute, and no path at all, with case-insensitive operation names and boolean strings for active.
//
// Out of scope: bulk operations, sorting, ETags, /Me, POST .search, password changes, and the admin API that creates and revokes clients, which is the product's (CreateClient and a TokenStore are its building blocks).
package scim
