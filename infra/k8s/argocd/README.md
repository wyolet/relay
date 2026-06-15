# Deploying relay with ArgoCD

ArgoCD syncs the Helm chart at `infra/k8s/relay` from git. It **cannot** create
secrets, so the master key / admin token / DB passwords are minted once,
out-of-band, by a script — and the chart consumes them via `existingSecret`.

## One-time, per environment

```sh
# 1. Mint relay's Secret into the target namespace (idempotent + master-key-safe).
NAMESPACE=relay RELEASE=relay infra/k8s/relay/scripts/gen-secret.sh
#    -> creates secret relay/relay-secrets, prints the admin token once.

# 2. Fill the <…> placeholders in relay-application.yaml:
#    repoURL, targetRevision, image.tag, ingress hosts.

# 3. Register the Application.
kubectl apply -f infra/k8s/argocd/relay-application.yaml
```

ArgoCD then syncs: bundled PostgreSQL / ClickHouse / Valkey come up, the relay
Deployment's init-container waits for them, relay self-migrates PG and seeds the
catalog on first boot, and the app goes healthy once `/healthz` passes.

## Why the secret is out-of-band

- **`RELAY_MASTER_KEY` is load-bearing forever** — it's the AES-GCM key that
  decrypts every `stored`-mode HostKey. It must be stable across syncs and pod
  restarts, so it lives in a Secret managed independently of the git revision.
  `gen-secret.sh` refuses to overwrite an existing secret unless `--force`.
- The Application manifest therefore carries **no secret material** and is safe
  to commit. `secrets.existingSecret: relay-secrets` wires it in; the bundled PG
  and CH StatefulSets read their passwords (`postgres-password` /
  `clickhouse-password`) from the same Secret.

## Rotating the admin token

Re-mint just that key without touching the master key:

```sh
kubectl -n relay patch secret relay-secrets --type merge \
  -p "{\"stringData\":{\"RELAY_ADMIN_TOKEN\":\"sk-wr-$(openssl rand -hex 24)\"}}"
kubectl -n relay rollout restart deploy/relay
```

## GitOps secret managers

If you run sealed-secrets / SOPS / external-secrets, replace step 1 with your
controller's flow — produce a Secret named `relay-secrets` holding the same six
keys (`RELAY_MASTER_KEY`, `RELAY_ADMIN_TOKEN`, `RELAY_PG_DSN`, `RELAY_CH_DSN`,
`postgres-password`, `clickhouse-password`). The chart only cares that the
`existingSecret` exists with those keys.
