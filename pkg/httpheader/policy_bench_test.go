package httpheader

import (
	"net/http"
	"testing"
)

// A stock SDK request as it reaches the inference edge.
var benchInbound = http.Header{
	"Accept":                      {"application/json"},
	"Accept-Encoding":             {"gzip, deflate, br"},
	"Anthropic-Beta":              {"prompt-caching-2024-07-31"},
	"Anthropic-Version":           {"2023-06-01"},
	"Authorization":               {"Bearer relay-key"},
	"Content-Length":              {"5321"},
	"Content-Type":                {"application/json"},
	"User-Agent":                  {"claude-cli/2.1.0 (external, cli)"},
	"X-Api-Key":                   {"relay-key"},
	"X-App":                       {"cli"},
	"X-Claude-Code-Session-Id":    {"sess_123"},
	"X-Stainless-Arch":            {"arm64"},
	"X-Stainless-Lang":            {"js"},
	"X-Stainless-Os":              {"MacOS"},
	"X-Stainless-Package-Version": {"0.60.0"},
	"X-Stainless-Retry-Count":     {"0"},
	"X-Stainless-Runtime":         {"node"},
	"X-Stainless-Timeout":         {"600"},
	"X-Wr-Client":                 {"claude-code"},
}

func BenchmarkStrip(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		Strip(benchInbound.Clone())
	}
}
