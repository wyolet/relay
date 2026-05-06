.PHONY: sqlc-generate test test-integration \
        smoke-up smoke-migrate smoke-seed smoke-down \
        ui-fetch build clean

COMPOSE_FILE := deploy/compose/docker-compose.yml
PG_DSN       := postgres://relay:relay@localhost:5432/relay?sslmode=disable

# UI release to embed. PER-272 publishes v0.0.1 in wyolet/relay-ui; bump here on each UI release.
UI_VERSION ?= v0.0.1
UI_DIST_DIR := cmd/relay/web/dist

smoke-up:
	docker compose -f $(COMPOSE_FILE) up -d --build
	@echo "Waiting for Postgres to be healthy..."
	@until docker inspect compose-postgres-1 --format '{{.State.Health.Status}}' 2>/dev/null | grep -q healthy; do sleep 2; done
	@$(MAKE) smoke-migrate
	@echo "Restarting relay instances after migration..."
	@docker compose -f $(COMPOSE_FILE) restart relay-a relay-b
	@echo "Restarting nginx after relay instances are up..."
	@docker compose -f $(COMPOSE_FILE) restart nginx
	@echo "Waiting for nginx to be reachable..."
	@until curl -sf http://localhost:8080/healthz >/dev/null 2>&1; do sleep 2; done
	@echo "Stack is up."

smoke-migrate:
	RELAY_PG_DSN=$(PG_DSN) go run ./cmd/relay migrate up

smoke-seed:
	RELAY_PG_DSN=$(PG_DSN) go run ./cmd/relay seed --from deploy/compose/config --apply

smoke-down:
	docker compose -f $(COMPOSE_FILE) down -v --remove-orphans

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
