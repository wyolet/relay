# Wyolet Relay Go SDK

`github.com/wyolet/relay/sdk` is the Go client for [Wyolet Relay](https://github.com/wyolet/relay). You build one canonical request and send it through a relay or straight to a provider. The module also holds relay's canonical protocol, the vendor translators and the public model catalog, so you can use it without running a relay.

```bash
go get github.com/wyolet/relay/sdk@latest
```

Requires Go 1.27 or later.

## Modes

`client.ForMode` picks where a call goes:

| Mode | The call goes to | Relay records it |
|---|---|---|
| `proxy` | Relay, with your relay key | yes, as a routed request |
| `telemetry` | the provider, with your provider key | yes, reported over OTLP with your relay key |
| `direct` | the provider, with your provider key | no |

The mode comes from `client.WithMode`, then `WR_MODE`. Without either, the client uses `proxy` when `WR_BASE_URL` and `WR_API_KEY` are set and `direct` otherwise. Telemetry is never chosen by default. In `telemetry` and `direct` mode, the model reference is resolved against the catalog embedded in the SDK, and the provider key is read from `OPENAI_API_KEY`, `ANTHROPIC_API_KEY` or `GEMINI_API_KEY`.

```go
c, err := client.ForMode("gpt-4o")
if err != nil {
	log.Fatal(err)
}
defer c.Close() // in telemetry mode, sends the calls still queued

resp, err := c.Generate(ctx, &v1.Request{
	Input: []v1.Item{&v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.TextPart{Text: "Hello"}}}},
})
if err != nil {
	log.Fatal(err)
}
if cost, ok := resp.Cost(); ok { // priced from the catalog; not set in proxy mode
	fmt.Printf("cost: $%.6f\n", cost)
}
```

In `telemetry` and `direct` mode, `Models` and `Catalog` read the latest release from `https://catalog.wyolet.com`. Set `WR_CATALOG_URL` to use another catalog server, or to `off` to use only the embedded catalog.

## Packages

| Package | Contents |
|---|---|
| `client` | `ForMode`, `Relay`, `RelayWS`, `For`, and explicit `OpenAI` / `Anthropic` / `Gemini` constructors; `Generate`, `GenerateStream`, `Cost` |
| `v1` | the canonical request, response and stream event types, and the `Translator` interface |
| `adapters/openai`, `adapters/anthropic`, `adapters/gemini` | translators between each vendor's wire shapes and `v1` |
| `catalog`, `catalogsource` | the embedded catalog, model reference resolution, per-binding cost; loading newer catalog releases |
| `telemetry` | the OTLP emitter used by `telemetry` mode (OpenTelemetry GenAI conventions) |
| `provider`, `host`, `model` | read-only catalog discovery views |

## Stability

The SDK is v0: its API may change between minor versions. Releases are tagged `sdk/vX.Y.Z` in the relay repository.

The module imports nothing from the relay server module, so it can be vendored on its own.

## Documentation

- [Go SDK guide](https://docs.wyolet.com/go-sdk)
- [API reference on pkg.go.dev](https://pkg.go.dev/github.com/wyolet/relay/sdk)
- License: [Apache-2.0](https://github.com/wyolet/relay/blob/main/LICENSE)
