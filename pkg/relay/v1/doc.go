// Package v1 defines the relay's canonical protocol — the wire-neutral internal
// representation that all vendor adapters translate to and from.
//
// # What this package is
//
// This is the lowest layer in the relay's type hierarchy. It declares:
//   - Canonical request and response types (Request, Response).
//   - Discriminated union types for items (Item), parts (Part), tools (Tool),
//     and stream events (Event).
//   - The Translator interface that every vendor adapter must implement.
//   - Name and Registry for adapter registration.
//   - SSE framing helpers shared by all stream paths.
//
// # What this package is NOT
//
// This package knows nothing about specific vendors. It imports no app/, internal/,
// or pkg/adapters/* code. It is vendorable: external consumers can use the
// canonical types and Translator interface without pulling in any relay
// application code.
//
// # Design principles
//
// Symmetric input/output: items you receive in output[] can be spliced into the
// next request's input[] without transformation. No tool_call_id gymnastics.
//
// Typed discriminated unions: every polymorphic field uses a sealed interface.
// External packages cannot implement Item, Part, Tool, or Event, so type
// switches in adapters are exhaustive.
//
// No request-echo in responses: the Response carries only what was produced.
// Vendor adapters that require echo (e.g. OpenAI Responses) receive *Request
// explicitly through SerializeResponse.
//
// String-or-array input normalizes to array: wire allows "content": "hi" for
// terse single-text messages. Internal types always use []Part.
//
// Extensions envelope: anything vendor-specific that doesn't map cleanly across
// all vendors lives in extensions: map[string]json.RawMessage on Request and
// Response. No new top-level canonical field for vendor-specific features.
//
// Provider data: signed/encrypted vendor payloads (Anthropic thinking
// signatures, OpenAI encrypted_content) are carried on the relevant item as
// provider_data json.RawMessage. Round-tripped verbatim to the same vendor;
// dropped cross-vendor. Customers never construct it.
package v1
