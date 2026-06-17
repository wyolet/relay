<div align="center">
  <img src=".github/assets/logo.png" alt="Wyolet Relay" width="104">

  <h1>Wyolet Relay</h1>

  <p>
    <strong>One endpoint in front of every LLM provider.</strong><br>
    Self-hosted, bring-your-own-keys, built for scale.
  </p>

  <p>
    <a href="https://docs.wyolet.com"><img src="https://img.shields.io/badge/docs-docs.wyolet.com-7c3aed" alt="Docs"></a>
    <a href="LICENSE"><img src="https://img.shields.io/badge/license-Apache--2.0-7c3aed" alt="License: Apache-2.0"></a>
    <a href="https://hub.docker.com/r/wyolet/relay"><img src="https://img.shields.io/badge/docker-wyolet%2Frelay-2496ED?logo=docker&logoColor=white" alt="Docker image"></a>
    <a href="https://discord.gg/KUhJ8X3w"><img src="https://img.shields.io/badge/discord-join-5865F2?logo=discord&logoColor=white" alt="Discord"></a>
    <a href="https://x.com/wyolethq"><img src="https://img.shields.io/badge/follow-%40wyolethq-000000?logo=x&logoColor=white" alt="Follow on X"></a>
  </p>

  <p>
    <a href="https://docs.wyolet.com/quickstart"><strong>Quickstart</strong></a> ·
    <a href="https://docs.wyolet.com">Docs</a> ·
    <a href="https://discord.gg/KUhJ8X3w">Discord</a> ·
    <a href="https://x.com/wyolethq">X</a> ·
    <a href="https://www.linkedin.com/company/wyolet">LinkedIn</a> ·
    <a href="https://bsky.app/profile/wyolet.bsky.social">Bluesky</a> ·
    <a href="https://www.reddit.com/r/wyolet">Reddit</a>
  </p>
</div>

---

Wyolet Relay puts a single **OpenAI- and Anthropic-compatible** endpoint in front
of every provider you use. Pool your own API keys for automatic failover and
higher effective rate limits, see exactly what every request costs, and run the
whole thing on your own infrastructure — a drop-in for the SDK code you already
have.

## Quickstart

Start a full relay — API, admin UI, database, and a pre-seeded model catalog — in
one command:

```bash
docker run -p 8080:8080 -p 8081:8081 wyolet/relay:standalone
```

Open the admin UI at **http://localhost:8081**, then let the setup wizard walk you
through adding a provider key and minting a relay key. Now call it like the
OpenAI API:

```bash
curl http://localhost:8080/openai/v1/chat/completions \
  -H "Authorization: Bearer <your-relay-key>" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}'
```

That's it. The full walkthrough, configuration, and production deployment guides
live at **[docs.wyolet.com](https://docs.wyolet.com)**.

## Features

- **One API, every provider.** OpenAI- and Anthropic-shape endpoints in front of
  OpenAI, Anthropic, Bedrock, Vertex, Azure, Ollama, Groq — anything speaking
  either wire format. No code changes to switch upstreams.
- **Disposable, rate-limited keys.** Mint relay keys scoped to whatever limits you
  set. Hand them out freely — even if one leaks, the damage is capped at those
  limits and your real provider keys are never exposed.
- **Pool accounts and providers.** Combine many keys, accounts, or providers into
  one pool behind a single endpoint. Relay load-balances and fails over across
  them, so per-account rate limits stop being your ceiling.
- **Per-key access control.** Decide exactly which models and providers each relay
  key may reach — allow or deny at the key level via policies.
- **400+ models, open catalog.** Ships knowing 400+ models out of the box, and the
  [catalog](https://github.com/wyolet/relay-catalog) is open and extensible — we
  add hosts and models on demand.
- **Batch processing** _(in progress)_. Batch requests against any provider — Relay
  simulates batching where there's no native API, and routes through the native
  one (OpenAI, Gemini, Anthropic) where it exists, passing the cost discount
  straight through. Configure a webhook to fire when a batch completes.
- **Proxy mode.** Point Relay at a provider with your own upstream key and use it
  as a transparent proxy — no policy enforcement, just full usage, cost, and
  payload logging.
- **Usage & cost tracking.** Every request is metered and stored in Postgres or
  ClickHouse. Optional full request/response payload capture (off by default).
- **Metrics & logs.** First-class Prometheus `/metrics` and structured JSON logs.
  (OpenTelemetry tracing is on the way.)
- **Self-hostable, built for scale.** Bring your own keys; nothing phones home.
  Sub-2 ms added latency, thousands of requests/sec per pod, Kubernetes-native.

## How it works

Relay runs two listeners: a **data plane** that accepts your inference requests
and a **control plane** that serves the admin UI and API. Each request is
authenticated by a relay key, matched to a **policy** that decides which models
and providers it may reach, rate-limited, and routed to a healthy upstream key
from the **pool** — then streamed straight back to you. Provider, model, and
pricing data comes from an open, versioned
[catalog](https://github.com/wyolet/relay-catalog), so a fresh container already
knows hundreds of models.

Want the full architecture, API reference, and configuration?
→ **[docs.wyolet.com](https://docs.wyolet.com)**

## Commercial support

Relay is Apache-2.0 — free to use, self-host, and build on, in commercial and
closed-source products alike. Want managed hosting, enterprise builds, or priority
support instead of running it yourself? We're happy to talk:
**[business@wyolet.com](mailto:business@wyolet.com)**.

## Contributing

Issues and pull requests are welcome. See [CONTRIBUTING.md](CONTRIBUTING.md) for
the build, test, and PR workflow.

## License

[Apache-2.0](LICENSE). Use it in anything — commercial, closed-source, hosted, or
embedded — no copyleft strings attached. See
[Commercial support](#commercial-support) if you'd rather we run or support it for
you.
