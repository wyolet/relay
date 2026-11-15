package openai

import (
	"testing"

	v1 "github.com/wyolet/relay/sdk/v1"
)

func TestRequestKeys(t *testing.T) {
	for _, p := range []v1.RequestKeysProvider{CCTranslator{}, ResponsesTranslator{}} {
		if got := p.RequestKeys(); got != (v1.RequestKeys{Stream: "stream"}) {
			t.Errorf("%T: RequestKeys = %+v, want the stream bool", p, got)
		}
	}
}
