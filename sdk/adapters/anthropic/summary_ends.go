package anthropic

import (
	"bytes"
	"encoding/json"

	"github.com/wyolet/relay/sdk/internal/jsonscan"
	"github.com/wyolet/relay/sdk/internal/sse"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// SummaryFromJSONEnds implements v1.JSONEndsSummarizer. A Messages response puts stop_reason and usage after content, so both are read walking back from the end. The one content block that changes the Summary, the structured-output tool, is searched for by name instead.
func (AnthropicTranslator) SummaryFromJSONEnds(body []byte) (v1.Summary, bool) {
	if mayHoldStructuredOutput(body) {
		return v1.Summary{}, false
	}
	var usage, stop []byte
	m := jsonscan.ObjectFromEnd(body, len(body)-jsonscan.EndsWindow)
	for (usage == nil || stop == nil) && m.Next() {
		switch {
		case usage == nil && jsonscan.KeyIs(m.Key(), "usage"):
			usage = m.Value()
		case stop == nil && jsonscan.KeyIs(m.Key(), "stop_reason"):
			stop = m.Value()
		}
	}
	if (usage == nil || stop == nil) && m.Err() {
		return v1.Summary{}, false
	}
	var u anthropicFullUsage
	var reason string
	if usage != nil && json.Unmarshal(usage, &u) != nil {
		return v1.Summary{}, false
	}
	if stop != nil && json.Unmarshal(stop, &reason) != nil {
		return v1.Summary{}, false
	}
	_, finish, _ := anthropicStopReasonToCanonical(reason)
	return v1.Summary{Tokens: anthropicUsageToCanonical(&u), FinishReason: finish, ServiceTier: u.ServiceTier}, true
}

// SummaryFromStreamEnds implements v1.StreamEndsSummarizer. Outside the structured-output tool's blocks, only message_start, message_delta and message_stop move a stream's Summary, so those events are found by name and summarized alone.
func (AnthropicTranslator) SummaryFromStreamEnds(body []byte) (v1.Summary, bool) {
	if mayHoldStructuredOutput(body) {
		return v1.Summary{}, false
	}
	frames, ok := sse.EventFrames(body, "message_start", "message_delta", "message_stop")
	if !ok {
		return v1.Summary{}, false
	}
	s := v1.NewStreamSummarizer(AnthropicTranslator{})
	for _, f := range frames {
		s.Observe(f)
	}
	return s.Summary(), true
}

// mayHoldStructuredOutput reports whether body could name the structured-output tool, whose block pins the finish reason to stop: by its plain name, or through an escape that would hide the name from the search.
func mayHoldStructuredOutput(body []byte) bool {
	return bytes.Contains(body, []byte(structuredOutputToolName)) || jsonscan.HasLetterEscape(body)
}
