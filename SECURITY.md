# Security Policy

## Reporting a vulnerability

**Please do not report security vulnerabilities through public GitHub
issues, discussions, or pull requests.**

Instead, use GitHub's private vulnerability reporting:

1. Go to the repository's **Security** tab.
2. Click **Report a vulnerability** under "Private vulnerability reporting".
3. Provide a description, reproduction steps, affected versions, and
   impact assessment.

We will acknowledge receipt within a few business days and keep you
informed as we work on a fix.

## Scope

Relay handles upstream provider credentials (encrypted at rest via the
master key, or resolved from external secret backends) and proxies LLM
traffic. Reports we're especially interested in:

- Credential exposure (HostKey leakage, master-key handling, secret
  resolution).
- Auth/authz bypass on the control or data plane.
- Header/secret leakage to upstream providers or in logs/payloads.
- SSRF, request smuggling, or injection in the proxy/dispatch path.
- Tenant or key-pool isolation failures.

## Supported versions

Until a stable release line is established, security fixes are applied to
the `main` branch. Pin to a tagged release and watch the repository for
advisories.

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

## Disclosure

We follow coordinated disclosure: we'll work with you on a fix and a
disclosure timeline, and credit you in the advisory unless you prefer to
remain anonymous.
