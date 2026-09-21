package v1

// DefaultKeepAliveFrame is the fallback idle frame: an SSE comment line, which every conforming SSE consumer ignores while still counting as bytes for clients that abort a stream after N seconds of silence. Treat it as immutable — callers only write it.
var DefaultKeepAliveFrame = []byte(": keepalive\n\n")

// KeepAliver is implemented by a Translator whose wire shape has a no-op stream frame the relay may emit while the upstream is silent. Shapes without such a frame simply don't implement it and get DefaultKeepAliveFrame.
type KeepAliver interface {
	KeepAliveFrame() []byte
}

// KeepAliveFrameFor returns the idle frame to emit on a stream whose caller speaks t's wire shape, falling back to the SSE comment when t declares none.
func KeepAliveFrameFor(t Translator) []byte {
	if k, ok := t.(KeepAliver); ok {
		if f := k.KeepAliveFrame(); len(f) > 0 {
			return f
		}
	}
	return DefaultKeepAliveFrame
}
