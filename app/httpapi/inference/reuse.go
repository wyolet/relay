package inference

// Exports of two internal request helpers so the batch subsystem can run an
// item through the realtime pipeline without forking the (fiddly) model-field
// rewrite or the minimal model/stream parse. Single source of truth stays here.

// ExtractModelStream parses the caller-supplied model name and stream flag from
// a request body. Reused by the batch subsystem's per-item runner.
func ExtractModelStream(body []byte) (model string, stream bool, err error) {
	return extractModelStream(body)
}

// RewriteModelField returns body with its "model" field rewritten to
// upstreamModel (the resolved upstream model name). Reused by the batch
// subsystem's per-item runner.
func RewriteModelField(body []byte, upstreamModel string) []byte {
	return rewriteModelField(body, upstreamModel)
}
