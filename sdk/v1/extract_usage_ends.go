package v1

// JSONEndsSummarizer is an optional companion to Translator for shapes whose sync JSON response keeps its Summary (usage, finish reason, service tier) near the ends of the body, content in between. SummaryFromJSONEnds returns the Summary ExtractSummary would return for body and true, reading only those ends, or false when what it read does not settle it. It does not validate the bytes it skips: a body the full decode would reject as malformed can still yield its usage.
type JSONEndsSummarizer interface {
	SummaryFromJSONEnds(body []byte) (Summary, bool)
}

// StreamEndsSummarizer is the SSE counterpart of JSONEndsSummarizer: SummaryFromStreamEnds reads a whole buffered stream's Summary from the few events that carry it, found without splitting every frame, and returns false when another frame of body could still change it.
type StreamEndsSummarizer interface {
	SummaryFromStreamEnds(body []byte) (Summary, bool)
}

// ExtractSummaryEndsFirst returns what ExtractSummary returns for body, asking tr's ends reader for the body's framing first and decoding the whole body only when that reader declines. fellBack reports that tr has such a reader and it declined, so the full decode ran; a shape with no reader for the framing decodes in full without falling back.
func ExtractSummaryEndsFirst(tr Translator, body []byte) (sum Summary, fellBack bool, err error) {
	if tr == nil || len(body) == 0 {
		return Summary{}, false, nil
	}
	if body, err = maybeUngzip(body); err != nil {
		return Summary{}, false, err
	}
	var read func([]byte) (Summary, bool)
	if looksLikeSSE(body) {
		if r, ok := tr.(StreamEndsSummarizer); ok {
			read = r.SummaryFromStreamEnds
		}
	} else if r, ok := tr.(JSONEndsSummarizer); ok {
		read = r.SummaryFromJSONEnds
	}
	if read == nil {
		return summarizeDecoded(tr, body), false, nil
	}
	if sum, settled := readEnds(read, body); settled {
		return sum, false, nil
	}
	return summarizeDecoded(tr, body), true, nil
}

// readEnds runs an ends reader. It walks untrusted upstream bytes on every response, so a panic in one declines, leaving the Summary to the full decode, instead of taking down the caller.
func readEnds(read func([]byte) (Summary, bool), body []byte) (sum Summary, settled bool) {
	defer func() {
		if recover() != nil {
			sum, settled = Summary{}, false
		}
	}()
	return read(body)
}
