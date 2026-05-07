# LiteLLM (for comparison)

Local LiteLLM proxy + Postgres. Used to evaluate features Relay may be missing
and to baseline performance.

## Run

```bash
cp .env.example .env
# edit .env: set OPENAI_API_KEY, generate a random LITELLM_SALT_KEY
docker compose --env-file .env up -d
```

Proxy: http://localhost:4000
Admin UI: http://localhost:4000/ui (login with `LITELLM_MASTER_KEY`)
Swagger: http://localhost:4000/

## Smoke test

```bash
curl http://localhost:4000/v1/chat/completions \
  -H "Authorization: Bearer $LITELLM_MASTER_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"ping"}]}'
```

## Features worth poking at

- `/ui` — virtual keys, teams, budgets, model access groups, spend dashboards
- `/spend/logs`, `/global/spend/*` — spend tracking surface
- `/key/generate` — virtual key issuance with budgets/TPM/RPM
- `/model/new` (with `STORE_MODEL_IN_DB=True`) — runtime model add via API
- Routing strategies: `simple-shuffle`, `least-busy`, `usage-based-routing-v2`,
  `latency-based-routing` (set under `router_settings` in config.yaml)
- Fallbacks, retries, cooldowns (`litellm_settings.fallbacks`)
- Caching (`litellm_settings.cache: true` with redis backend)
- Guardrails / pre+post call hooks
- Pass-through endpoints (`/anthropic/*`, `/vertex_ai/*`)
- Prometheus metrics at `/metrics`

## Teardown

```bash
docker compose down -v
```
