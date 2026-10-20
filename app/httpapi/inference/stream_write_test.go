package inference

// Write discipline of the cross-shape stream path: each emitted frame reaches the underlying writer in one Write followed by one Flush, as soon as it is translated, with the keepalive wrapper on and off, and the bytes on the wire do not depend on either.

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/usagelog"
	"github.com/wyolet/relay/pkg/httpheader"
	"github.com/wyolet/relay/pkg/lifecycle"
	"github.com/wyolet/relay/pkg/sse"
	"github.com/wyolet/relay/sdk/adapters/anthropic"
	"github.com/wyolet/relay/sdk/adapters/openai"
	v1 "github.com/wyolet/relay/sdk/v1"
)

var updateStreamGolden = flag.Bool("update", false, "rewrite the stream golden files under testdata/stream_canonical/")

// productionKeepAlive is long enough that no keepalive frame lands during a test, so the counts below are the stream's own.
const productionKeepAlive = 15 * time.Second

// anthropicMultiDeltaStream is an Anthropic upstream stream with three text deltas.
var anthropicMultiDeltaStream = []string{
	"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"test-model\",\"content\":[],\"stop_reason\":null,\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\n",
	"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
	"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hel\"}}\n\n",
	"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"lo\"}}\n\n",
	"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\" world\"}}\n\n",
	"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
	"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":3}}\n\n",
	"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
}

// opRecorder is an http.ResponseWriter + http.Flusher logging each Write ('W') and Flush ('F') in call order.
type opRecorder struct {
	mu   sync.Mutex
	hdr  http.Header
	body bytes.Buffer
	ops  []byte
}

func (o *opRecorder) Header() http.Header {
	if o.hdr == nil {
		o.hdr = http.Header{}
	}
	return o.hdr
}

func (o *opRecorder) WriteHeader(int) {}

func (o *opRecorder) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.ops = append(o.ops, 'W')
	return o.body.Write(p)
}

func (o *opRecorder) Flush() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.ops = append(o.ops, 'F')
}

// clockFields are the parts of the output that differ run to run: the CC chunk's wall-clock timestamp and relay_usage's elapsed time.
var clockFields = regexp.MustCompile(`"(created|end)":\d+`)

func checkStreamGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	got = clockFields.ReplaceAll(got, []byte(`"$1":0`))
	path := filepath.Join("testdata", "stream_canonical", name+".golden")
	if *updateStreamGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("missing golden %s (run go test -update): %v", path, err)
	}
	if !bytes.Equal(want, got) {
		t.Errorf("wire bytes differ from %s\n--- want ---\n%s\n--- got ---\n%s", path, want, got)
	}
}

// anthropicToCanonicalFailingStop is the Anthropic stream translator failing on message_stop, as it would on a malformed terminal event.
func anthropicToCanonicalFailingStop() func([]byte) ([]byte, error) {
	next := anthropic.AnthropicTranslator{}.NewToCanonicalStream()
	fail := failOn("message_stop")
	return func(b []byte) ([]byte, error) {
		if _, err := fail(b); err != nil {
			return nil, err
		}
		return next(b)
	}
}

func TestStreamCanonical_OneWriteAndFlushPerFrame(t *testing.T) {
	cases := []struct {
		name      string
		echo      bool
		toCanon   func() func([]byte) ([]byte, error)
		fromCanon func() func([]byte) ([]byte, error)
		// frames is the number of frames streamCanonical emits for anthropicMultiDeltaStream.
		frames int
	}{
		{
			// generation.created, three deltas, and the finish chunk + [DONE] (one canonical event, one frame write).
			name:      "anthropic_to_cc",
			toCanon:   anthropic.AnthropicTranslator{}.NewToCanonicalStream,
			fromCanon: openai.CCTranslator{}.NewFromCanonicalStream,
			frames:    5,
		},
		{
			// Held one frame back for the relay_usage splice; the last is written after the scan loop.
			name:      "anthropic_to_canonical_echo",
			echo:      true,
			toCanon:   anthropic.AnthropicTranslator{}.NewToCanonicalStream,
			fromCanon: v1.IdentityTranslator{}.NewFromCanonicalStream,
			frames:    7,
		},
		{
			// The terminal event is dropped, so the closing incomplete-stream frame ends it.
			name:      "anthropic_to_cc_dropped_terminal",
			toCanon:   anthropicToCanonicalFailingStop,
			fromCanon: openai.CCTranslator{}.NewFromCanonicalStream,
			frames:    5,
		},
	}
	for _, tc := range cases {
		for _, keepAlive := range []time.Duration{productionKeepAlive, 0} {
			name := tc.name + "/keepalive_on"
			if keepAlive == 0 {
				name = tc.name + "/keepalive_off"
			}
			t.Run(name, func(t *testing.T) {
				cat, _ := buildDispatchCatalog(t, "anthropic", adapters.Anthropic)
				d := buildDeps(t, cat)
				r := httptest.NewRequest(http.MethodPost, "/v1/generate", nil)
				if tc.echo {
					reg := lifecycle.New()
					reg.RegisterStreamObserver(usagelog.NewStreamUsageFactory(nil, "test"))
					d.Lifecycle = reg
					lc := lifecycle.NewContext("req-1", "pipeline", time.Now())
					lc.Translator = anthropic.AnthropicTranslator{}
					lc.SetStreamSession(reg.NewStreamSession(lc))
					r = r.WithContext(lifecycle.ContextWith(r.Context(), lc))
				}

				rec := &opRecorder{}
				// Mirrors keepAliveWriter: the ResponseWriter itself when the keepalive is off.
				var w io.Writer = rec
				if keepAlive > 0 {
					ka := sse.NewKeepAlive(rec, v1.DefaultKeepAliveFrame, keepAlive)
					ka.Start(r.Context())
					defer ka.Stop()
					w = ka
				}
				body := io.NopCloser(strings.NewReader(strings.Join(anthropicMultiDeltaStream, "")))
				streamCanonical(d, w, r, body, tc.echo, false, streamShapes{}, tc.toCanon(), tc.fromCanon())

				if got, want := string(rec.ops), strings.Repeat("WF", tc.frames); got != want {
					t.Errorf("underlying writer ops = %q, want %q (one Write then one Flush per frame)", got, want)
				}
				checkStreamGolden(t, tc.name, rec.body.Bytes())
			})
		}
	}
}

// gatedUpstream streams events one at a time: after writing and flushing each, it waits for release before the next, and after the last it waits once more before ending the body. Closing stop ends the handler early so a failed test does not hang in the servers' Close.
func gatedUpstream(t *testing.T, events []string) (srv *httptest.Server, release chan<- struct{}, stop chan<- struct{}) {
	t.Helper()
	rel := make(chan struct{})
	stp := make(chan struct{})
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f := w.(http.Flusher)
		for _, e := range events {
			_, _ = w.Write([]byte(e))
			f.Flush()
			select {
			case <-rel:
			case <-stp:
				return
			case <-r.Context().Done():
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv, rel, stp
}

// readFrames sends each complete SSE frame of body to out as it arrives, closing out at EOF.
func readFrames(body io.Reader, out chan<- string) {
	defer close(out)
	br := bufio.NewReader(body)
	var frame strings.Builder
	for {
		line, err := br.ReadString('\n')
		frame.WriteString(line)
		if line == "\n" {
			out <- frame.String()
			frame.Reset()
		}
		if err != nil {
			return
		}
	}
}

// awaitFrame reads frames until one contains marker. reqErr is read only once frames is closed.
func awaitFrame(t *testing.T, frames <-chan string, marker, while string, reqErr *error) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case f, ok := <-frames:
			if !ok {
				t.Fatalf("stream ended before a frame with %q (%s): %v", marker, while, *reqErr)
			}
			if strings.Contains(f, marker) {
				return
			}
		case <-deadline:
			t.Fatalf("no frame with %q reached the client %s", marker, while)
		}
	}
}

// step is one upstream event and the frames, in order, the client must see before the upstream is let go of the next one.
type step struct {
	event string
	want  []string
}

func runGatedStream(t *testing.T, keepAlive time.Duration, inbound adapters.Name, echo bool, steps []step, afterEOF []string) {
	t.Helper()
	events := make([]string, len(steps))
	for i, s := range steps {
		events[i] = s.event
	}
	up, release, stop := gatedUpstream(t, events)
	// Deferred, so it runs before the servers' Cleanup Close, which waits for their handlers.
	defer close(stop)
	cat, rk := keepAliveCatalog(t, up.URL)
	d := keepAliveDeps(t, cat, keepAlive)
	if echo {
		reg := lifecycle.New()
		reg.RegisterHook(usagelog.NewUsageHook(nil, "test"))
		reg.RegisterStreamObserver(usagelog.NewStreamUsageFactory(nil, "test"))
		d.Lifecycle = reg
		d.Pipeline.Lifecycle = reg
	}
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		Dispatch(d, w, withNormalContext(r, rk), DispatchInput{Inbound: inbound, Body: body, ModelName: "test-model", Stream: true})
	}))
	t.Cleanup(relay.Close)

	reqBody := `{"model":"test-model","messages":[{"role":"user","content":"hi"}],"stream":true}`
	if inbound == adapters.Canonical {
		reqBody = `{"model":"test-model","input":"hi","stream":true}`
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, relay.URL, strings.NewReader(reqBody))
	if echo {
		req.Header.Set(httpheader.HeaderUsage, httpheader.UsageValueFull)
	}
	// The response headers go out with the first frame, so the request runs alongside the steps rather than before them.
	frames := make(chan string, 64)
	var reqErr error
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			reqErr = err
			close(frames)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			reqErr = fmt.Errorf("status %d: %s", resp.StatusCode, b)
			close(frames)
			return
		}
		readFrames(resp.Body, frames)
	}()
	for i, s := range steps {
		for _, want := range s.want {
			awaitFrame(t, frames, want, "while the upstream holds event "+strconv.Itoa(i+1), &reqErr)
		}
		select {
		case release <- struct{}{}:
		case <-time.After(3 * time.Second):
			t.Fatalf("upstream stopped waiting at event %d", i+1)
		}
	}
	for _, want := range afterEOF {
		awaitFrame(t, frames, want, "after the upstream ended", &reqErr)
	}
}

func TestStreamCanonical_EachFrameReachesClientBeforeNextEvent(t *testing.T) {
	ev := anthropicMultiDeltaStream
	for _, keepAlive := range []time.Duration{productionKeepAlive, 0} {
		suffix := "/keepalive_on"
		if keepAlive == 0 {
			suffix = "/keepalive_off"
		}
		t.Run("anthropic_to_cc"+suffix, func(t *testing.T) {
			runGatedStream(t, keepAlive, adapters.OpenAI, false, []step{
				{ev[0], []string{`"role":"assistant"`}},
				{ev[1], nil},
				{ev[2], []string{`"content":"Hel"`}},
				{ev[3], []string{`"content":"lo"`}},
				{ev[4], []string{`"content":" world"`}},
				{ev[5], nil},
				{ev[6], nil},
				// The terminal chunk and [DONE] land while the upstream connection is still open.
				{ev[7], []string{`"finish_reason":"stop"`, "data: [DONE]"}},
			}, nil)
		})
		// With the usage echo on, the stream holds one frame back by design so the terminal event can carry relay_usage: each frame reaches the client as soon as the next one is translated, and generation.completed (the only frame with relay_usage) once the upstream ends.
		t.Run("anthropic_to_canonical_echo"+suffix, func(t *testing.T) {
			runGatedStream(t, keepAlive, adapters.Canonical, true, []step{
				{ev[0], nil},
				{ev[1], []string{"event: generation.created"}},
				{ev[2], []string{"event: item.started"}},
				{ev[3], []string{`"delta":"Hel"`}},
				{ev[4], []string{`"delta":"lo"`}},
				{ev[5], []string{`"delta":" world"`}},
				{ev[6], nil},
				{ev[7], []string{"event: item.completed"}},
			}, []string{`"relay_usage"`})
		})
	}
}
