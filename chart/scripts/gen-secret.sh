#!/usr/bin/env bash
# Generate and apply relay's Kubernetes Secret, then hand the chart an
# existingSecret to consume. Run this ONCE per environment before the first
# ArgoCD sync (ArgoCD cannot mint secrets — this is the one out-of-band step).
#
# The Secret carries everything the chart's `secrets.existingSecret` path reads:
#   RELAY_MASTER_KEY    — AES-GCM key. LOAD-BEARING FOREVER: it decrypts every
#                         stored HostKey. Rotate it and all `stored`-mode upstream
#                         credentials become unreadable. Never regenerated on
#                         re-run (see the exists-guard below).
#   RELAY_ADMIN_TOKEN   — break-glass bearer for the control API.
#   RELAY_PG_DSN        — relay's OWN Postgres (bundled service DNS).
#   RELAY_CH_DSN        — relay's ClickHouse (bundled service DNS).
#   postgres-password   — consumed by the bundled PG StatefulSet.
#   clickhouse-password — consumed by the bundled CH StatefulSet.
#
# Passwords are hex (URL-safe) because they are embedded verbatim in the DSN URLs.
#
# Usage:
#   ./gen-secret.sh                      # ns=relay release=relay secret=relay-secrets
#   NAMESPACE=relay-prod RELEASE=relay ./gen-secret.sh
#   ./gen-secret.sh --force              # overwrite an existing secret (DANGER)
#   ./gen-secret.sh --show               # print the admin token from the live secret
#   ./gen-secret.sh --refresh-dsn        # rewrite PG/CH DSNs to FQDN hosts, in place
#                                        # (safe: master key + passwords untouched)
set -euo pipefail

NAMESPACE="${NAMESPACE:-relay}"
RELEASE="${RELEASE:-relay}"
SECRET_NAME="${SECRET_NAME:-relay-secrets}"
PG_USER="${PG_USER:-relay}"
PG_DB="${PG_DB:-relay}"
CH_USER="${CH_USER:-relay}"
CH_DB="${CH_DB:-relay}"
# Cluster DNS domain — must match the chart's clusterDomain value so the DSN
# FQDNs resolve. Trailing-dot absolute names skip ndots:5 search expansion.
CLUSTER_DOMAIN="${CLUSTER_DOMAIN:-cluster.local}"
FORCE=0
SHOW=0
REFRESH_DSN=0
for arg in "$@"; do
  case "$arg" in
    --force) FORCE=1 ;;
    --show)  SHOW=1 ;;
    --refresh-dsn) REFRESH_DSN=1 ;;
    -h|--help) sed -n '2,30p' "$0"; exit 0 ;;
    *) echo "unknown arg: $arg" >&2; exit 2 ;;
  esac
done

command -v kubectl >/dev/null || { echo "kubectl not found" >&2; exit 1; }
command -v openssl >/dev/null || { echo "openssl not found" >&2; exit 1; }

# Mirror the chart's relay.fullname logic so the DSN service names match.
if [[ "$RELEASE" == *relay* ]]; then FULLNAME="$RELEASE"; else FULLNAME="${RELEASE}-relay"; fi

# Absolute FQDNs (trailing dot) for the bundled stores. An absolute name skips
# the resolver's ndots:5 search-list expansion — one ~20ms query instead of the
# bare-name fan-out (~8 queries) that hits the conntrack UDP race and stalls cold
# connections for seconds. This is exactly what relay dials.
PG_HOST="${FULLNAME}-postgresql.${NAMESPACE}.svc.${CLUSTER_DOMAIN}."
CH_HOST="${FULLNAME}-clickhouse.${NAMESPACE}.svc.${CLUSTER_DOMAIN}."

# --refresh-dsn: rewrite ONLY RELAY_PG_DSN + RELAY_CH_DSN in the existing secret
# to the FQDN hosts, preserving the embedded credentials verbatim (host-swap via
# sed). Master key, DB passwords, and tokens are never read or changed — safe on
# a live cluster and idempotent (re-running yields the same DSNs).
if [[ "$REFRESH_DSN" == 1 ]]; then
  kubectl -n "$NAMESPACE" get secret "$SECRET_NAME" >/dev/null 2>&1 \
    || { echo "secret $NAMESPACE/$SECRET_NAME not found — mint it first (run without --refresh-dsn)" >&2; exit 1; }
  old_pg="$(kubectl -n "$NAMESPACE" get secret "$SECRET_NAME" -o jsonpath='{.data.RELAY_PG_DSN}' | base64 -d)"
  old_ch="$(kubectl -n "$NAMESPACE" get secret "$SECRET_NAME" -o jsonpath='{.data.RELAY_CH_DSN}' | base64 -d)"
  new_pg="$(printf '%s' "$old_pg" | sed -E "s#@[^@/]+:5432/#@${PG_HOST}:5432/#")"
  new_ch="$(printf '%s' "$old_ch" | sed -E "s#@[^@/]+:9000/#@${CH_HOST}:9000/#")"
  kubectl -n "$NAMESPACE" patch secret "$SECRET_NAME" --type merge \
    -p "{\"stringData\":{\"RELAY_PG_DSN\":\"${new_pg}\",\"RELAY_CH_DSN\":\"${new_ch}\"}}"
  echo "✓ refreshed RELAY_PG_DSN + RELAY_CH_DSN to FQDN hosts (master key + passwords + tokens untouched)"
  echo "    PG → ${PG_HOST}"
  echo "    CH → ${CH_HOST}"
  echo "  Roll to apply:  kubectl -n ${NAMESPACE} rollout restart deploy/${FULLNAME}"
  exit 0
fi

if [[ "$SHOW" == 1 ]]; then
  kubectl -n "$NAMESPACE" get secret "$SECRET_NAME" \
    -o jsonpath='{.data.RELAY_ADMIN_TOKEN}' | base64 -d; echo
  exit 0
fi

kubectl get namespace "$NAMESPACE" >/dev/null 2>&1 || kubectl create namespace "$NAMESPACE"

if kubectl -n "$NAMESPACE" get secret "$SECRET_NAME" >/dev/null 2>&1; then
  if [[ "$FORCE" != 1 ]]; then
    echo "✓ secret $NAMESPACE/$SECRET_NAME already exists — leaving it untouched."
    echo "  (Regenerating RELAY_MASTER_KEY would orphan every stored HostKey."
    echo "   Pass --force only if you accept that.)"
    exit 0
  fi
  echo "⚠ --force: deleting and recreating $NAMESPACE/$SECRET_NAME."
  echo "  Any 'stored'-mode HostKeys encrypted under the old master key are now UNREADABLE."
  kubectl -n "$NAMESPACE" delete secret "$SECRET_NAME"
fi

MASTER_KEY="$(openssl rand -base64 32)"
ADMIN_TOKEN="sk-wr-$(openssl rand -hex 24)"
ADMIN_PASSWORD="$(openssl rand -hex 16)"
PG_PW="$(openssl rand -hex 24)"
CH_PW="$(openssl rand -hex 24)"

PG_DSN="postgres://${PG_USER}:${PG_PW}@${PG_HOST}:5432/${PG_DB}?sslmode=disable"
CH_DSN="clickhouse://${CH_USER}:${CH_PW}@${CH_HOST}:9000/${CH_DB}"

kubectl -n "$NAMESPACE" create secret generic "$SECRET_NAME" \
  --from-literal=RELAY_MASTER_KEY="$MASTER_KEY" \
  --from-literal=RELAY_ADMIN_TOKEN="$ADMIN_TOKEN" \
  --from-literal=RELAY_ADMIN_PASSWORD="$ADMIN_PASSWORD" \
  --from-literal=RELAY_PG_DSN="$PG_DSN" \
  --from-literal=RELAY_CH_DSN="$CH_DSN" \
  --from-literal=postgres-password="$PG_PW" \
  --from-literal=clickhouse-password="$CH_PW"

kubectl -n "$NAMESPACE" label secret "$SECRET_NAME" \
  app.kubernetes.io/part-of=relay app.kubernetes.io/managed-by=gen-secret.sh --overwrite >/dev/null

cat <<EOF

✓ created secret $NAMESPACE/$SECRET_NAME
  Point the chart at it:   secrets.existingSecret: $SECRET_NAME
  (this is already set in infra/k8s/argocd/relay-application.yaml)

  Admin bearer token (control API):
    $ADMIN_TOKEN

  Admin UI login (when auth.adminUser.enabled, username "admin"):
    password: $ADMIN_PASSWORD

  Re-read the token later:  $0 --show
EOF
