package openai

import (
	"bytes"

	"github.com/wyolet/relay/sdk/internal/jsonscan"
	"github.com/wyolet/relay/sdk/internal/sse"
)

// NewSummaryStream implements v1.SummaryStream.
func (CCTranslator) NewSummaryStream() (func([]byte) ([]byte, error), func([]byte) bool) {
	s := &ccToCanonicalStream{}
	return s.translate, s.affectsSummary
}

// affectsSummary reports whether a CC frame can change the stream's Summary.
// handleDone builds generation.completed on [DONE] from what only these set:
// a non-null usage, a non-empty service_tier, choices[0].finish_reason, the
// first non-empty delta.refusal, and a top-level error, which silences the
// rest of the stream. Content, reasoning and tool-call deltas feed items only.
// The tier is compared with the one already applied because OpenAI repeats it
// on every chunk. Invalid JSON fails translate before any state changes, so
// the scan's leniency there cannot skip a frame that matters.
func (s *ccToCanonicalStream) affectsSummary(frame []byte) bool {
	if s.errorEmitted {
		return false
	}
	for sc := sse.NewScanner(frame); sc.Next(); {
		if s.dataAffectsSummary(sc.Data()) {
			return true
		}
	}
	return false
}

func (s *ccToCanonicalStream) dataAffectsSummary(data []byte) bool {
	if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
		return true
	}
	m := jsonscan.Object(data)
	for m.Next() {
		k, v := m.Key(), m.Value()
		switch {
		case jsonscan.KeyIs(k, "usage"), jsonscan.KeyIs(k, "error"):
			if !jsonscan.IsNull(v) {
				return true
			}
		case jsonscan.KeyIs(k, "service_tier"):
			if !jsonscan.NullOrEmpty(v) && !jsonscan.StringIs(v, s.serviceTier) {
				return true
			}
		case jsonscan.KeyIs(k, "choices"):
			if !jsonscan.IsNull(v) && s.firstChoiceAffectsSummary(v) {
				return true
			}
		}
	}
	return m.Err()
}

// firstChoiceAffectsSummary checks choices[0], the only choice translate reads.
func (s *ccToCanonicalStream) firstChoiceAffectsSummary(choices []byte) bool {
	e := jsonscan.Array(choices)
	if !e.Next() {
		return e.Err()
	}
	m := jsonscan.Object(e.Value())
	for m.Next() {
		k, v := m.Key(), m.Value()
		switch {
		case jsonscan.KeyIs(k, "finish_reason"):
			if !jsonscan.NullOrEmpty(v) {
				return true
			}
		case jsonscan.KeyIs(k, "delta"):
			if !s.refused && !jsonscan.IsNull(v) && deltaHasRefusal(v) {
				return true
			}
		}
	}
	return m.Err()
}

func deltaHasRefusal(delta []byte) bool {
	m := jsonscan.Object(delta)
	for m.Next() {
		if jsonscan.KeyIs(m.Key(), "refusal") && !jsonscan.NullOrEmpty(m.Value()) {
			return true
		}
	}
	return m.Err()
}
