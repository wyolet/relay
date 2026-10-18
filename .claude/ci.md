# Branches, CI and releases

How changes move from a branch to a published release, and what CI runs at each step.

## Branches

| Branch | Holds | Changes arrive by |
|---|---|---|
| `dev` | the work for the next release | squash-merged PRs |
| `main` | released code; every `vX.Y.Z` tag points here | a merge commit from `dev`, or a hotfix PR |

Branch off `dev` and open the PR against `dev`. Keep branches short-lived: a PR doesn't have to be up to date with `dev` to merge, so rebase only when GitHub reports a conflict.

`dev` → `main` is always a **merge commit**, never a squash. A squash gives `main` a commit `dev` doesn't have, and every later release conflicts on it.

## CI tiers

| Event | Runs |
|---|---|
| PR into `dev` (or any branch other than `main`) | gofmt, golangci-lint, the codebase-rule checks, the build, unit tests without `-race` |
| … that touches `migrations/`, `internal/storage/`, `pkg/kv/`, `app/pipeline/`, `jobq/`, the ClickHouse sinks or `integration/` | the above, plus the integration tests |
| … that touches `chart/` or `versions.env` | the above, plus `helm lint` or the boot smoke |
| PR into `main`, push to `main`, full run on `dev` | everything: unit tests under `-race` with coverage, Postgres/Valkey and ClickHouse integration tests, fuzzing, the allocation gate, govulncheck, trivy, CodeQL |
| weekly schedule on `main` | the security scans |

The `changes` job in `.github/workflows/ci.yml` decides which jobs a run needs. Skipped jobs report as passed, so `changes` itself must stay a required check: if it fails, everything behind it is skipped.

Docs and markdown changes run nothing beyond `changes`.

Locally, `make check` runs gofmt, the codebase-rule checks, golangci-lint, the unit tests and `helm lint`; `make vulncheck` runs govulncheck. The ClickHouse tests run locally with `make test-clickhouse-run` against the server `RELAY_TEST_CH_DSN` names.

## Full run on `dev`

Every three hours `.github/workflows/full.yml` starts the full CI on `dev`'s HEAD, unless that commit already had a full run (green or red). A green run moves the `ci-full-passed` tag to it.

To run the full tier now, on any branch:

```sh
gh workflow run ci.yml --ref dev
```

## When the full run on `dev` fails

The summary of the `full.yml` run that follows the failure lists the commits since `ci-full-passed`. Each one is a single squash-merged PR.

1. Bisect them with the failing test only: `git bisect start <bad> ci-full-passed`, then `git bisect run go test ./<pkg> -run <Test>`.
2. Fix forward with a PR into `dev` that names the culprit commit, or `git revert` that commit when the fix isn't obvious and send the change back to its author.
3. The next full run checks the result.

A fix must not loosen a test to make it pass. If a test changes, the PR says why the test was wrong.

## Releasing

1. Open a PR from `dev` into `main`. It runs the full tier.
2. Merge it with a merge commit.
3. Wait for CI on the push to `main` to go green.
4. Tag: `make release` (patch), `make release-minor` or `make release-major`. The target checks that CI passed for `origin/main`'s HEAD, then tags and pushes.

The tag starts `.github/workflows/release.yml`. It doesn't re-run the tests: it refuses to publish unless CI passed for the push of the tagged commit to `main`. It builds the images (`wyolet/relay:<version>`, `:latest` lean, `:standalone` all-in-one), fails on fixable HIGH/CRITICAL vulnerabilities, pushes them to Docker Hub and GHCR with signed provenance and SBOM attestations, publishes the Helm chart to `oci://ghcr.io/wyolet/charts/relay` (chart version = image version), and creates the GitHub release with the SBOMs attached. [`SECURITY.md`](../SECURITY.md) shows how to verify the attestations.

To exercise the pipeline without publishing: `gh workflow run release.yml --ref <branch>` (`dry_run` defaults to true).

## Hotfixes

A hotfix is a PR into `main`, released like any other `main` commit. Afterwards, merge `main` back into `dev` with a PR, also as a merge commit.

## Pinned UI and catalog

`versions.env` pins the admin UI and the catalog the image bundles.

- `make ui-pin UI_VERSION=vX.Y.Z` downloads the relay-ui release tarball, verifies its signed attestation with `gh attestation verify`, and writes the version and the tarball's sha256. The image build, `make ui-fetch` and the boot smoke refuse a tarball that doesn't match the hash; a bare `docker build` needs both `UI_VERSION` and `UI_SHA256` as build args. `make image-assets-check` shows the check failing on a wrong or empty hash.
- `CATALOG_VERSION` is bumped by hand. The catalog source archive is not hash-checked.
- Base images in the `Dockerfile` are pinned as `tag@sha256:digest`; Renovate updates the digests and opens its PRs against `dev`.

## Branch rules

Both branches have a repository ruleset; admins can bypass.

| | `dev` | `main` |
|---|---|---|
| Deletion | blocked (auto-delete of merged branches would otherwise delete `dev` on release) | blocked |
| Force push | blocked | blocked |
| Changes by | PR only | PR only |
| Merge methods | squash, merge commit | merge commit, squash |
| Required checks | `changes`, `lint`, `test`, `integration`, `clickhouse`, `chart`, `boot` | the `dev` set plus `fuzz`, `bench`, `govulncheck`, `trivy`, `coverage` |
| Branch must be up to date | no | no |
