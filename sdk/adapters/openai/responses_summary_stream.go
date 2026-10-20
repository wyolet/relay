package openai

import "github.com/wyolet/relay/sdk/internal/sse"

// NewSummaryStream implements v1.SummaryStream.
func (ResponsesTranslator) NewSummaryStream() (func([]byte) ([]byte, error), func([]byte) bool) {
	s := &responsesToCanonicalStream{}
	return s.translate, responsesAffectsSummary
}

// responsesAffectsSummary reports whether a Responses frame can change the
// stream's Summary. generation.completed is built from the terminal event's
// own response object (completed, incomplete, failed) and nothing the stream
// accumulated, so every other event is skipped; error passes as well.
func responsesAffectsSummary(frame []byte) bool {
	for sc := sse.NewScanner(frame); sc.Next(); {
		switch string(sc.Event()) {
		case ResponsesEventCompleted, ResponsesEventIncomplete, ResponsesEventFailed, ResponsesEventError:
			return true
		}
	}
	return false
}
