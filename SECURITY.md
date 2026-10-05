# Security Policy

## Reporting a vulnerability

Do not report security vulnerabilities through public issues, discussions, or pull requests.

Use GitHub's private vulnerability reporting: the repository's **Security** tab → **Report a vulnerability**. Include a description, reproduction steps, affected versions, and the impact you observed.

What to expect:

- Acknowledgement within 3 business days.
- An initial assessment (confirmed, needs more information, or not a vulnerability) within 10 business days.
- Fixes for confirmed issues ship in a patch release of the supported line, followed by a GitHub security advisory. We aim to release within 90 days of the report and agree the disclosure date with you; reporters are credited unless they ask not to be.

## Supported versions

Security fixes go into the latest minor release line (currently `0.10.x`) and `main`. Older lines are not patched; upgrade to receive fixes. See [upgrading](docs/concepts/upgrade.mdx).

## Scope

In scope:

- Credential exposure: upstream provider keys, the master key, the admin token, relay-signed tokens, session cookies, secret-backend credentials.
- Authentication or authorization bypass on the control plane (`/api`) or the inference plane, including RBAC scope escapes between teams and projects.
- Upstream request forgery (SSRF) or header/secret leakage through the inference, proxy, or catalog paths.
- Leakage of request or response payloads across principals, or into logs.
- Supply-chain issues in published images, the Helm chart, or the release pipeline.

Out of scope:

- Actions an authenticated caller can take by design. Under `RELAY_AUTHZ=single` every authenticated control-plane user is fully trusted; report authorization issues against `rbac`.
- Findings that require the admin token, the master key, or write access to the database, process environment, or host.
- Denial of service through request volume. Rate limits and budgets are operator configuration.
- Vulnerabilities in upstream providers or third-party dependencies with no relay-specific impact (report those upstream; we track dependency advisories in CI).

## Deployment security model

These are the trust assumptions the code is written against. A deployment that breaks one is outside what relay can defend.

- **The master key (`RELAY_MASTER_KEY`) and admin token (`RELAY_ADMIN_TOKEN`) are root credentials.** The master key decrypts every stored upstream credential; the admin token bypasses authorization in every mode. Provide both from a secret store, never commit them, and rotate the admin token if it is exposed. On the lean image, leaving `RELAY_ADMIN_TOKEN` empty disables the break-glass path (the standalone image generates one on first boot).
- **Authorization mode.** `single` (the default) is for one operator or a fully trusted team behind a network boundary. Any deployment where control-plane users should not all be administrators must run `RELAY_AUTHZ=rbac`. See [authentication](docs/concepts/auth.mdx).
- **Datastores are trusted.** Postgres holds configuration and encrypted credentials; Valkey holds sessions, rate-limit counters, and key health. Anyone who can write to either can impersonate users or change routing. Keep them on a private network, require authentication, and restrict access with network policy.
- **The control plane is not meant for the open internet without a front door.** Serve it over TLS with `RELAY_COOKIE_SECURE=true` (the default on the lean image; the standalone image defaults it to `false` for local use), and expose only the inference endpoints publicly where you can.
- **Payload logging stores request and response bodies.** Enable it only where storing prompts and completions is acceptable, and treat the payload store with the same care as the traffic itself.

## Security checks in CI

Every change runs `govulncheck` over all Go modules and Trivy over dependencies, configuration, and secrets; both also run weekly against `main`. Release images are scanned before they are published, and a release with fixable high or critical vulnerabilities does not ship.

## Verifying releases

Each release image carries a signed SLSA build provenance attestation and a CycloneDX SBOM attestation, made by the release workflow and stored on GitHub (and alongside the image on ghcr.io). Docker Hub serves the same digests, so the commands work for `docker.io/wyolet/relay` too. They need the GitHub CLI.

```bash
# Provenance: built by wyolet/relay's release workflow from the tagged commit.
gh attestation verify oci://ghcr.io/wyolet/relay:<version> --repo wyolet/relay \
  --signer-workflow wyolet/relay/.github/workflows/release.yml \
  --source-ref refs/tags/v<version>

# SBOM attestation, and the SBOM itself.
gh attestation verify oci://ghcr.io/wyolet/relay:<version> --repo wyolet/relay \
  --predicate-type https://cyclonedx.org/bom \
  --format json --jq '.[0].verificationResult.statement.predicate'

# An SBOM file downloaded from the GitHub release.
gh attestation verify relay-<version>-lean.cdx.json --repo wyolet/relay
```

The standalone image is `ghcr.io/wyolet/relay:<version>-standalone`. Releases are built on self-hosted runners: an attestation proves which workflow and commit produced an image, not that the build ran isolated from other workloads on that infrastructure.
