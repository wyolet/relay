# secret

`github.com/wyolet/relay/secret` resolves secret references and encrypts secrets at rest. It depends only on the Go standard library and `golang.org/x/crypto`; the cloud clients use no vendor SDK.

```bash
go get github.com/wyolet/relay/secret@latest
```

| Package | Contents |
|---|---|
| `secret` | `Ref`, a JSON-serializable pointer to where a credential lives, plus a `Registry` that dispatches each `Ref` to the `Resolver` for its kind; `EnvResolver` reads the process environment |
| `aead` | a self-describing sealed format: XAES-256-GCM with a random 192-bit nonce, the sealing key's id in the header and caller-bound associated data |
| `kek`, `kek/local` | key-encryption keys that wrap data keys; `local` holds a 32-byte key in process memory |
| `keyring` | per-scope data keys wrapped by KEKs and kept in a store you supply. Each scope has one primary key for encryption; older keys stay decrypt-only until destroyed, and values sealed under them are reported as stale |
| `backend/aws`, `backend/gcp`, `backend/azure`, `backend/bitwarden` | fetch-only resolvers for AWS Secrets Manager, GCP Secret Manager, Azure Key Vault and Bitwarden or Vaultwarden. Plaintext is held in memory and never written back |
| `cloud/aws`, `cloud/gcp`, `cloud/azure` | credentials and request signing for those clouds (SigV4, service-account and metadata tokens, Entra ID client credentials and managed identity) |

Relay uses `aead`, `kek` and `keyring` to seal stored host keys under per-owner data keys wrapped by `RELAY_MASTER_KEY`. Relay host keys reference an env var or a stored value; the external backends are not configurable for host keys.

The module is v0: its API may change between minor versions. Releases are tagged `secret/vX.Y.Z`. It was built for relay, and it imports nothing from the relay server module.

API reference: [pkg.go.dev](https://pkg.go.dev/github.com/wyolet/relay/secret). License: [Apache-2.0](https://github.com/wyolet/relay/blob/main/LICENSE).
