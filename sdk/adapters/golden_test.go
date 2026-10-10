package adapters_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// Each case under testdata/<vendor>/<case> holds up to three wire inputs; every
// input drives two translator directions, each pinned by its own golden:
//
//	request.json  → request.canonical.json  (ParseRequest)
//	              → request.wire.json       (SerializeRequest of that canonical)
//	response.json → response.canonical.json (ParseResponse)
//	              → response.wire.json      (SerializeResponse, echoing the case request)
//	stream.sse    → stream.canonical.json   (NewToCanonicalStream, frame by frame)
//	              → stream.wire.json        (NewFromCanonicalStream over those frames)
//
// The second golden of each pair is the X → canonical → X round trip. On top
// of the goldens, canonical → X → canonical must reproduce the canonical
// golden: whatever a parse yields, the shape must be able to carry back.
const (
	requestInput  = "request.json"
	responseInput = "response.json"
	streamInput   = "stream.sse"
)

// lossyRoundTrip explains why canonical → X → canonical cannot reproduce a
// case's parse. A bug is reported as a skipped subtest until fixed; a lossy
// mapping by design is logged. The wire golden pins the output either way,
// and an entry that stops differing fails as stale.
type lossyRoundTrip struct {
	why string
	bug bool
}

// lossyRoundTrips is keyed by "<case dir> <input file>".
var lossyRoundTrips = func() map[string]lossyRoundTrip {
	m := map[string]lossyRoundTrip{
		"testdata/openai/chat-refusal stream.sse":           {why: "a CC stream writes a refusal as text deltas plus finish_reason content_filter — message.refusal would need the finish reason before the first delta"},
		"testdata/openai/responses-tool-calls request.json": {why: "input items carry no status on Responses (annotated drop)"},
	}
	for _, c := range []string{"service-tier", "text"} {
		m["testdata/anthropic/"+c+" "+responseInput] = lossyRoundTrip{why: "SerializeResponse drops service_tier (annotated): the canonical tier is the serving upstream's lane name, which usage.service_tier would mislabel when another vendor served it"}
	}
	return m
}()

// checkRoundTrip compares the canonical golden with the canonical re-parsed
// from the serialized wire.
func checkRoundTrip(t *testing.T, dir, input string, canon, again []byte) {
	t.Helper()
	known, lossy := lossyRoundTrips[filepath.ToSlash(dir)+" "+input]
	same := bytes.Equal(canon, again)
	switch {
	case lossy && same:
		t.Errorf("%s %s round-trips cleanly now; delete its lossyRoundTrips entry", dir, input)
	case lossy && known.bug:
		t.Run("round trip "+input, func(t *testing.T) { t.Skip("lossy round trip: " + known.why) })
	case lossy:
		t.Logf("%s round trip is lossy by design: %s", input, known.why)
	case !same:
		t.Errorf("canonical → wire → canonical changed %s:\n%s", input, firstDifference(canon, again))
	}
}

func TestGoldenCorpus(t *testing.T) {
	vendors, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatal(err)
	}
	for _, vendor := range vendors {
		cases, err := os.ReadDir(filepath.Join("testdata", vendor.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range cases {
			s, ok := shapeForCase(vendor.Name(), c.Name())
			if !ok {
				t.Errorf("testdata/%s/%s: no shape owns this case", vendor.Name(), c.Name())
				continue
			}
			dir := filepath.Join("testdata", vendor.Name(), c.Name())
			t.Run(vendor.Name()+"/"+c.Name(), func(t *testing.T) { runCase(t, s, dir) })
		}
	}
}

func runCase(t *testing.T, s shape, dir string) {
	known := map[string]bool{}
	var req *v1.Request
	if body, ok := readInput(t, dir, requestInput, known); ok {
		req = runRequest(t, s, dir, body, known)
	}
	if body, ok := readInput(t, dir, responseInput, known); ok {
		runResponse(t, s, dir, body, req, known)
	}
	if body, ok := readInput(t, dir, streamInput, known); ok {
		runStream(t, s, dir, body, req, known)
	}
	if len(known) == 0 {
		t.Fatalf("%s has none of %s, %s, %s", dir, requestInput, responseInput, streamInput)
	}
	checkNoStrayFiles(t, dir, known)
}

func readInput(t *testing.T, dir, name string, known map[string]bool) ([]byte, bool) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if os.IsNotExist(err) {
		return nil, false
	}
	if err != nil {
		t.Fatal(err)
	}
	known[name] = true
	return b, true
}

// golden writes or checks one direction's output; a direction that errored
// records the error instead.
func golden(t *testing.T, dir, name string, known map[string]bool, out []byte, err error) {
	t.Helper()
	known[name] = true
	if err != nil {
		checkGolden(t, filepath.Join(dir, name), errorGolden(err))
		return
	}
	checkGolden(t, filepath.Join(dir, name), out)
}

func runRequest(t *testing.T, s shape, dir string, body []byte, known map[string]bool) *v1.Request {
	t.Helper()
	req, err := s.tr.ParseRequest(body)
	if err != nil {
		golden(t, dir, "request.canonical.json", known, nil, err)
		return nil
	}
	canon := mustPrettyValue(t, req)
	golden(t, dir, "request.canonical.json", known, canon, nil)

	wire, err := s.tr.SerializeRequest(req)
	if err != nil {
		golden(t, dir, "request.wire.json", known, nil, err)
		return req
	}
	golden(t, dir, "request.wire.json", known, mustPretty(t, wire), nil)

	back, err := s.tr.ParseRequest(wire)
	if err != nil {
		t.Errorf("re-parse of the serialized request: %v", err)
		return req
	}
	checkRoundTrip(t, dir, requestInput, canon, mustPrettyValue(t, back))
	return req
}

func runResponse(t *testing.T, s shape, dir string, body []byte, req *v1.Request, known map[string]bool) {
	t.Helper()
	resp, err := s.tr.ParseResponse(body)
	if err != nil {
		golden(t, dir, "response.canonical.json", known, nil, err)
		return
	}
	canon := mustPrettyValue(t, resp)
	golden(t, dir, "response.canonical.json", known, canon, nil)

	wire, err := s.tr.SerializeResponse(resp, req)
	if err != nil {
		golden(t, dir, "response.wire.json", known, nil, err)
		return
	}
	golden(t, dir, "response.wire.json", known, mustPretty(t, wire), nil)

	back, err := s.tr.ParseResponse(wire)
	if err != nil {
		t.Errorf("re-parse of the serialized response: %v", err)
		return
	}
	checkRoundTrip(t, dir, responseInput, canon, mustPrettyValue(t, back))
}

func runStream(t *testing.T, s shape, dir string, body []byte, req *v1.Request, known map[string]bool) {
	t.Helper()
	canonFrames, err := translateFrames(s.tr.NewToCanonicalStream(), splitFrames(body))
	if err != nil {
		golden(t, dir, "stream.canonical.json", known, nil, err)
		return
	}
	canon := mustPrettyValue(t, recordFrames(canonFrames))
	golden(t, dir, "stream.canonical.json", known, canon, nil)

	wireFrames, err := translateFrames(fromCanonicalStream(s.tr, req), canonFrames)
	if err != nil {
		golden(t, dir, "stream.wire.json", known, nil, err)
		return
	}
	golden(t, dir, "stream.wire.json", known, mustPrettyValue(t, recordFrames(wireFrames)), nil)

	backFrames, err := translateFrames(s.tr.NewToCanonicalStream(), wireFrames)
	if err != nil {
		t.Errorf("re-translation of the serialized stream: %v", err)
		return
	}
	checkRoundTrip(t, dir, streamInput, canon, mustPrettyValue(t, recordFrames(backFrames)))
}

// fromCanonicalStream prefers the request-aware factory when the case has a
// request, as dispatch does.
func fromCanonicalStream(tr v1.Translator, req *v1.Request) func([]byte) ([]byte, error) {
	if aware, ok := tr.(v1.RequestAwareStream); ok && req != nil {
		return aware.NewFromCanonicalStreamFor(req)
	}
	return tr.NewFromCanonicalStream()
}

// translateFrames feeds frames one at a time through a stream function, as
// dispatch does, and splits its output back into frames.
func translateFrames(fn func([]byte) ([]byte, error), frames [][]byte) ([][]byte, error) {
	if fn == nil {
		return frames, nil
	}
	var out [][]byte
	for _, f := range frames {
		b, err := fn(f)
		if err != nil {
			return nil, err
		}
		out = append(out, splitFrames(b)...)
	}
	return out, nil
}

func mustPretty(t *testing.T, raw []byte) []byte {
	t.Helper()
	b, err := pretty(raw)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustPrettyValue(t *testing.T, v any) []byte {
	t.Helper()
	b, err := prettyValue(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
