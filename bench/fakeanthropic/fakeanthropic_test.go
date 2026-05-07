package fakeanthropic_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/wyolet/relay/bench/fakeanthropic"
)

func fixture(t *testing.T) []json.RawMessage {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	path := filepath.Join(filepath.Dir(file), "testdata", "session.jsonl")
	responses, err := fakeanthropic.LoadSession(path)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if len(responses) == 0 {
		t.Fatal("no assistant turns found in fixture")
	}
	return responses
}

func TestNonStreaming_ValidJSON(t *testing.T) {
	srv := httptest.NewServer(fakeanthropic.New(fixture(t)).Handler())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("x-api-key", "test-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("want application/json, got %q", ct)
	}

	body, _ := io.ReadAll(resp.Body)
	var msg map[string]any
	if err := json.Unmarshal(body, &msg); err != nil {
		t.Fatalf("response not valid JSON: %v\nbody: %s", err, body)
	}
	if msg["type"] != "message" {
		t.Errorf("want type=message, got %v", msg["type"])
	}
	if msg["role"] != "assistant" {
		t.Errorf("want role=assistant, got %v", msg["role"])
	}
}

func TestNonStreaming_MissingAPIKey_401(t *testing.T) {
	srv := httptest.NewServer(fakeanthropic.New(fixture(t)).Handler())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(`{}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", resp.StatusCode)
	}
}

func TestStreaming_SSEWellFormed(t *testing.T) {
	srv := httptest.NewServer(fakeanthropic.New(fixture(t)).Handler())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(`{"stream":true}`))
	req.Header.Set("x-api-key", "test-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("want text/event-stream, got %q", ct)
	}

	var events []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "event: ") {
			events = append(events, strings.TrimPrefix(line, "event: "))
		}
	}

	if len(events) == 0 {
		t.Fatal("no SSE events received")
	}
	if events[0] != "message_start" {
		t.Errorf("first event should be message_start, got %q", events[0])
	}
	last := events[len(events)-1]
	if last != "message_stop" {
		t.Errorf("last event should be message_stop, got %q", last)
	}

	// Verify message_start data is valid JSON with expected shape.
	sc2 := bufio.NewScanner(strings.NewReader(""))
	_ = sc2

	// Re-read to check data lines are valid JSON.
	req2, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(`{"stream":true}`))
	req2.Header.Set("x-api-key", "test-key")
	resp2, _ := http.DefaultClient.Do(req2)
	defer resp2.Body.Close()

	sc3 := bufio.NewScanner(resp2.Body)
	for sc3.Scan() {
		line := sc3.Text()
		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			var v map[string]any
			if err := json.Unmarshal([]byte(data), &v); err != nil {
				t.Errorf("SSE data line not valid JSON: %q: %v", data, err)
			}
		}
	}
}

func TestRoundRobin(t *testing.T) {
	responses := fixture(t)
	srv := httptest.NewServer(fakeanthropic.New(responses).Handler())
	defer srv.Close()

	ids := make([]string, 0, len(responses)+1)
	for i := 0; i <= len(responses); i++ {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(`{}`))
		req.Header.Set("x-api-key", "key")
		resp, _ := http.DefaultClient.Do(req)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var msg map[string]any
		_ = json.Unmarshal(body, &msg)
		ids = append(ids, fmt.Sprint(msg["id"]))
	}
	// After len(responses) calls, the next should wrap around to the first.
	if ids[0] != ids[len(responses)] {
		t.Errorf("round-robin failed: first=%q, wrapped=%q", ids[0], ids[len(responses)])
	}
}

