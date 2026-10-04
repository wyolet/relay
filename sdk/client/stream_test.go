package client

import (
	"bufio"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wyolet/relay/sdk/adapters/openai"
	v1 "github.com/wyolet/relay/sdk/v1"
)

func TestStreamRecv_MalformedFrameReturnsErrorAndStreamContinues(t *testing.T) {
	body := "" +
		"event: response.created\ndata: {\"response\":{\"id\":\"resp_1\",\"model\":\"m\"}}\n\n" +
		"event: response.output_item.added\ndata: {\"output_index\":0,\"item\":{\"type\":\"message\",\"id\":\"msg_1\",\"role\":\"assistant\"}}\n\n" +
		"event: response.output_text.delta\ndata: {\"item_id\":\"msg_1\",\"delta\":42}\n\n" +
		"event: response.output_text.delta\ndata: {\"item_id\":\"msg_1\",\"delta\":\"hi\"}\n\n" +
		"event: response.completed\ndata: {\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"output\":[]}}\n\n"
	sc := bufio.NewScanner(strings.NewReader(body))
	sc.Split(splitSSEFrames)
	s := &Stream{
		body:    io.NopCloser(strings.NewReader("")),
		sc:      sc,
		toCanon: openai.ResponsesTranslator{}.NewToCanonicalStream(),
		start:   time.Now(),
	}

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
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}
