# Wyolet Relay — Docs

The public documentation site for Wyolet Relay, built with
[Mintlify](https://mintlify.com). It lives in this repo under `docs/`; the
Mintlify project is configured to build from this directory.

## Local preview

```bash
npm i -g mint
cd docs
mint dev
```

Opens at `http://localhost:3000`.

## Layout

```
docs.json                 site config (theme, nav, branding)
introduction.mdx          landing page
quickstart.mdx            first request in ~2 min
clients.mdx               SDK / client setup
claude-code.mdx           Claude Code passthrough
configuration.mdx         env vars + runtime settings
concepts/
  architecture.mdx        two-plane overview
  catalog.mdx             providers/hosts/models/policies + aliases
  key-pooling.mdx         failover + breakers
  proxy-mode.mdx          transparent passthrough
  observability.mdx       usage + payload logging
  auth.mdx                relay keys vs sessions
  overlays.mdx            user catalog customization
reference/
  inference-api.mdx       data-plane endpoints
  control-api.mdx         admin CRUD + ops (mounted under /api)
  headers.mdx             X-WR-* request/response headers
troubleshooting.mdx       common errors
```

## Deployment

Hosted by Mintlify. Point the Mintlify project at this repo with the content
directory set to `docs/`. Edits merged to the default branch publish
automatically.
