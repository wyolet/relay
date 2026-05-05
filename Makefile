.PHONY: sqlc-generate test test-integration \
        smoke-up smoke-migrate smoke-seed smoke-down

COMPOSE_FILE := deploy/compose/docker-compose.yml
PG_DSN       := postgres://relay:relay@localhost:5432/relay?sslmode=disable

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

test:
	go test ./...

test-integration:
	go test -tags=integration -race ./...
