<!-- Thanks for contributing! Keep PRs to one logical change. -->

## What

<!-- What does this PR do, and why? Link any related issue. -->

## How

<!-- Key implementation notes — anything a reviewer should know. -->

## Checklist

- [ ] One logical change; branched off `main`.
- [ ] `make test` + `go vet ./...` + `gofmt` pass locally.
- [ ] Integration tests updated/passing if the PG or e2e path changed
      (`make test-integration`), and `integration/` still compiles.
- [ ] New behavior has tests.
- [ ] No vendor names leaked into `app/` code; SDK module purity preserved
      (`make lint-rules` clean).
- [ ] No secrets, credentials, or private-infra references added.
- [ ] Docs updated if behavior/config changed.
