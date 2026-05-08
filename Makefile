.PHONY: sqlc-generate test test-integration \
        smoke-up smoke-migrate smoke-seed smoke-down \
        control-rebuild control-logs control-login control-whoami control-openapi \
        ui-fetch build clean

COMPOSE_FILE := deploy/compose/docker-compose.yml
PG_DSN       := postgres://relay:relay@localhost:5432/relay?sslmode=disable

# Control plane (operator-facing API on :5103, fronted by relay-control-api.wyolet.dev).
CONTROL_LOCAL  := http://localhost:5103
CONTROL_PUBLIC := https://relay-control-api.wyolet.dev
CONTROL_HOST   ?= $(CONTROL_PUBLIC)
COOKIE_JAR     := /tmp/relay-control-cookie.txt

# UI release to embed. PER-272 publishes v0.0.1 in wyolet/relay-ui; bump here on each UI release.
UI_VERSION ?= v0.0.1
UI_DIST_DIR := cmd/relay/web/dist

smoke-up:
	docker compose --env-file .env -f $(COMPOSE_FILE) up -d --build
	@echo "Waiting for Postgres to be healthy..."
	@until docker inspect compose-postgres-1 --format '{{.State.Health.Status}}' 2>/dev/null | grep -q healthy; do sleep 2; done
	@$(MAKE) smoke-migrate
	@echo "Restarting relay instances after migration..."
	@docker compose --env-file .env -f $(COMPOSE_FILE) restart relay-a relay-b
	@echo "Restarting nginx after relay instances are up..."
	@docker compose --env-file .env -f $(COMPOSE_FILE) restart nginx
	@echo "Waiting for nginx to be reachable..."
	@until curl -sf http://localhost:8080/healthz >/dev/null 2>&1; do sleep 2; done
	@echo "Stack is up."

smoke-migrate:
	RELAY_PG_DSN=$(PG_DSN) go run ./cmd/relay migrate up

smoke-seed:
	RELAY_PG_DSN=$(PG_DSN) go run ./cmd/relay seed --from deploy/compose/config --apply

smoke-down:
	docker compose --env-file .env -f $(COMPOSE_FILE) down -v --remove-orphans

# control-rebuild rebuilds relay-a only (control listener lives on it) and
# force-recreates the container. Use after editing internal/control/* or
# anything else affecting the control surface.
control-rebuild:
	docker compose --env-file .env -f $(COMPOSE_FILE) up -d --build --force-recreate relay-a
	@echo "Waiting for control listener..."
	@until curl -sfo /dev/null $(CONTROL_LOCAL)/openapi.json; do sleep 1; done
	@echo "Control listener up at $(CONTROL_LOCAL) ($(CONTROL_PUBLIC))."

control-logs:
	docker compose --env-file .env -f $(COMPOSE_FILE) logs -f relay-a

# control-login posts {username, password} to /control/login. Override the
# host with CONTROL_HOST=$(CONTROL_LOCAL) for in-pod testing without DNS.
# Reads RELAY_ADMIN_USERNAME and RELAY_ADMIN_PASSWORD from the local .env.
control-login:
	@test -f .env || (echo "no .env; create one with RELAY_ADMIN_USERNAME and RELAY_ADMIN_PASSWORD" && exit 1)
	@USERNAME=$$(grep '^RELAY_ADMIN_USERNAME=' .env | cut -d= -f2-); \
	 PASSWORD=$$(grep '^RELAY_ADMIN_PASSWORD=' .env | cut -d= -f2-); \
	 if [ -z "$$USERNAME" ]; then USERNAME=aaliboyev; fi; \
	 rm -f $(COOKIE_JAR); \
	 curl -sS -c $(COOKIE_JAR) -X POST $(CONTROL_HOST)/control/login \
	   -H 'content-type: application/json' \
	   -d "{\"username\":\"$$USERNAME\",\"password\":\"$$PASSWORD\"}" \
	   -w "\nstatus=%{http_code}\n"

control-whoami:
	@curl -sS -b $(COOKIE_JAR) -w "\nstatus=%{http_code}\n" $(CONTROL_HOST)/control/whoami

control-openapi:
	@curl -sS $(CONTROL_HOST)/openapi.json | python3 -c \
	  "import json,sys; d=json.load(sys.stdin); print('title:', d['info']['title']); print('paths:'); [print(' ', p) for p in sorted(d['paths'])]"

sqlc-generate:
	sqlc generate

# ui-fetch downloads and extracts the pinned relay-ui tarball into cmd/relay/web/dist/.
# Idempotent: re-running overwrites existing files.
ui-fetch:
	@echo "Fetching relay-ui $(UI_VERSION)..."
	@mkdir -p $(UI_DIST_DIR)
	curl -fsSL "https://github.com/wyolet/relay-ui/releases/download/$(UI_VERSION)/relay-ui-$(UI_VERSION).tar.gz" \
	  | tar -xz -C $(UI_DIST_DIR) --strip-components=1
	@echo "UI fetched into $(UI_DIST_DIR)"

# build fetches the UI then compiles the relay binary.
build: ui-fetch
	CGO_ENABLED=0 go build -trimpath -o relay ./cmd/relay

clean:
	rm -rf $(UI_DIST_DIR)
	rm -f relay

test:
	go test ./...

test-integration:
	go test -tags=integration -race ./...
