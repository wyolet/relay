// Package stored is relay's "stored" secret backend over Postgres: values in secret_values, sealed under per-scope data keys from the secret module's key ring (data keys in secret_data_keys, target KEK in secret_keyring), the rewriter that moves stale rows under their scope's current key, and the operator status and downgrade paths.
//
// Out of scope: choosing a host key's scope (the host key store passes it, from the owner), the resolver registry (app/secret) and the request path, which only reads values already resolved into the catalog snapshot.
package stored
