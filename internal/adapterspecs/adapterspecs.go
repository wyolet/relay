// Package adapterspecs builds the adapter.Spec for every wire shape relay serves. It is part of the composition root: with cmd/relay, the only server package that imports a vendor adapter, so app/ and pkg/ code and tests reach vendor translators through the specs it returns. Building the Registry and checking its wiring stay with the caller.
package adapterspecs

import (
	"github.com/wyolet/relay/app/adapter"
	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/pkg/adapters"
	"github.com/wyolet/relay/pkg/catalogkind/binding"
	"github.com/wyolet/relay/pkg/catalogkind/host"
	pkganthropic "github.com/wyolet/relay/sdk/adapters/anthropic"
	pkggemini "github.com/wyolet/relay/sdk/adapters/gemini"
	pkgopenai "github.com/wyolet/relay/sdk/adapters/openai"
	relayv1 "github.com/wyolet/relay/sdk/v1"
)

// Specs returns one built Spec per supported wire shape, fresh on every call.
func Specs() []*adapter.Spec {
	// Header names are canonical so the per-attempt Header.Set/Get don't
	// re-canonicalize them.
	openaiAuth := adapter.AuthStrategy{Header: "Authorization", Scheme: "Bearer"}
	anthropicAuth := adapter.AuthStrategy{
		Header:       "X-Api-Key",
		ExtraHeaders: map[string]string{"Anthropic-Version": "2023-06-01"},
	}
	geminiAuth := adapter.AuthStrategy{Header: "X-Goog-Api-Key"}
	// The response header naming the credential's organization, per each vendor's API reference; Gemini documents none, so its spec has no probe.
	openaiOrg := &adapter.OrgProbe{Path: "/v1/models", OrgHeader: "Openai-Organization"}
	anthropicOrg := &adapter.OrgProbe{Path: "/v1/models", OrgHeader: "Anthropic-Organization-Id"}
	// Gemini encodes the model and the sync/stream choice in the URL path
	// rather than the request body, so its upstream path is resolved per call.
	geminiUpstreamPath := func(model string, stream bool) string {
		if stream {
			return "/v1beta/models/" + model + ":streamGenerateContent?alt=sse"
		}
		return "/v1beta/models/" + model + ":generateContent"
	}

	specs := []*adapter.Spec{
		(&adapter.Spec{
			Name: adapters.OpenAI,
			InboundPaths: []adapter.InboundPath{
				{Path: "/openai/v1/chat/completions", OperationID: "openai_chat_completions", Summary: "Create a chat completion (OpenAI Chat Completions shape)"},
			},
			DefaultPath:   "/v1/chat/completions",
			Org:           openaiOrg,
			Auth:          openaiAuth,
			Translator:    pkgopenai.CCTranslator{},
			ExtractTokens: pkgopenai.ExtractTokens,
			StreamUsage:   &adapter.StreamUsageOptIn{Request: pkgopenai.RequestStreamUsage, IsUsageFrame: pkgopenai.IsUsageOnlyChunk},
			ParamPaths:    map[string]string{"temperature": "temperature", "top_p": "top_p"},
		}).Build(),
		(&adapter.Spec{
			Name: adapters.OpenAIResponses,
			InboundPaths: []adapter.InboundPath{
				{Path: "/openai/v1/responses", OperationID: "openai_responses_create", Summary: "Create a response (OpenAI Responses API)"},
			},
			DefaultPath:   "/v1/responses",
			Org:           openaiOrg,
			Auth:          openaiAuth,
			Translator:    pkgopenai.ResponsesTranslator{},
			ExtractTokens: pkgopenai.ExtractTokens,
			ParamPaths:    map[string]string{"temperature": "temperature", "top_p": "top_p"},
			UseHTTP1:      true,
			IsNativePath: func(b *binding.Binding, h *host.Host) bool {
				return b.Spec.Adapter == adapters.OpenAI && h.Meta.Name == "openai"
			},
		}).Build(),
		(&adapter.Spec{
			Name: adapters.OpenAIEmbeddings,
			InboundPaths: []adapter.InboundPath{
				{Path: "/openai/v1/embeddings", OperationID: "openai_embeddings_create", Summary: "Create embeddings (OpenAI-compatible)"},
			},
			DefaultPath:   "/v1/embeddings",
			Org:           openaiOrg,
			Auth:          openaiAuth,
			BytePass:      true,
			ExtractTokens: pkgopenai.ExtractTokens,
		}).Build(),
		(&adapter.Spec{
			Name: adapters.Anthropic,
			InboundPaths: []adapter.InboundPath{
				{Path: "/anthropic/v1/messages", OperationID: "anthropic_messages", Summary: "Create a message (Anthropic Messages shape)"},
			},
			DefaultPath:   "/v1/messages",
			CountPath:     "/v1/messages/count_tokens",
			Org:           anthropicOrg,
			Auth:          anthropicAuth,
			Translator:    pkganthropic.AnthropicTranslator{},
			ExtractTokens: pkganthropic.ExtractTokens,
			ParamPaths:    map[string]string{"temperature": "temperature", "top_p": "top_p", "top_k": "top_k"},
			Overloaded:    &adapter.ErrorResponse{Status: pkganthropic.StatusOverloaded, Body: pkganthropic.OverloadedError(httpapi.OverloadMessage)},
		}).Build(),
		// Gemini native shape — upstream-only for now (HostBinding.Adapter:
		// gemini), reachable via the canonical / OpenAI / Anthropic inbound
		// shapes through the cross-shape chain. No InboundPaths yet: native
		// inbound Gemini puts the model in the URL path, which the body-based
		// minimal parse doesn't extract — a separate follow-up.
		(&adapter.Spec{
			Name:           adapters.Gemini,
			UpstreamPathFn: geminiUpstreamPath,
			Auth:           geminiAuth,
			Translator:     pkggemini.GeminiTranslator{},
			ExtractTokens:  pkggemini.ExtractTokens,
			ParamPaths: map[string]string{
				"temperature": "generationConfig.temperature",
				"top_p":       "generationConfig.topP",
				"top_k":       "generationConfig.topK",
			},
		}).Build(),
		// Canonical shape — relay's own protocol (pkg/relay/v1), served at /v1.
		// Inbound-only: callers POST canonical, relay routes + translates
		// canonical→upstream-vendor via the upstream's translator, returns
		// canonical. The identity translator makes the generic cross-shape
		// dispatch chain handle it with no special-casing.
		(&adapter.Spec{
			Name: adapters.Canonical,
			InboundPaths: []adapter.InboundPath{
				{Path: "/v1/generate", OperationID: "generate", Summary: "Generate (relay canonical shape)"},
			},
			Translator: relayv1.IdentityTranslator{},
		}).Build(),
	}
	return specs
}
