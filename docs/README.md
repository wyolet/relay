# Wyolet Relay — Docs

The public documentation site for Wyolet Relay, built with [Mintlify](https://mintlify.com). It lives in this repo under `docs/`; the Mintlify project is configured to build from this directory. Navigation and page order are defined in `docs.json`.

## Local preview

```bash
npm i -g mint
cd docs
mint dev
```

Opens at `http://localhost:3000`.

## Deployment

Hosted by Mintlify. Point the Mintlify project at this repo with the content directory set to `docs/`. Edits merged to the default branch publish automatically.
