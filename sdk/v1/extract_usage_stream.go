package v1

// StreamSummarizer is the incremental counterpart to ExtractSummary's SSE
// path: instead of buffering a whole vendor SSE body and walking it once at
// the end, it harvests the Summary frame-by-frame as they arrive, retaining
// only the running canonical stream state and the last usage-bearing
// Summary. Feeding every frame of a body to a StreamSummarizer yields the
// same Summary that ExtractSummary would return for the concatenated body —
// same per-frame translation, same "last non-empty summary wins" rule — so a
// streamed observer never has to retain the whole stream to report usage.
//
// Not safe for concurrent use: one stream, one summarizer, one goroutine
// (the translator closure it holds carries per-stream state).
type StreamSummarizer struct {
	toCanon       func([]byte) ([]byte, error)
	hasTranslator bool
	found         Summary
}

// NewStreamSummarizer builds a summarizer bound to tr's to-canonical stream
// transform. A nil translator yields a no-op summarizer (Summary stays zero),
// matching ExtractSummary(nil, …).
func NewStreamSummarizer(tr Translator) *StreamSummarizer {
	if tr == nil {
		return &StreamSummarizer{}
	}
	// toCanon may be nil: the translator declares no stream transform, i.e.
	// the wire IS canonical SSE and frames are harvested directly.
	return &StreamSummarizer{toCanon: tr.NewToCanonicalStream(), hasTranslator: true}
}

// Observe feeds one vendor SSE frame — a single event, WITHOUT the trailing
// blank-line separator (it is re-appended internally, mirroring
// extractSummaryFromSSE which feeds the closure separator-terminated chunks).
// Usage/finish from a terminal generation.completed event replaces the
// retained Summary wholesale (last non-empty wins), exactly as the batch
// ExtractSummary does.
func (s *StreamSummarizer) Observe(frame []byte) {
	if s == nil || !s.hasTranslator || len(frame) == 0 {
		return
	}
	input := make([]byte, 0, len(frame)+2)
	input = append(input, frame...)
	input = append(input, '\n', '\n')

	canon := input // toCanon nil: wire is already canonical SSE.
	if s.toCanon != nil {
		c, err := s.toCanon(input)
		if err != nil || len(c) == 0 {
			return
		}
		canon = c
	}
	if sum := harvestSummaryFromCanonicalSSE(canon); len(sum.Tokens) > 0 || sum.FinishReason != "" {
		s.found = sum
	}
}

// Summary returns the Summary harvested so far (zero until a usage-bearing
// frame is seen). Nil-safe.
func (s *StreamSummarizer) Summary() Summary {
	if s == nil {
		return Summary{}
	}
	return s.found
}
