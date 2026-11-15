package client

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/wyolet/relay/sdk/adapters/openai"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// streamOver reads body as a client stream would, through toCanon when given.
func streamOver(body string, toCanon func([]byte) ([]byte, error)) *Stream {
	sc := bufio.NewScanner(strings.NewReader(body))
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	sc.Split(v1.SplitSSEFrames)
	return &Stream{body: io.NopCloser(strings.NewReader("")), sc: sc, toCanon: toCanon, start: time.Now()}
}

func eventTypes(events []Event) []string {
	types := make([]string, len(events))
	for i, ev := range events {
		types[i] = ev.Type
	}
	return types
}

func TestGenerateStream_RelayEvents(t *testing.T) {
	// CRLF line ends and a comment frame must not change what the caller receives.
	body := ": keepalive\r\n\r\n" + strings.ReplaceAll(canonicalStream, "\n", "\r\n")
	rec := newRecorder(t, http.StatusOK, body)
	got := drain(t, Relay(rec.srv.URL, "rk"))
	want := []Event{
		{Type: v1.EventGenerationCreated, Data: []byte(`{"id":"resp_1","model":"m"}`)},
		{Type: v1.EventItemDelta, Data: []byte(`{"item_id":"msg_1","index":0,"kind":"text","delta":"hi"}`)},
		{Type: v1.EventGenerationCompleted, Data: []byte(`{"id":"resp_1","status":"completed","finish_reason":"stop"}`)},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("events (-want +got):\n%s", diff)
	}
	if sent := rec.only(t).Body; sent != sampleWire("stream") {
		t.Errorf("body:\n got %s\nwant %s", sent, sampleWire("stream"))
	}
}

const ccStreamBody = `data: {"id":"cc1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"}}]}

data: {"id":"cc1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000000,"completion_tokens":100000}}

data: [DONE]

`

func TestGenerateStream_VendorEventsAndCost(t *testing.T) {
	rec := newRecorder(t, http.StatusOK, ccStreamBody)
	tests := []struct {
		name   string
		client *Client
		cost   float64
		priced bool
	}{
		{"priced binding", catalogClient(t, "gpt-4o@openai-direct", rec.srv.URL), 2.5 + 1.0, true},
		{"binding without pricing", catalogClient(t, "llama3@ollama-local", rec.srv.URL), 0, false},
		{"constructor client", OpenAI(rec.srv.URL, "sk"), 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stream, err := tc.client.GenerateStream(context.Background(), sampleReq())
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Close()
			if cost, ok := stream.Cost(); cost != 0 || ok {
				t.Errorf("Cost before the usage frame = %v, %v", cost, ok)
			}
			events := recvAll(t, stream)
			want := []string{v1.EventGenerationCreated, v1.EventItemStarted, v1.EventItemDelta, v1.EventItemCompleted, v1.EventGenerationCompleted}
			if diff := cmp.Diff(want, eventTypes(events)); diff != "" {
				t.Errorf("event types (-want +got):\n%s", diff)
			}
			if cost, ok := stream.Cost(); cost != tc.cost || ok != tc.priced {
				t.Errorf("Cost = %v, %v; want %v, %v", cost, ok, tc.cost, tc.priced)
			}
		})
	}
}

func TestStream_Timing(t *testing.T) {
	frames := []string{
		"event: generation.created\ndata: {\"id\":\"r1\"}\n\n",
		"event: item.started\ndata: {\"item_id\":\"rs1\",\"item_type\":\"reasoning\",\"index\":0}\n\n",
		"event: item.delta\ndata: {\"item_id\":\"rs1\",\"kind\":\"reasoning\",\"delta\":\"x\"}\n\n",
		"event: item.delta\ndata: {\"item_id\":\"m1\",\"kind\":\"text\",\"delta\":\"hi\"}\n\n",
		"event: generation.completed\ndata: {\"id\":\"r1\",\"status\":\"completed\"}\n\n",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for _, f := range frames {
			_, _ = io.WriteString(w, f)
			w.(http.Flusher).Flush()
			time.Sleep(2 * time.Millisecond)
		}
	}))
	defer srv.Close()

	stream, err := Relay(srv.URL, "rk").GenerateStream(context.Background(), sampleReq())
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if tm := stream.Timing(); tm.Upstream.ResponseStart != 0 || tm.Upstream.ResponseEnd != 0 || tm.Reasoning != nil {
		t.Errorf("timing before the first event = %+v", tm)
	}
	recvAll(t, stream)

	// Each frame is 2ms after the one before, so the spans nest strictly: first byte < reasoning start < reasoning end < stream end.
	tm := stream.Timing()
	up, rs := tm.Upstream, tm.Reasoning
	if up.ResponseStart <= 0 || up.Start != up.ResponseStart {
		t.Errorf("upstream = %+v, want a first-byte time with Start equal to it (no relay leg)", up)
	}
	if rs == nil || rs.Start <= up.ResponseStart || rs.End <= rs.Start || up.ResponseEnd <= rs.End {
		t.Errorf("upstream %+v reasoning %+v, want first byte < reasoning start < reasoning end < end", up, rs)
	}

	plain := streamOver(canonicalStream, nil)
	recvAll(t, plain)
	if tm := plain.Timing(); tm.Reasoning != nil || tm.Upstream.ResponseEnd < tm.Upstream.ResponseStart {
		t.Errorf("stream without reasoning: %+v, want no reasoning span", tm)
	}
}

func TestStream_Recv_MalformedFrameReturnsErrorAndStreamContinues(t *testing.T) {
	body := "" +
		"event: response.created\ndata: {\"response\":{\"id\":\"resp_1\",\"model\":\"m\"}}\n\n" +
		"event: response.output_item.added\ndata: {\"output_index\":0,\"item\":{\"type\":\"message\",\"id\":\"msg_1\",\"role\":\"assistant\"}}\n\n" +
		"event: response.output_text.delta\ndata: {\"item_id\":\"msg_1\",\"delta\":42}\n\n" +
		"event: response.output_text.delta\ndata: {\"item_id\":\"msg_1\",\"delta\":\"hi\"}\n\n" +
		"event: response.completed\ndata: {\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"output\":[]}}\n\n"
	s := streamOver(body, openai.ResponsesTranslator{}.NewToCanonicalStream())

	var events []string
	var errs int
	for {
		ev, err := s.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			errs++
			continue
		}
		events = append(events, ev.Type)
	}
	if errs != 1 {
		t.Fatalf("Recv errors = %d, want 1 for the malformed frame", errs)
	}
	want := []string{v1.EventGenerationCreated, v1.EventItemStarted, v1.EventItemDelta, v1.EventGenerationCompleted}
	if diff := cmp.Diff(want, events); diff != "" {
		t.Fatalf("events (-want +got):\n%s", diff)
	}
}

func TestStream_Recv_Edges(t *testing.T) {
	t.Run("frame over the 4 MiB limit", func(t *testing.T) {
		s := streamOver("data: "+strings.Repeat("x", 4<<20)+"\n\n", nil)
		if ev, err := s.Recv(); ev != nil || !errors.Is(err, bufio.ErrTooLong) {
			t.Errorf("Recv = %v, %v; want bufio.ErrTooLong", ev, err)
		}
	})
	t.Run("frames without data are skipped", func(t *testing.T) {
		got := recvAll(t, streamOver(": comment\n\nevent: ping\n\nretry: 5\n\ndata: x\n\n", nil))
		if diff := cmp.Diff([]Event{{Data: []byte("x")}}, got); diff != "" {
			t.Errorf("events (-want +got):\n%s", diff)
		}
	})
	t.Run("one upstream frame fanning out to several", func(t *testing.T) {
		fan := func([]byte) ([]byte, error) {
			return []byte("event: a\ndata: 1\n\n\n\nevent: b\ndata: 2\n\nevent: c\ndata: 3"), nil
		}
		got := recvAll(t, streamOver("data: x\n\n", fan))
		want := []Event{{Type: "a", Data: []byte("1")}, {Type: "b", Data: []byte("2")}, {Type: "c", Data: []byte("3")}}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("events (-want +got):\n%s", diff)
		}
	})
	t.Run("EOF repeats", func(t *testing.T) {
		s := streamOver("", nil)
		for range 2 {
			if ev, err := s.Recv(); ev != nil || !errors.Is(err, io.EOF) {
				t.Errorf("Recv = %v, %v; want io.EOF", ev, err)
			}
		}
	})
}
