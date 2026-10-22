package v1

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"

	"github.com/wyolet/relay/sdk/usage"
)

// ExtractUsage decodes a vendor wire response body — sync JSON or SSE
// stream, optionally gzipped — into a canonical usage.Tokens map keyed
// by orthogonal meter dimensions (input, output, cache_read, …).
// Returns nil when the body carries no usage block (failed request,
// non-completion response, etc.); returns (nil, err) only for
// decompression failures we want to surface.
//
// Three layers of normalization:
//
//   - gzip: sniff magic bytes (0x1f 0x8b), decompress with stdlib.
//   - SSE: sniff `event:`/`data:` framing. Walk frames, convert each
//     vendor SSE frame to canonical via Translator.NewToCanonicalStream
//     (the closure handles per-stream state), capture Usage from the
//     terminal GenerationCompletedEvent.
//   - sync JSON: hand the body to Translator.ParseResponse, read
//     Response.Usage.
//
// The helper lets observers speak canonical without per-vendor
// knowledge. Vendor adapters keep their narrow Translator interface;
// gzip + SSE handling lives in the canonical layer where it can be
// shared across every observer that needs Usage.
func ExtractUsage(tr Translator, body []byte) (usage.Tokens, error) {
	s, err := ExtractSummary(tr, body)
	return s.Tokens, err
}

// Summary is the canonical post-flight view of a response body: token
// counts, the finish reason, and the served service tier. All are harvested
// in a single decode (one ParseResponse / one SSE walk) so observers don't
// double-parse.
type Summary struct {
	Tokens       usage.Tokens
	FinishReason FinishReason
	ServiceTier  string
}

func (s Summary) isEmpty() bool {
	return len(s.Tokens) == 0 && s.FinishReason == "" && s.ServiceTier == ""
}

// ExtractSummary decodes a vendor wire response body — sync JSON or SSE
// stream, optionally gzipped — into a canonical Summary. Returns the zero
// Summary when the body carries no completion block; returns an error
// only for decompression failures we want to surface. Same three-layer
// normalization as ExtractUsage (which is now a thin wrapper).
func ExtractSummary(tr Translator, body []byte) (Summary, error) {
	if tr == nil || len(body) == 0 {
		return Summary{}, nil
	}
	body, err := maybeUngzip(body)
	if err != nil {
		return Summary{}, err
	}

	if !looksLikeSSE(body) {
		if d, ok := tr.(SummaryDecoder); ok {
			if s, err := d.DecodeSummary(body); err == nil {
				return s, nil
			}
			return Summary{}, nil
		}
		resp, err := tr.ParseResponse(body)
		if err != nil || resp == nil {
			return Summary{}, nil
		}
		return Summary{Tokens: resp.Usage, FinishReason: resp.FinishReason, ServiceTier: resp.ServiceTier}, nil
	}

	return extractSummaryFromSSE(tr, body), nil
}

// SummaryDecoder is an optional companion to Translator for ExtractSummary on sync JSON bodies: DecodeSummary decodes only the members a response's Summary is built from, returning what ParseResponse would yield for them, or an error wherever ParseResponse fails on a member it reads. A body ParseResponse rejects only for a member the Summary does not read still yields its Summary.
type SummaryDecoder interface {
	DecodeSummary(body []byte) (Summary, error)
}

// extractSummaryFromSSE walks a vendor SSE body through the translator's
// to-canonical stream factory and harvests the Summary from the terminal
// generation.completed event. Errors per-frame are skipped (one bad
// chunk shouldn't lose the whole stream's usage).
func extractSummaryFromSSE(tr Translator, body []byte) Summary {
	toCanon, affectsSummary := summaryStream(tr)
	if toCanon == nil {
		// Translator declares no stream transform — the wire IS canonical.
		// In that case the body itself is already canonical SSE; parse it
		// directly without round-tripping through the translator.
		return harvestSummaryFromCanonicalSSE(body)
	}

	// Split vendor body on blank-line boundaries; each frame is one SSE
	// event. Feed frames one at a time to the closure so its per-stream
	// state (chunk reassembly, item accounting) stays consistent.
	var found Summary
	for rest := body; len(rest) > 0; {
		n, raw, _ := SplitSSEFrames(rest, true)
		if n == 0 {
			break
		}
		rest = rest[n:]
		frame := NormalizeSSELineEnds(raw)
		if len(frame) == 0 || (affectsSummary != nil && !affectsSummary(frame)) {
			continue
		}
		// The closure expects each chunk terminated by the SSE blank-line
		// separator. Re-append because the split stripped it.
		input := make([]byte, 0, len(frame)+2)
		input = append(input, frame...)
		input = append(input, '\n', '\n')

		canonChunk, err := toCanon(input)
		if err != nil || len(canonChunk) == 0 {
			continue
		}
		if s := harvestSummaryFromCanonicalSSE(canonChunk); !s.isEmpty() {
			found = s
		}
	}
	return found
}

// harvestSummaryFromCanonicalSSE scans canonical SSE chunks and returns
// the Summary carried by a generation.completed event if present.
// Used both for translator-emitted canonical chunks and for bodies
// whose translator declares identity stream (wire IS canonical).
func harvestSummaryFromCanonicalSSE(canon []byte) Summary {
	var found Summary
	for rest := canon; len(rest) > 0; {
		n, raw, _ := SplitSSEFrames(rest, true)
		if n == 0 {
			break
		}
		rest = rest[n:]
		frame := NormalizeSSELineEnds(raw)
		if len(frame) == 0 {
			continue
		}
		event, data, ok := ParseSSEChunk(frame)
		if !ok || event != EventGenerationCompleted {
			continue
		}
		var ev GenerationCompletedEvent
		if json.Unmarshal(data, &ev) != nil {
			continue
		}
		if len(ev.Usage) > 0 {
			found.Tokens = ev.Usage
		}
		if ev.FinishReason != "" {
			found.FinishReason = ev.FinishReason
		}
		if ev.ServiceTier != "" {
			found.ServiceTier = ev.ServiceTier
		}
	}
	return found
}

// maybeUngzip detects gzip magic bytes and returns the decompressed
// payload. Bodies without gzip framing pass through unchanged.
func maybeUngzip(body []byte) ([]byte, error) {
	if len(body) < 2 || body[0] != 0x1f || body[1] != 0x8b {
		return body, nil
	}
	gr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer gr.Close()
	return io.ReadAll(gr)
}

// looksLikeSSE sniffs the leading bytes for SSE framing.
func looksLikeSSE(body []byte) bool {
	head := body
	if len(head) > 256 {
		head = head[:256]
	}
	head = bytes.TrimLeft(head, " \t\r\n\xef\xbb\xbf")
	return bytes.HasPrefix(head, []byte("event:")) ||
		bytes.HasPrefix(head, []byte("data:"))
}
