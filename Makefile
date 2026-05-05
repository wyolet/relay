.PHONY: sqlc-generate test test-integration

sqlc-generate:
	sqlc generate

test:
	go test ./...

test-integration:
	go test -tags=integration -race ./...
