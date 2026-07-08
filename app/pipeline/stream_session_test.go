package pipeline_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/pipeline"
	"github.com/wyolet/relay/pkg/lifecycle"
	pkgusage "github.com/wyolet/relay/sdk/usage"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// identityTranslator declares no stream transform, so v1.StreamSummarizer
// harvests the canonical SSE body directly — enough to exercise the pipeline
// → session token handoff without a real vendor adapter.
type identityTranslator struct{}

func (identityTranslator) ParseRequest([]byte) (*v1.Request, error)     { return nil, nil }
func (identityTranslator) SerializeRequest(*v1.Request) ([]byte, error) { return nil, nil }
func (identityTranslator) ParseResponse([]byte) (*v1.Response, error)   { return nil, nil }
func (identityTranslator) SerializeResponse(*v1.Response, *v1.Request) ([]byte, error) {
	return nil, nil
}
func (identityTranslator) NewToCanonicalStream() func([]byte) ([]byte, error)   { return nil }
func (identityTranslator) NewFromCanonicalStream() func([]byte) ([]byte, error) { return nil }

// recordingStreamFactory captures the frames the pipeline tees into the
// session, proving the streamed body is observed incrementally rather than
// buffered whole for post-flight.
type recordingStreamFactory struct{ frames *int }

func (recordingStreamFactory) Name() string { return "rec" }
func (f recordingStreamFactory) NewObserver(*lifecycle.Context) lifecycle.StreamObserver {
	return &recStreamObs{frames: f.frames}
}

type recStreamObs struct{ frames *int }

func (o *recStreamObs) Observe([]byte)       { *o.frames++ }
func (o *recStreamObs) Result() (any, error) { return *o.frames, nil }

// A streamed request with a stream session must (a) feed the upstream frames
// to the session (not buffer them for post-flight) and (b) stash the tokens
// the session extracted on the Context — those, not the adapter's
// ExtractTokens, are what post-flight commits.
func TestRun_StreamSessionExtractsTokens(t *testing.T) {
	completed, _ := json.Marshal(v1.GenerationCompletedEvent{
		ID:    "resp_1",
		Usage: pkgusage.Tokens{"input": 42, "output": 7},
	})
	sse := string(v1.SSEFrame{Event: v1.EventGenerationCreated, Data: []byte(`{}`)}.Bytes()) +
		string(v1.SSEFrame{Event: v1.EventGenerationCompleted, Data: completed}.Bytes())

	adp := &fakeAdapter{
		// A different value than the stream's usage, so a fallback to the
		// adapter's ExtractTokens would be detectable.
		tokens: pkgusage.Tokens{"input": 1},
		callFn: func(context.Context, string, string, []byte, http.Header) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(sse))}, nil
		},
	}

	frames := 0
	reg := lifecycle.New()
	reg.RegisterStreamObserver(recordingStreamFactory{frames: &frames})

	p := newPipeline()
	p.Lifecycle = reg

	lc := lifecycle.NewContext("req-stream", "pipeline", time.Now())
	lc.Translator = identityTranslator{}
	req := &pipeline.Request{
		Adapter:   adp,
		Keys:      []*hostkey.HostKey{makeKey("h", "sk")},
		Policy:    makePolicy(),
		Stream:    true,
		Lifecycle: lc,
	}

	res, err := p.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	drainResult(t, res)

	if frames == 0 {
		t.Fatal("stream session never observed a frame — body was not teed into it")
	}
	got, ok := lc.StreamTokens()
	if !ok {
		t.Fatal("stream tokens not stashed on the Context")
	}
	if got["input"] != 42 || got["output"] != 7 {
		t.Fatalf("stream tokens = %+v, want the stream's usage {input:42, output:7}", got)
	}
}
