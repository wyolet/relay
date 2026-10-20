package anthropic

import "github.com/wyolet/relay/sdk/internal/sse"

// NewSummaryStream implements v1.SummaryStream.
func (AnthropicTranslator) NewSummaryStream() (func([]byte) ([]byte, error), func([]byte) bool) {
	s := &anthropicToCanonicalStream{}
	return s.translate, anthropicAffectsSummary
}

// anthropicAffectsSummary reports whether an Anthropic frame can change the
// stream's Summary. content_block_delta only grows the open block's buffers
// and ping does nothing, so those are skipped. Every other event passes:
// message_start and message_delta carry usage, stop reason and service tier,
// message_stop emits the completion, and content_block_start/stop decide
// whether a structured-output block pins the stop reason.
func anthropicAffectsSummary(frame []byte) bool {
	for sc := sse.NewScanner(frame); sc.Next(); {
		switch string(sc.Event()) {
		case "content_block_delta", "ping", "":
		default:
			return true
		}
	}
	return false
}
