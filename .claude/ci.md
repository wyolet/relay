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

Two workflows, so a run lists only the jobs its tier has: `.github/workflows/ci-dev.yml` (`CI dev`) runs the light tier on PRs into any branch other than `main`; `.github/workflows/ci.yml` (`CI`) runs the full tier on everything else.

| Event | Runs |
|---|---|
| PR into `dev` (or any branch other than `main`) | one `dev` job: gofmt, golangci-lint, the codebase-rule checks, the build, unit tests without `-race` |
| … that touches `migrations/`, `internal/storage/`, `pkg/kv/` or a kv consumer with Valkey tests, `app/pipeline/`, `jobq/`, the ClickHouse sinks or `integration/` | the above, plus the integration tests the change can reach |
| … that touches `chart/` or `versions.env` | the above, plus `helm lint` or the boot smoke |
| PR into `main`, push to `main`, full run on `dev` | everything: unit tests under `-race` with coverage, Postgres/Valkey and ClickHouse integration tests, fuzzing, the allocation gate, govulncheck, trivy, CodeQL |
| weekly schedule on `main` | the security scans |

The `dev` job runs its parts in parallel through `scripts/ci-dev.sh`, each part in its own log group, with a summary table at the end; it fails when any part fails. Its integration part runs only the integration-tagged packages (`scripts/integration-pkgs.sh`) whose test binary depends on a changed package, without coverage. A change to migrations, module files, the Makefile, `ci-dev.yml` or those scripts runs all of them. The unit tests are a separate part, so integration doesn't rerun them. Locally: `scripts/ci-dev.sh lint test integration` (integration and ClickHouse need `RELAY_TEST_PG_DSN`, `RELAY_TEST_REDIS_ADDR` and `RELAY_TEST_CH_DSN`; `CHANGED_FROM=<commit>` limits integration to what changed since it).

On the cluster, PRs into `dev` run the `dev` job on the warm pool `relay-ci-warm` (a node-local Go and lint cache shared by its pods, with Postgres, Valkey and ClickHouse sidecars) when the repository variable `CI_WARM_POOL` is `on`. Otherwise, and for PRs into any other branch or a dispatched run (the warm pool's job hook admits only PRs into `dev`), it runs on the cold `relay-ci-integration` pool. Set the variable to anything else to move off the warm pool, for instance when its node is down. The full tier never uses the warm cache.

The `changes` job in each workflow decides which of its jobs a run needs. Jobs are gated with `if:` rather than workflow-level `paths`, so every required check reports on every PR; skipped jobs report as passed, so `changes` itself must stay a required check: if it fails, everything behind it is skipped. Both workflows keep the job names the rulesets require (`changes`, `dev`, `boot` in `ci-dev.yml`; the `main` list in `ci.yml`).

Docs and markdown changes run nothing beyond `changes`.

A newer push to a PR cancels that PR's run in progress, and so does merging or closing it. Pushes to `main`, full runs on `dev` and release runs are never cancelled.

Locally, `make check` runs gofmt, the codebase-rule checks, golangci-lint, the unit tests and `helm lint`; `make vulncheck` runs govulncheck. The ClickHouse tests run locally with `make test-clickhouse-run` against the server `RELAY_TEST_CH_DSN` names.

## Full run on `dev`

`.github/workflows/full.yml` starts the full CI on `dev`'s HEAD after every push to `dev`, unless that commit already had a full run (green or red). It waits two minutes first, and a newer push cancels the waiting dispatcher, so a burst of merges starts one run, on the newest HEAD; a full run already started is never cancelled. Every three hours the schedule does the same as a backstop: GitHub drops or delays cron slots, and a push made with a workflow's own token starts no workflows. A green run moves the `ci-full-passed` tag to it and fast-forwards `release/next` there (see [Releasing](#releasing)). Both depend on the [CI app](#ci-app): a run is recorded only when it was dispatched with the app's token.

The `push` trigger runs `dev`'s copy of `full.yml`, while `schedule` and `workflow_run` run `main`'s. A change to the dispatcher's push handling therefore takes effect as soon as it lands on `dev`; the schedule and the `record` job change only when `main` next gets `dev`.

### CI app

GitHub fires no `workflow_run` event for a run started with a workflow's `GITHUB_TOKEN`, and starts no workflows for a pull request opened or pushed with it. So the actions whose follow-ups matter use an installation token of a GitHub App (Actions, Contents, Pull requests and Issues read and write, installed on this repository only), minted per job with only the permissions that job needs:

- `full.yml` dispatches `ci.yml` with it, so the finished run triggers `full.yml`'s `record` job.
- `scripts/release-pr.sh` moves `release/next` and creates or edits the release PR with it, so the PR's checks report without a manual "approve and run".
- the fixer's `gh` calls use it, so a fix PR gets `ci-dev.yml`'s checks on its own.

The `ci-full-passed` tag, the `fix-dev.yml` dispatch and the fixer's `git push` keep the workflow token: a dispatch starts its run with either token, and nothing reacts to the tag, the fixer's run finishing, or the push. Configuration: the `RELAY_CI_APP_ID` variable (the app's ID or client ID) and the `RELAY_CI_APP_KEY` secret (its private key). Without them every step falls back to the workflow token with a warning, and none of the follow-ups above fire.

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

### The fixer

A red full run on `dev` also starts `.github/workflows/fix-dev.yml`, once per failing commit. Claude, reached through relay, reproduces the failure with the failing test only, bisects the range, and takes exactly one outcome:

- a small fix or a `git revert` of the culprit, as a PR into `dev` labelled `dev-fix` that names the culprit and the root cause. Before pushing it reruns the failing test or check and builds and vets the packages it touched; the full suite is left to the PR's CI. The PR is opened with the [CI app](#ci-app)'s token, so `ci-dev.yml` runs on it; as a fallback for when the app isn't configured, the fixer also dispatches both workflows on its branch: `ci.yml` tests the fix, and `ci-dev.yml`, with every part on, reports the checks `dev` requires. `ci-dev.yml` is dispatched by id, since a workflow not yet on `main` can't be dispatched by name.
- an issue labelled `needs-decision` with the failure, the culprit, the root cause and the options, when the fix isn't small or needs a design decision. Nothing else.

It never merges. A failure on a commit that came from a `dev-fix` PR is not retried; the guard opens a `needs-decision` issue for a human instead. A run is skipped when `fix/dev-<sha>`, an open `dev-fix` PR or an open `needs-decision` issue already names the commit. Its tokens have contents, pull-request and issue write and at most `actions` read: no `actions` write and no `workflows` permission, so it can neither start runs nor change `.github/workflows/`.

Claude Code runs as a plain `claude -p --bare` at a pinned version (`CLAUDE_CODE_VERSION` in the workflow): only the Bash, Read and Edit tools, no MCP servers, hooks or plugins, no settings or `CLAUDE.md` picked up from the checked-out branch, and one model for every call. Bash is unrestricted, since the job runs on a throwaway GitHub-hosted runner and the token's scope is the boundary; the few denies (merging, starting workflows, force pushes, edits under `.github/` and `.claude/`) encode policy. Limits: 30 minutes, 80 turns, $5 per run, 16000 output tokens per request. With `CI_POOL=cluster` the fix job runs on the `relay-ci-integration` pool: Postgres, Valkey and ClickHouse come from the pod's sidecars, Node and `gh` are installed by the job, and the CI egress proxy allows the relay host in `DEV_FIXER_BASE_URL`.

Configuration: the `DEV_FIXER_RELAY_KEY` secret (a relay key dedicated to the fixer, on a policy with a tight token rate limit), the `DEV_FIXER_BASE_URL` variable (relay's Anthropic-shape base URL), and optionally `DEV_FIXER_MODEL` and `DEV_FIXER_MAX_OUTPUT_TOKENS`. Without the key, the run reports that and stops. Every outcome goes to the run summary, and to Telegram when `RELEASE_TELEGRAM_TOKEN` (secret) and `RELEASE_TELEGRAM_CHAT` (variable) are set at repository level.

`full.yml` dispatches it on `dev` (`--ref dev`), so it runs in dev's context. A workflow file that isn't on `main` can't be dispatched by name; to try a change on another branch, register it there once (a push run whose jobs skip), then dispatch by id: `gh api -X POST repos/{owner}/{repo}/actions/workflows/<id>/dispatches -f ref=<branch> -f 'inputs[sha]=…' -f 'inputs[run_id]=…' -f 'inputs[base]=<branch>'`.

## AI review of `dev`

Every day `.github/workflows/review-dev.yml` reviews the code that changed on `dev` since the `ai-reviewed` tag (or, before the first run, since the latest release). The schedule only dispatches the workflow on `dev`, so the review runs in dev's context.

- `plan` splits the range into units with `scripts/review-units.sh`: one per area (a Go package; the `auth`, `sdk`, `jobq` and `secret` modules as one each; `migrations/`, `.github/` and `chart/`), large areas split by file, and one approach unit per PR that touched reviewed code.
- `review` runs one unit per matrix job through `scripts/review-run.sh`: a bare `claude -p` with only Read and read-only Bash, no GitHub token, and a final answer validated against `scripts/review-schema.json`. Area units get the changed files, the area's diff and the PRs' descriptions; approach units judge one PR's approach (`fine` unless an alternative wins on a named, concrete criterion). Then `cross` makes one pass over the interactions between areas.
- `report` (`scripts/review-report.sh`) files the issues. A high- or medium-severity finding with at least medium confidence, or one that needs a design call (`needs-decision`), gets an issue labelled `ai-review` plus `severity:<level>`. If an open `ai-review` issue already covers the same file and kind, it gets a comment instead. Everything else goes into one digest issue per run, along with the approach verdicts and the units that didn't complete. The tag moves only when every unit completed.

To run a review now, or on another range: `gh workflow run review-dev.yml --ref dev [-f from=<sha>] [-f to=<sha>] [-f dry_run=true]`. A dry run prints the issues and the tag move instead of making them.

Configuration: the `AI_REVIEW_RELAY_KEY` secret (a relay key of its own, so review spend shows separately) and the `AI_REVIEW_BASE_URL` variable. `AI_REVIEW_MODEL` (areas and approach) and `AI_REVIEW_CROSS_MODEL` both default to Opus. `AI_REVIEW_MAX_PARALLEL` (default 4) caps the concurrent reviews. Without the key the run plans, says so, and opens nothing.

## Releasing

`release/next` follows the last `dev` commit whose full run passed, and there is always one open PR `release/next` → `main`. After each green full run, `scripts/release-pr.sh` (from `full.yml`) fast-forwards the branch and rewrites that PR's description: the commits since the latest release grouped as draft release notes, a link to the green run, and a checklist of what needs coordinating (a `versions.env` change, new migrations, control API code changes that may need a UI release). The PR runs no CI of its own: its head already has a green full run, and the required checks match by commit. Nothing merges or tags automatically. `DRY_RUN=1 scripts/release-pr.sh <sha> <run-url>` prints the description without touching anything.

1. Work through the checklist in the release PR.
2. Merge it with a merge commit. That creates a new commit on `main`.
3. Check that `dev` and `release/next` still exist (`git ls-remote origin dev release/next`). An admin merge bypasses the bypassable rulesets, and the repository deletes merged head branches; the no-bypass ruleset below is what keeps them.
4. Wait for CI on that push to `main` to go green.
5. Tag: `make release` (patch), `make release-minor` or `make release-major`. The target checks that CI passed for `origin/main`'s HEAD, then tags and pushes.

Releasing without waiting for the next green run means opening a PR from `dev` into `main` by hand; it runs the full tier itself.

The tag starts `.github/workflows/release.yml`. It doesn't re-run the tests: it refuses to publish unless CI passed for the push of the tagged commit to `main`. It builds the images (`wyolet/relay:<version>`, `:latest` lean, `:standalone` all-in-one, and `wyolet/catalog-server:<version>`), fails on fixable HIGH/CRITICAL vulnerabilities, pushes them to Docker Hub and GHCR (catalog-server to GHCR only) with signed provenance and SBOM attestations, publishes the Helm chart to `oci://ghcr.io/wyolet/charts/relay` (chart version = image version), and creates the GitHub release with the SBOMs attached. [`SECURITY.md`](../SECURITY.md) shows how to verify the attestations.

To exercise the pipeline without publishing: `gh workflow run release.yml --ref <branch>` (`dry_run` defaults to true).

## Hotfixes

A hotfix is a PR into `main`, released like any other `main` commit. Afterwards, merge `main` back into `dev` with a PR, also as a merge commit.

## Pinned UI and catalog

`versions.env` pins the admin UI and the catalog the image bundles.

- `make ui-pin UI_VERSION=vX.Y.Z` downloads the relay-ui release tarball, verifies its signed attestation with `gh attestation verify`, and writes the version and the tarball's sha256. The image build, `make ui-fetch` and the boot smoke refuse a tarball that doesn't match the hash; a bare `docker build` needs both `UI_VERSION` and `UI_SHA256` as build args. `make image-assets-check` shows the check failing on a wrong or empty hash.
- `make catalog-pin CATALOG_VERSION=vX.Y.Z` downloads the relay-catalog release's data tarball, verifies its signed attestation and its published `.sha256`, and writes the version and the tarball's sha256. The image build and the boot smoke refuse a tarball that doesn't match `CATALOG_SHA256`; a bare `docker build` needs both `CATALOG_REF` and `CATALOG_SHA256` as build args. Only release tags from v0.1.15 publish a data tarball.
- Base images in the `Dockerfile` are pinned as `tag@sha256:digest`; Renovate updates the digests and opens its PRs against `dev`.

## Branch rules

| | `dev` | `main` |
|---|---|---|
| Force push | blocked | blocked |
| Changes by | PR only | PR only |
| Merge methods | squash, merge commit | merge commit, squash |
| Required checks | `changes`, `dev`, `boot` | `changes`, `lint`, `test`, `integration`, `clickhouse`, `chart`, `boot`, `fuzz`, `bench`, `govulncheck`, `trivy`, `coverage` |
| Branch must be up to date | no | no |

These come from the `dev` and `main` rulesets, which admins can bypass. An admin merge therefore skips them, including their deletion block.

Deletion and force pushes of `dev`, `main` and `release/next` are blocked by the `long-lived-branches` ruleset, which has no bypass actors. Without it, the repository's automatic deletion of merged head branches removes `dev` when an admin merges the release PR. Removing that protection takes editing the ruleset itself.

`release/next` gets no PR-only rule: `full.yml` moves it with a direct, fast-forward-only ref update, which such a rule would reject.

Release tags (`v*.*.*`, and the module tags `sdk/`, `auth/`, `jobq/` and `secret/` + `v*.*.*`) are covered by the `release-tags` ruleset, which has no bypass actors. A tag can be created but never moved or deleted, so a published version always names the commit it was built from. A bad release gets a new patch version. `ci-full-passed` is not covered, because it has to move.
