package openai

import (
	"encoding/json"

	"github.com/wyolet/relay/sdk/internal/jsonscan"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// SummaryFromJSONEnds implements v1.JSONEndsSummarizer. The finish reason is choices[0]'s, and telling that choice from the last one takes walking every choice, so a body is read only when the window covers it whole; even then only the members the Summary needs are decoded.
func (CCTranslator) SummaryFromJSONEnds(body []byte) (v1.Summary, bool) {
	if len(body) > jsonscan.EndsWindow {
		return v1.Summary{}, false
	}
	var (
		usage   *Usage
		tier    string
		choices []byte
	)
	m := jsonscan.Object(body)
	for m.Next() {
		var err error
		switch k, v := m.Key(), m.Value(); {
		case jsonscan.KeyIs(k, "usage"):
			err = json.Unmarshal(v, &usage)
		case jsonscan.KeyIs(k, "service_tier"):
			err = json.Unmarshal(v, &tier)
		case jsonscan.KeyIs(k, "choices"):
			choices = v
		}
		if err != nil {
			return v1.Summary{}, false
		}
	}
	if m.Err() {
		return v1.Summary{}, false
	}
	finish, ok := ccFirstChoiceFinish(choices)
	if !ok {
		return v1.Summary{}, false
	}
	return v1.Summary{Tokens: ccUsageToCanonical(usage), FinishReason: finish, ServiceTier: tier}, true
}

// ccFirstChoiceFinish is ParseResponse's finish reason read from the raw choices array: choices[0].finish_reason, lifted to refusal when its message carries one, stop when there is no choice.
func ccFirstChoiceFinish(choices []byte) (v1.FinishReason, bool) {
	if choices == nil || jsonscan.IsNull(choices) {
		return v1.FinishReasonStop, true
	}
	e := jsonscan.Array(choices)
	if !e.Next() {
		return v1.FinishReasonStop, !e.Err()
	}
	var (
		reason  string
		refusal *string
	)
	if first := e.Value(); !jsonscan.IsNull(first) {
		m := jsonscan.Object(first)
		for m.Next() {
			switch k, v := m.Key(), m.Value(); {
			case jsonscan.KeyIs(k, "finish_reason"):
				if json.Unmarshal(v, &reason) != nil {
					return "", false
				}
			case jsonscan.KeyIs(k, "message"):
				if !ccMessageRefusal(v, &refusal) {
					return "", false
				}
			}
		}
		if m.Err() {
			return "", false
		}
	}
	_, finish, _ := ccFinishReasonToCanonical(reason)
	if refusal != nil && *refusal != "" && finish == v1.FinishReasonStop {
		finish = v1.FinishReasonRefusal
	}
	return finish, true
}

// ccMessageRefusal decodes a message's refusal members into refusal the way ChatResponseMessage would; a null message leaves it as is.
func ccMessageRefusal(msg []byte, refusal **string) bool {
	if jsonscan.IsNull(msg) {
		return true
	}
	m := jsonscan.Object(msg)
	for m.Next() {
		if jsonscan.KeyIs(m.Key(), "refusal") && json.Unmarshal(m.Value(), refusal) != nil {
			return false
		}
	}
	return !m.Err()
}
