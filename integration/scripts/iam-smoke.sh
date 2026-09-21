#!/usr/bin/env bash
# Runs the operator scenario walk against the compose test Postgres in a
# database of its own, so a run never disturbs the shared relay_test one.
# The stack is left up: it is shared, and bringing it down is the caller's
# call (see the test-integration target).
#
#   integration/scripts/iam-smoke.sh [extra go test flags...]
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
cd "$repo_root"

compose_file=deploy/compose/docker-compose.test.yml
pg_user=relay
pg_password=relay
db=relay_e2e

docker compose -f "$compose_file" up -d --wait

# psql runs inside the compose service, so the host needs no client and the
# database can only ever be created on the stack this script brought up.
psql_in_stack() {
	docker compose -f "$compose_file" exec -T -e PGPASSWORD="$pg_password" pg-test \
		psql -v ON_ERROR_STOP=1 -U "$pg_user" -d postgres "$@"
}

drop_db() {
	psql_in_stack -c "DROP DATABASE IF EXISTS \"$db\" WITH (FORCE)" >/dev/null
}

drop_db
psql_in_stack -c "CREATE DATABASE \"$db\" OWNER \"$pg_user\"" >/dev/null
trap drop_db EXIT INT TERM

port=$(docker compose -f "$compose_file" port pg-test 5432)
echo "running the operator scenario walk against ${port}/${db}"
RELAY_TEST_PG_DSN="postgres://${pg_user}:${pg_password}@${port}/${db}?sslmode=disable" \
	go test -tags=integration -race -count=1 -run TestOperatorWalk ./integration/ "$@"
