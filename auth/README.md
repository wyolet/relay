# auth

`github.com/wyolet/relay/auth` holds general-purpose authentication and authorization primitives. Kinds, verbs and owner kinds are opaque strings that the product passes in, so the module carries no relay vocabulary. Relay builds its users, teams, projects, RBAC, SSO, SCIM and inference tokens on it.

```bash
go get github.com/wyolet/relay/auth@latest
```

| Package | Contents |
|---|---|
| `authn` | an ordered chain of authenticators and HTTP middleware that puts the authenticated principal in the request context |
| `principal` | the authenticated caller: who it is, which credential proved it, and what that credential is limited to |
| `rbac`, `authz` | a default-deny role-based authorization engine with scopes, bindings and product rules; the shared resource and refusal types |
| `directory` | users, external identities, groups, teams, projects, service accounts, roles and role bindings, with grant checks on writes (`memstore` for tests, `storetest` for conformance) |
| `store/postgres` | the Postgres implementation of the directory stores, with embedded migrations |
| `manifest` | YAML documents (`auth.wyolet.dev/v1alpha1`) for directory rows: parse, plan, apply, prune, JSON Schema |
| `oidc` | an OpenID Connect relying party for browser login, with PKCE |
| `scim` | a SCIM 2.0 service provider over the directory |
| `session` | cookie sessions over `alexedwards/scs/v2`, with token rotation on login |
| `token` | Ed25519-signed JWTs with `kid` key rotation and a JWK Set |
| `apikey` | opaque bearer keys and the sha256 hash a server stores in place of them |
| `password` | bcrypt hashing and verification, and detection of published placeholder passwords |

The module is v0: its API may change between minor versions. Releases are tagged `auth/vX.Y.Z`. It was built for relay, and it imports nothing from the relay server module.

API reference: [pkg.go.dev](https://pkg.go.dev/github.com/wyolet/relay/auth). License: [Apache-2.0](https://github.com/wyolet/relay/blob/main/LICENSE).
