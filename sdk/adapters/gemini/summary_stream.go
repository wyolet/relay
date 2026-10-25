package gemini

import (
	"github.com/wyolet/relay/sdk/internal/jsonscan"
	"github.com/wyolet/relay/sdk/internal/sse"
)

// NewSummaryStream implements v1.SummaryStream.
func (GeminiTranslator) NewSummaryStream() (func([]byte) ([]byte, error), func([]byte) bool) {
	s := &geminiToCanonicalStream{}
	return s.translate, geminiAffectsSummary
}

// geminiAffectsSummary reports whether a Gemini frame can change the stream's
// Summary. Every chunk repeats usageMetadata, but translate only emits the
// completion, with that frame's usage, on a finishReason, on a first candidate
// without content, or on a blocked prompt; a functionCall part flips the
// finish to tool_calls for the rest of the stream. So a frame is skipped only
// when it has exactly one candidates array whose first candidate has one
// non-null content, no finishReason and no functionCall part, and no
// promptFeedback or error. Anything the scan cannot read passes.
func geminiAffectsSummary(frame []byte) bool {
	for sc := sse.NewScanner(frame); sc.Next(); {
		if geminiDataAffectsSummary(sc.Data()) {
			return true
		}
	}
	return false
}

func geminiDataAffectsSummary(data []byte) bool {
	candidates := 0
	m := jsonscan.Object(data)
	for m.Next() {
		k, v := m.Key(), m.Value()
		switch {
		case jsonscan.KeyIs(k, "promptFeedback"), jsonscan.KeyIs(k, "error"):
			if !jsonscan.IsNull(v) {
				return true
			}
		case jsonscan.KeyIs(k, "candidates"):
			candidates++
			if jsonscan.IsNull(v) || firstCandidateAffectsSummary(v) {
				return true
			}
		}
	}
	return m.Err() || candidates != 1
}

func firstCandidateAffectsSummary(candidates []byte) bool {
	e := jsonscan.Array(candidates)
	if !e.Next() {
		return true
	}
	contents := 0
	m := jsonscan.Object(e.Value())
	for m.Next() {
		k, v := m.Key(), m.Value()
		switch {
		case jsonscan.KeyIs(k, "finishReason"):
			if !jsonscan.NullOrEmpty(v) {
				return true
			}
		case jsonscan.KeyIs(k, "content"):
			contents++
			if jsonscan.IsNull(v) || contentHasFunctionCall(v) {
				return true
			}
		}
	}
	return m.Err() || contents != 1
}

func contentHasFunctionCall(content []byte) bool {
	m := jsonscan.Object(content)
	for m.Next() {
		if !jsonscan.KeyIs(m.Key(), "parts") || jsonscan.IsNull(m.Value()) {
			continue
		}
		parts := jsonscan.Array(m.Value())
		for parts.Next() {
			p := jsonscan.Object(parts.Value())
			for p.Next() {
				if jsonscan.KeyIs(p.Key(), "functionCall") && !jsonscan.IsNull(p.Value()) {
					return true
				}
			}
			if p.Err() {
				return true
			}
		}
		if parts.Err() {
			return true
		}
	}
	return m.Err()
}
