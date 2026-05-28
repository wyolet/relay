.PHONY: help dev dev-compose dev-redis dev-down down logs migrate seed seed-wipe seed-reset restart \
        image dev-push push-all local-image run-local \
        version release release-minor release-major \
        sqlc-generate test test-integration smoke-mock breakers-reset \
        control-rebuild control-logs control-login control-whoami control-openapi \
        ui-fetch build clean schemas catalog-validate

# Load .env if present.
-include .env
export

# Registry + image
REGISTRY    ?= harbor.aliboyev.com/wyolet
IMAGE_NAME  ?= relay
VERSION     ?= latest
GIT_REVISION := $(shell git rev-parse --short HEAD 2>/dev/null)

# Latest semver tag in repo, or v0.0.0 if none.
LATEST_TAG       := $(shell git tag -l 'v*.*.*' --sort=-v:refname 2>/dev/null | head -n1)
LATEST_TAG       := $(or $(LATEST_TAG),v0.0.0)
CURRENT_VERSION  := $(LATEST_TAG:v%=%)
VERSION_MAJOR    := $(word 1,$(subst ., ,$(CURRENT_VERSION)))
VERSION_MINOR    := $(word 2,$(subst ., ,$(CURRENT_VERSION)))
VERSION_PATCH    := $(word 3,$(subst ., ,$(CURRENT_VERSION)))

# Compose: prod-shape base + dev overrides (dev-stack-wired, builds locally).
# `docker compose up` without -f auto-loads docker-compose.override.yml instead
# (standalone mode — bundles PG/CH/Jaeger).
COMPOSE_BASE := deploy/compose/docker-compose.yml
COMPOSE_DEV  := deploy/compose/docker-compose.dev.yml
COMPOSE_DEV_ARGS := --env-file .env -f $(COMPOSE_BASE) -f $(COMPOSE_DEV)

# PG DSN for host-side migrate/seed (talks to dev-stack PG).
PG_DSN := $(RELAY_PG_DSN)

# Control plane endpoints (operator-facing API on :5103, fronted by relay-control-api.wyolet.dev).
CONTROL_LOCAL  := http://localhost:5103
CONTROL_PUBLIC := https://relay-control-api.wyolet.dev
CONTROL_HOST   ?= $(CONTROL_PUBLIC)
COOKIE_JAR     := /tmp/relay-control-cookie.txt

# UI release to embed.
UI_VERSION ?= v0.0.1
UI_DIST_DIR := cmd/relay/web/dist

help: ## Show this help
	@echo '════════════════════════════════════════════════════════════════'
	@echo '  wyolet relay — Makefile commands'
	@echo '════════════════════════════════════════════════════════════════'
	@echo ''
	@echo '⚡ Local dev (host-side go run — fastest inner loop):'
	@echo '  make dev               start valkey in compose, then go run relay on :$(DEV_DATA_PORT)/:$(DEV_CONTROL_PORT)'
	@echo '  make dev-redis         bring up just the valkey container (host-published on $(RELAY_VALKEY_PORT))'
	@echo '  make dev-down          stop the valkey container'
	@echo ''
	@echo '🐋 Full compose stack (multi-pod + nginx LB — for integration shape):'
	@echo '  make dev-compose       bring stack up (build relay-a/b locally, PG/CH/Jaeger from dev-stack)'
	@echo '  make down              stop + remove (no volume drop)'
	@echo '  make logs              tail relay-a/b logs'
	@echo '  make migrate           run migrations against dev-stack PG'
	@echo '  make seed              seed config/ + catalog into PG (no wipe)'
	@echo '  make seed-wipe         delete system/host/provider-owned catalog rows (keeps user-owned + settings)'
	@echo '  make seed-reset        wipe then seed — full catalog reset'
	@echo '  make restart           restart relay-a/b + nginx after a code change'
	@echo ''
	@echo '🛂 Control plane:'
	@echo '  make control-rebuild   rebuild + restart relay-a (control listener)'
	@echo '  make control-logs      tail relay-a logs'
	@echo '  make control-login     POST /control/login using .env creds'
	@echo '  make control-whoami    GET /control/whoami'
	@echo '  make control-openapi   list paths from /openapi.json'
	@echo ''
	@echo '🏗️  Container — bake on cluster:'
	@echo '  make image             build + push :$(VERSION) + :latest + :$(GIT_REVISION) to Harbor (prod)'
	@echo '  make dev-push          build + push :dev + :$(GIT_REVISION) (dev moving label)'
	@echo '  make push-all          build + push prod and dev'
	@echo '  make local-image       bake into local docker daemon as relay:dev'
	@echo '  make run-local         boot the local image on :8080'
	@echo ''
	@echo '🏷️  Release (semver via git tags):'
	@echo '  make version           show current version'
	@echo '  make release           bump patch (X.Y.Z → X.Y.(Z+1))'
	@echo '  make release-minor     bump minor'
	@echo '  make release-major     bump major'
	@echo ''
	@echo '🧰 Go:'
	@echo '  make sqlc-generate     regenerate sqlc code'
	@echo '  make test              go test ./...'
	@echo '  make test-integration  integration tag, race'
	@echo '  make smoke-mock        replay recorded fixtures through relay → openai-mock.wyolet.dev'
	@echo '  make ui-fetch          fetch relay-ui $(UI_VERSION) into $(UI_DIST_DIR)'
	@echo '  make build             ui-fetch + go build → ./relay'
	@echo '  make clean             drop UI dist + binary'
	@echo ''
	@echo '════════════════════════════════════════════════════════════════'
	@echo '⚙️  Current configuration:'
	@echo '════════════════════════════════════════════════════════════════'
	@echo '  Registry:      $(REGISTRY)'
	@echo '  Image:         $(IMAGE_NAME)'
	@echo '  Version:       $(VERSION)'
	@echo '  Git revision:  $(GIT_REVISION)'
	@echo '  Latest tag:    $(LATEST_TAG)'
	@echo ''

# --- local dev (host-side go run) ---

# Ports for `make dev`. Match what Caddy on dev-stack expects (relay-api.wyolet.dev
# → Mac:$RELAY_LB_PORT, relay-control-api.wyolet.dev → Mac:$RELAY_CONTROL_PORT)
# so the live URLs keep working without touching dev-stack.
DEV_DATA_PORT    ?= $(RELAY_LB_PORT)
DEV_CONTROL_PORT ?= $(RELAY_CONTROL_PORT)
RELAY_VALKEY_PORT ?= 6379

dev: dev-redis ## go run on the Mac against dev-stack PG/CH + local valkey
	@echo "▸ relay-api.wyolet.dev → Mac:$(DEV_DATA_PORT)   control → :$(DEV_CONTROL_PORT)"
	RELAY_PORT=$(DEV_DATA_PORT) \
	RELAY_CONTROL_PORT=$(DEV_CONTROL_PORT) \
	RELAY_REDIS_ADDR=127.0.0.1:$(RELAY_VALKEY_PORT) \
	RELAY_CONFIG_DIR=$(CURDIR)/deploy/compose/config \
	RELAY_INSTANCE_ID=relay-local \
	go run ./cmd/relay

dev-redis: ## bring up just the valkey container, host-published on $(RELAY_VALKEY_PORT)
	RELAY_VALKEY_PORT=$(RELAY_VALKEY_PORT) docker compose $(COMPOSE_DEV_ARGS) up -d valkey
	@until docker exec relay-valkey valkey-cli ping >/dev/null 2>&1; do sleep 0.5; done
	@echo "valkey ready on 127.0.0.1:$(RELAY_VALKEY_PORT)"

dev-down: ## stop the valkey container
	docker compose $(COMPOSE_DEV_ARGS) stop valkey

breakers-reset: ## clear all keypool circuit-breaker state in valkey (lets the next request retry from healthy)
	@for k in $$(docker exec relay-valkey valkey-cli --scan --pattern 'secret_health:*'); do \
		docker exec -i relay-valkey valkey-cli del "$$k" >/dev/null; \
	done
	@echo "circuit breakers cleared"

# --- full compose stack ---

dev-compose: ## bring full multi-pod stack up against dev-stack
	docker compose $(COMPOSE_DEV_ARGS) up -d --build
	@echo "Waiting for relay LB on :$(RELAY_LB_PORT)..."
	@until curl -sf http://localhost:$(RELAY_LB_PORT)/healthz >/dev/null 2>&1; do sleep 1; done
	@echo "Stack up. nginx → :$(RELAY_LB_PORT)  control → :$(RELAY_CONTROL_PORT)"

down: ## stop + remove
	docker compose $(COMPOSE_DEV_ARGS) down --remove-orphans

logs: ## tail relay-a/b logs
	docker compose $(COMPOSE_DEV_ARGS) logs -f relay-a relay-b

migrate: ## run migrations against dev-stack PG
	RELAY_PG_DSN='$(PG_DSN)' go run ./cmd/relay migrate up

seed: seed-system seed-catalog ## seed both relay-internal yamls (./config) and the public catalog (../relay-catalog/data)

seed-system: ## seed relay-internal yamls from ./config — system RLs + users
	RELAY_PG_DSN='$(PG_DSN)' go run ./cmd/relay seed --from config --apply

seed-catalog: ## seed the public catalog from $$RELAY_CATALOG_DIR (default ../relay-catalog/data)
	@if [ -z "$${RELAY_CATALOG_DIR:-}" ] && [ ! -d ../relay-catalog/data ]; then \
		echo "error: set RELAY_CATALOG_DIR or clone wyolet/relay-catalog as ../relay-catalog"; exit 1; \
	fi
	RELAY_PG_DSN='$(PG_DSN)' go run ./cmd/relay seed --from "$${RELAY_CATALOG_DIR:-../relay-catalog/data}" --apply

seed-loadtest: ## seed catalog from deploy/compose/config (load-tester fixtures)
	RELAY_PG_DSN='$(PG_DSN)' go run ./cmd/relay seed --from deploy/compose/config --apply

# `make seed-wipe` deletes rows whose metadata.owner.kind is one of
# system/host/provider — the catalog tier. User-owned rows (the operator's
# own relay keys, hand-edited policies, BYO host keys) survive. Settings
# and schema_migrations are never touched.
SEED_WIPE_KINDS := 'system','host','provider'

# Delete order respects FK RESTRICTs: drop dependents (policy/pricing/
# secret rows) before the models/hosts/providers they point at. Join
# tables (policy_models, policy_host_keys, pricing_models) cascade with
# their owning rows.
define SEED_WIPE_SQL
BEGIN;
DELETE FROM pricings    WHERE metadata->'owner'->>'kind' IN ($(SEED_WIPE_KINDS));
DELETE FROM policies    WHERE metadata->'owner'->>'kind' IN ($(SEED_WIPE_KINDS));
DELETE FROM secrets     WHERE metadata->'owner'->>'kind' IN ($(SEED_WIPE_KINDS));
DELETE FROM rate_limits WHERE metadata->'owner'->>'kind' IN ($(SEED_WIPE_KINDS));
DELETE FROM models      WHERE metadata->'owner'->>'kind' IN ($(SEED_WIPE_KINDS));
DELETE FROM hosts       WHERE metadata->'owner'->>'kind' IN ($(SEED_WIPE_KINDS));
DELETE FROM providers   WHERE metadata->'owner'->>'kind' IN ($(SEED_WIPE_KINDS));
COMMIT;
endef
export SEED_WIPE_SQL

seed-wipe: ## delete system/host/provider-owned catalog rows (keeps user-owned + settings). Destructive.
	@printf "About to delete owner.kind ∈ ($(SEED_WIPE_KINDS)) rows from pricings/policies/secrets/rate_limits/models/hosts/providers.\nUser-owned rows + settings survive.\nPG_DSN=$(PG_DSN)\nType 'yes' to continue: " && read ans && [ "$$ans" = "yes" ] || (echo "aborted"; exit 1)
	@echo "$$SEED_WIPE_SQL" | psql '$(PG_DSN)' -v ON_ERROR_STOP=1

seed-reset: seed-wipe seed ## wipe catalog tables then seed-system + seed-catalog

restart: ## restart relay-a/b + nginx after a rebuild
	docker compose $(COMPOSE_DEV_ARGS) up -d --build relay-a relay-b
	docker compose $(COMPOSE_DEV_ARGS) restart nginx

# --- container (bake on cluster) ---

image: ## build + push prod (multi-tag)
	@echo "▸ pushing $(REGISTRY)/$(IMAGE_NAME) — version=$(VERSION) sha=$(GIT_REVISION)"
	VERSION=$(VERSION) GIT_REVISION=$(GIT_REVISION) docker buildx bake -f docker-bake.hcl --push prod

dev-push: ## build + push :dev moving label
	@echo "▸ pushing $(REGISTRY)/$(IMAGE_NAME):dev — sha=$(GIT_REVISION)"
	GIT_REVISION=$(GIT_REVISION) docker buildx bake -f docker-bake.hcl --push dev

push-all: ## build + push both prod and dev
	VERSION=$(VERSION) GIT_REVISION=$(GIT_REVISION) docker buildx bake -f docker-bake.hcl --push all

local-image: ## bake into local docker as relay:dev
	docker buildx bake -f docker-bake.hcl local

run-local: local-image ## boot the local image on :8080
	docker run --rm -p 8080:8080 --name relay-local relay:dev

# --- release ---

version: ## show current version from git tags
	@echo "Current: $(LATEST_TAG)  (M=$(VERSION_MAJOR) m=$(VERSION_MINOR) p=$(VERSION_PATCH))"

release: ## bump patch + build + push + tag
	git fetch --tags --force --prune
	$(eval NEW_PATCH := $(shell echo $$(($(VERSION_PATCH) + 1))))
	$(eval NEW_VERSION := $(VERSION_MAJOR).$(VERSION_MINOR).$(NEW_PATCH))
	@echo "📦 current: $(LATEST_TAG)"
	@echo "🚀 next:    v$(NEW_VERSION)"
	@if git rev-parse "v$(NEW_VERSION)" >/dev/null 2>&1; then \
		echo "⚠️  tag v$(NEW_VERSION) already exists. delete locally: git tag -d v$(NEW_VERSION)"; exit 1; \
	fi
	VERSION=$(NEW_VERSION) GIT_REVISION=$(GIT_REVISION) docker buildx bake -f docker-bake.hcl --push prod
	git tag -a "v$(NEW_VERSION)" -m "Release v$(NEW_VERSION)"
	git push origin "v$(NEW_VERSION)"
	@echo "✅ released v$(NEW_VERSION)"

release-minor: ## bump minor + build + push + tag
	git fetch --tags --force --prune
	$(eval NEW_MINOR := $(shell echo $$(($(VERSION_MINOR) + 1))))
	$(eval NEW_VERSION := $(VERSION_MAJOR).$(NEW_MINOR).0)
	@echo "📦 current: $(LATEST_TAG)"
	@echo "🚀 next:    v$(NEW_VERSION)"
	@if git rev-parse "v$(NEW_VERSION)" >/dev/null 2>&1; then \
		echo "⚠️  tag v$(NEW_VERSION) already exists. delete locally: git tag -d v$(NEW_VERSION)"; exit 1; \
	fi
	VERSION=$(NEW_VERSION) GIT_REVISION=$(GIT_REVISION) docker buildx bake -f docker-bake.hcl --push prod
	git tag -a "v$(NEW_VERSION)" -m "Release v$(NEW_VERSION)"
	git push origin "v$(NEW_VERSION)"
	@echo "✅ released v$(NEW_VERSION)"

release-major: ## bump major + build + push + tag
	git fetch --tags --force --prune
	$(eval NEW_MAJOR := $(shell echo $$(($(VERSION_MAJOR) + 1))))
	$(eval NEW_VERSION := $(NEW_MAJOR).0.0)
	@echo "📦 current: $(LATEST_TAG)"
	@echo "🚀 next:    v$(NEW_VERSION)"
	@if git rev-parse "v$(NEW_VERSION)" >/dev/null 2>&1; then \
		echo "⚠️  tag v$(NEW_VERSION) already exists. delete locally: git tag -d v$(NEW_VERSION)"; exit 1; \
	fi
	VERSION=$(NEW_VERSION) GIT_REVISION=$(GIT_REVISION) docker buildx bake -f docker-bake.hcl --push prod
	git tag -a "v$(NEW_VERSION)" -m "Release v$(NEW_VERSION)"
	git push origin "v$(NEW_VERSION)"
	@echo "✅ released v$(NEW_VERSION)"

# --- control plane ---

control-rebuild: ## rebuild relay-a (control listener)
	docker compose $(COMPOSE_DEV_ARGS) up -d --build --force-recreate relay-a
	@echo "Waiting for control listener..."
	@until curl -sfo /dev/null $(CONTROL_LOCAL)/openapi.json; do sleep 1; done
	@echo "Control listener up at $(CONTROL_LOCAL) ($(CONTROL_PUBLIC))."

control-logs: ## tail relay-a logs
	docker compose $(COMPOSE_DEV_ARGS) logs -f relay-a

control-login: ## POST /control/login using .env creds
	@test -f .env || (echo "no .env; create one with RELAY_ADMIN_USERNAME and RELAY_ADMIN_PASSWORD" && exit 1)
	@USERNAME=$$(grep '^RELAY_ADMIN_USERNAME=' .env | cut -d= -f2-); \
	 PASSWORD=$$(grep '^RELAY_ADMIN_PASSWORD=' .env | cut -d= -f2-); \
	 if [ -z "$$USERNAME" ]; then USERNAME=aaliboyev; fi; \
	 rm -f $(COOKIE_JAR); \
	 curl -sS -c $(COOKIE_JAR) -X POST $(CONTROL_HOST)/control/login \
	   -H 'content-type: application/json' \
	   -d "{\"username\":\"$$USERNAME\",\"password\":\"$$PASSWORD\"}" \
	   -w "\nstatus=%{http_code}\n"

control-whoami: ## GET /control/whoami
	@curl -sS -b $(COOKIE_JAR) -w "\nstatus=%{http_code}\n" $(CONTROL_HOST)/control/whoami

control-openapi: ## list paths from /openapi.json
	@curl -sS $(CONTROL_HOST)/openapi.json | python3 -c \
	  "import json,sys; d=json.load(sys.stdin); print('title:', d['info']['title']); print('paths:'); [print(' ', p) for p in sorted(d['paths'])]"

# --- go ---

sqlc-generate: ## regenerate sqlc code
	sqlc generate

ui-fetch: ## fetch relay-ui release tarball
	@echo "Fetching relay-ui $(UI_VERSION)..."
	@mkdir -p $(UI_DIST_DIR)
	curl -fsSL "https://github.com/wyolet/relay-ui/releases/download/$(UI_VERSION)/relay-ui-$(UI_VERSION).tar.gz" \
	  | tar -xz -C $(UI_DIST_DIR) --strip-components=1
	@echo "UI fetched into $(UI_DIST_DIR)"

build: ui-fetch ## ui-fetch + go build → ./relay
	CGO_ENABLED=0 go build -trimpath -o relay ./cmd/relay

clean: ## drop UI dist + binary
	rm -rf $(UI_DIST_DIR)
	rm -f relay

test: ## go test ./...
	go test ./...

schemas: ## regenerate JSON Schemas for catalog kinds → schemas/v1alpha2/
	go run ./cmd/catalog-schemas schemas/v1alpha2

catalog-validate: ## graph-lint the public catalog ($$RELAY_CATALOG_DIR or ../relay-catalog/data)
	go run ./cmd/catalog-validate $${RELAY_CATALOG_DIR:-../relay-catalog/data}

catalog-embed: ## generate sdk/catalog/catalog.json from the public catalog
	go run ./cmd/catalog-embed -o sdk/catalog/catalog.json $${RELAY_CATALOG_DIR:-../relay-catalog/data}

COMPOSE_TEST := deploy/compose/docker-compose.test.yml
TEST_PG_DSN  := postgres://relay:relay@127.0.0.1:5499/relay_test?sslmode=disable

test-integration: ## spin up ephemeral pg, run integration-tagged tests with -race, tear down
	docker compose -f $(COMPOSE_TEST) up -d --wait
	RELAY_TEST_PG_DSN='$(TEST_PG_DSN)' go test -tags=integration -race ./... ; \
		status=$$?; \
		docker compose -f $(COMPOSE_TEST) down -v; \
		exit $$status

# Smoke: relay → openai-mock.wyolet.dev (via Caddy on dev-stack, mock on Mac:5180).
# Sister repo: ../../spec-mock-openai/ provides the mock binary + fixture corpus.
# Extracts two fixture bodies (sync + streaming+parallel-tool-calls) into /tmp,
# starts the mock if not running, brings up test PG, runs the replay tests.
# Idempotent — re-running uses cached artifacts.
SPEC_MOCK_DIR ?= $(HOME)/projects/wyolet/spec-mock-openai
MOCK_BIN      := /tmp/spec-mock-openai
MOCK_FIXTURES := /tmp/mock-fixtures
MOCK_CORPUS   := $(SPEC_MOCK_DIR)/fixtures-corpus/openai-mini-2026-05-19.tar.zst

smoke-mock: ## replay recorded openai-mini fixtures through relay → openai-mock.wyolet.dev
	@command -v zstd >/dev/null || { echo "zstd required (brew install zstd)"; exit 1; }
	@test -d $(SPEC_MOCK_DIR) || { echo "missing $(SPEC_MOCK_DIR) — clone wyolet/spec-mock-openai"; exit 1; }
	@test -x $(MOCK_BIN) || (cd $(SPEC_MOCK_DIR) && go build -o $(MOCK_BIN) .)
	@test -d $(MOCK_FIXTURES) || (mkdir -p $(MOCK_FIXTURES) && cd $(MOCK_FIXTURES) && \
		zstd -d --stdout $(MOCK_CORPUS) | tar -xf - && rm -f gemma4.tar)
	@test -f /tmp/fixbody.json || python3 -c "import json,glob; \
		obj=json.loads(open(sorted(glob.glob('$(MOCK_FIXTURES)/session-*.jsonl'))[0]).readline()); \
		open('/tmp/fixbody.json','w').write(json.dumps(obj['match']['body_json_equals']))"
	@test -f /tmp/fixbody-parallel.json || python3 -c "import json; \
		obj=json.loads(open('$(MOCK_FIXTURES)/session-1779156057-87325-4.jsonl').readlines()[9]); \
		open('/tmp/fixbody-parallel.json','w').write(json.dumps(obj['match']['body_json_equals']))"
	@pgrep -f 'spec-mock-openai.*-addr.*5180' >/dev/null || \
		( $(MOCK_BIN) -addr ":5180" \
			-spec $(SPEC_MOCK_DIR)/spec/openapi.yaml \
			-fixtures-dir $(MOCK_FIXTURES) >/tmp/spec-mock-openai.log 2>&1 & \
		  sleep 2 && echo "mock started on :5180 (log: /tmp/spec-mock-openai.log)" )
	docker compose -f $(COMPOSE_TEST) up -d --wait
	RELAY_TEST_PG_DSN='$(TEST_PG_DSN)' go test -tags=integration -race -run TestMockReplay -v ./integration/ ; \
		status=$$?; \
		docker compose -f $(COMPOSE_TEST) down -v; \
		exit $$status
