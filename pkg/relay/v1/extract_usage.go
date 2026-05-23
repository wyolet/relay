package v1

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"

	"github.com/wyolet/relay/pkg/usage"
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
	if tr == nil || len(body) == 0 {
		return nil, nil
	}
	body, err := maybeUngzip(body)
	if err != nil {
		return nil, err
	}

	if !looksLikeSSE(body) {
		resp, err := tr.ParseResponse(body)
		if err != nil || resp == nil {
			return nil, nil
		}
		return resp.Usage, nil
	}

	return extractUsageFromSSE(tr, body), nil
}

// extractUsageFromSSE walks a vendor SSE body through the translator's
// to-canonical stream factory and harvests Usage from the terminal
// generation.completed event. Errors per-frame are skipped (one bad
// chunk shouldn't lose the whole stream's usage).
func extractUsageFromSSE(tr Translator, body []byte) usage.Tokens {
	toCanon := tr.NewToCanonicalStream()
	if toCanon == nil {
		// Translator declares no stream transform — the wire IS canonical.
		// In that case the body itself is already canonical SSE; parse it
		// directly without round-tripping through the translator.
		return harvestUsageFromCanonicalSSE(body)
	}

	// Split vendor body on blank-line boundaries; each frame is one SSE
	// event. Feed frames one at a time to the closure so its per-stream
	// state (chunk reassembly, item accounting) stays consistent.
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	sc.Split(splitSSEFrames)

	var found usage.Tokens
	for sc.Scan() {
		frame := sc.Bytes()
		if len(frame) == 0 {
			continue
		}
		// The closure expects each chunk terminated by the SSE blank-line
		// separator. Re-append because the scanner stripped it.
		input := make([]byte, 0, len(frame)+2)
		input = append(input, frame...)
		input = append(input, '\n', '\n')

		canonChunk, err := toCanon(input)
		if err != nil || len(canonChunk) == 0 {
			continue
		}
		if u := harvestUsageFromCanonicalSSE(canonChunk); u != nil {
			found = u
		}
	}
	return found
}

// harvestUsageFromCanonicalSSE scans canonical SSE chunks and returns
// the Usage carried by a generation.completed event if present.
// Used both for translator-emitted canonical chunks and for bodies
// whose translator declares identity stream (wire IS canonical).
func harvestUsageFromCanonicalSSE(canon []byte) usage.Tokens {
	sc := bufio.NewScanner(bytes.NewReader(canon))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	sc.Split(splitSSEFrames)

	var found usage.Tokens
	for sc.Scan() {
		frame := sc.Bytes()
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
			found = ev.Usage
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

// splitSSEFrames is a bufio.Scanner SplitFunc that splits on the
// blank-line frame separator ("\n\n"). Each token is one SSE event
// (event: + data: lines), trailing separator stripped.
func splitSSEFrames(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.Index(data, []byte("\n\n")); i >= 0 {
		return i + 2, data[:i], nil
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}
