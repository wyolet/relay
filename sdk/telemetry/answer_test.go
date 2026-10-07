package telemetry

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// answering makes the collector send a content-capture answer on every successful response.
func answering(c *collector, value string) {
	c.respond = func(path string, n int, w http.ResponseWriter) bool {
		if value != "" {
			w.Header().Set(HeaderContentCapture, value)
		}
		w.WriteHeader(http.StatusOK)
		return true
	}
}

// waitForAnswer waits until the emitter has learned an answer.
func waitForAnswer(t *testing.T, e *Emitter) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !e.answerFresh() {
		if time.Now().After(deadline) {
			t.Fatal("no answer learned")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestStoreAnswerSendsContentWithoutTheOptIn(t *testing.T) {
	c := newCollector(t)
	answering(c, ContentStore)
	e := newTestEmitter(t, c, WithContent(false))
	if e.SendsContent() {
		t.Fatal("SendsContent before any answer = true, want the opt-in (false)")
	}
	waitForAnswer(t, e)
	if !e.SendsContent() {
		t.Fatal("SendsContent after \"store\" = false")
	}
	e.Record(withContent(call("m")))
	if err := e.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	spans, events, probes := c.counts()
	if spans != 1 || events != 1 || probes != 1 {
		t.Errorf("spans %d events %d probes %d, want 1 1 1", spans, events, probes)
	}
	if first := c.seen()[0]; first.path != "/otlp/v1/logs" || len(first.logs.ResourceLogs) != 0 {
		t.Errorf("first request = %s, want the empty logs probe", first.path)
	}
}

func TestDropAnswerWithholdsContentDespiteTheOptIn(t *testing.T) {
	c := newCollector(t)
	answering(c, ContentDrop)
	e := newTestEmitter(t, c, WithContent(true))
	// Unknown answer and an opted-in client: content is built, then withheld at export once relay says drop.
	if !e.SendsContent() {
		t.Fatal("SendsContent before any answer = false, want the opt-in (true)")
	}
	e.Record(withContent(call("m")))
	if err := e.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, events, _ := c.counts(); events != 0 {
		t.Errorf("sent %d content events under \"drop\"", events)
	}
	if e.SendsContent() {
		t.Error("SendsContent under \"drop\" = true")
	}
}

func TestClientAnswerAndAbsentHeaderFollowTheOptIn(t *testing.T) {
	for _, value := range []string{ContentClient, ""} {
		for _, optIn := range []bool{true, false} {
			c := newCollector(t)
			answering(c, value)
			e := newTestEmitter(t, c, WithContent(optIn))
			e.Record(withContent(call("m")))
			if err := e.Flush(context.Background()); err != nil {
				t.Fatal(err)
			}
			_, events, _ := c.counts()
			if want := map[bool]int{true: 1, false: 0}[optIn]; events != want || e.SendsContent() != optIn {
				t.Errorf("answer %q opt-in %v: %d events, SendsContent %v", value, optIn, events, e.SendsContent())
			}
		}
	}
}

func TestAnswerIsCachedAndRelearnedWhenStale(t *testing.T) {
	c := newCollector(t)
	answering(c, ContentClient)
	e := newTestEmitter(t, c)
	for range 3 {
		e.Record(call("m"))
		if err := e.Flush(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, probes := c.counts(); probes != 1 {
		t.Errorf("probes = %d, want one while the answer is fresh", probes)
	}
	// Exports refresh the answer, so only an answer older than the cache window is probed again.
	e.answer.Store(&contentAnswer{value: ContentClient, at: time.Now().Add(-time.Hour)})
	e.Record(call("m"))
	if err := e.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, probes := c.counts(); probes != 2 {
		t.Errorf("probes = %d, want a second one after expiry", probes)
	}
}

func TestFailedProbeIsNotRepeatedPerBatch(t *testing.T) {
	c := newCollector(t)
	c.respond = func(path string, n int, w http.ResponseWriter) bool {
		if path == "/otlp/v1/logs" {
			w.WriteHeader(http.StatusNotFound)
			return true
		}
		return false
	}
	e := newTestEmitter(t, c, WithContent(true))
	e.answer.Store(nil)
	e.learn(context.Background())
	if !e.answerFresh() || !e.SendsContent() {
		t.Error("a failed probe must be cached and leave the opt-in in charge")
	}
}
