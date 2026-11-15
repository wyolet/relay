//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wyolet/relay/app/lastused"
	"github.com/wyolet/relay/internal/storage/gen"
)

// A served request shows on its relay key and host key after a flush, and the flush notifies no pod.
func TestE2E_LastUsedAfterAFlush(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c","object":"chat.completion","model":"test-model","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer upstream.Close()

	st := newStack(t)
	bearer := st.seedHappyPath(upstream.URL, "sk-mock-upstream-key")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	keyID, hostKeyID := idByName(t, st, "keys", "test-key"), idByName(t, st, "host-keys", "test-hostkey")

	rec := lastused.NewRecorder(0)
	st.lifecycle.RegisterCollector(rec)
	pool := testPool(t, st.dsn)
	store := lastused.NewStore(gen.New(pool))
	flusher := lastused.NewFlusher(rec, store, time.Hour, nil)

	listener, err := pgx.Connect(ctx, st.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close(context.Background())
	if _, err := listener.Exec(ctx, "LISTEN catalog_events"); err != nil {
		t.Fatal(err)
	}

	req, _ := http.NewRequest(http.MethodPost, st.inference.URL+"/v1/chat/completions",
		bytes.NewReader([]byte(`{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	keyPath, hostKeyPath := "/api/keys/"+keyID, "/api/host-keys/"+hostKeyID
	// Post-flight runs detached after the body closes; flush until it has noted the request.
	for {
		if err := flusher.Flush(ctx); err != nil {
			t.Fatal(err)
		}
		if lastUsedOf(t, st, keyPath) != nil {
			break
		}
		if ctx.Err() != nil {
			t.Fatal("the key never showed as used")
		}
		time.Sleep(20 * time.Millisecond)
	}

	used := map[string]time.Time{}
	for _, path := range []string{keyPath, hostKeyPath} {
		at := lastUsedOf(t, st, path)
		if at == nil {
			t.Fatalf("GET %s has no status.lastUsedAt", path)
		}
		if age := time.Since(*at); age < 0 || age > 2*time.Minute || at.Second() != 0 {
			t.Errorf("GET %s status.lastUsedAt = %v, want this minute", path, at)
		}
		used[path] = *at
	}
	for _, plural := range []string{"keys", "host-keys"} {
		code, body := st.adminDo(http.MethodGet, "/api/"+plural, "")
		var list struct {
			Items []struct {
				Metadata   struct{ ID string } `json:"metadata"`
				LastUsedAt *time.Time          `json:"lastUsedAt"`
				Status     struct {
					LastUsedAt *time.Time `json:"lastUsedAt"`
				} `json:"status"`
			} `json:"items"`
		}
		if code != http.StatusOK || json.Unmarshal(body, &list) != nil || len(list.Items) == 0 {
			t.Fatalf("GET /api/%s = %d %s", plural, code, body)
		}
		for _, it := range list.Items {
			if it.Status.LastUsedAt == nil || it.LastUsedAt != nil {
				t.Errorf("GET /api/%s: %s has status.lastUsedAt %v and top-level %v, want only status", plural, it.Metadata.ID, it.Status.LastUsedAt, it.LastUsedAt)
			}
		}
	}

	wait, stop := context.WithTimeout(ctx, 500*time.Millisecond)
	defer stop()
	for {
		n, err := listener.WaitForNotification(wait)
		if err != nil {
			break
		}
		if strings.Contains(n.Payload, keyID) || strings.Contains(n.Payload, hostKeyID) {
			t.Errorf("notification %q after the last-used flush, want none for the keys", n.Payload)
		}
	}

	// An operator PUT, echoing the read or leaving status out, keeps the recorded time.
	for path, put := range map[string]string{keyPath: "/api/keys/by-id/" + keyID, hostKeyPath: "/api/host-keys/by-id/" + hostKeyID} {
		for _, echo := range []bool{true, false} {
			code, body := st.adminDo(http.MethodGet, path, "")
			var row map[string]any
			if code != http.StatusOK || json.Unmarshal(body, &row) != nil {
				t.Fatalf("GET %s = %d %s", path, code, body)
			}
			row["metadata"].(map[string]any)["displayName"] = "edited"
			if !echo {
				delete(row, "status")
			}
			edited, _ := json.Marshal(row)
			if code, body := st.adminDo(http.MethodPut, put, string(edited)); code != http.StatusOK {
				t.Fatalf("PUT %s = %d %s", put, code, body)
			}
			if at := lastUsedOf(t, st, path); at == nil || !at.Equal(used[path]) {
				t.Errorf("after PUT %s (status echoed: %v) status.lastUsedAt = %v, want %v", put, echo, at, used[path])
			}
		}
	}
}

// lastUsedOf reads a row's status.lastUsedAt through the control API.
func lastUsedOf(t *testing.T, st *stack, path string) *time.Time {
	t.Helper()
	code, body := st.adminDo(http.MethodGet, path, "")
	var row struct {
		Status struct {
			LastUsedAt *time.Time `json:"lastUsedAt"`
		} `json:"status"`
	}
	if code != http.StatusOK || json.Unmarshal(body, &row) != nil {
		t.Fatalf("GET %s = %d %s", path, code, body)
	}
	return row.Status.LastUsedAt
}

// idByName finds a row's id through the control API list.
func idByName(t *testing.T, st *stack, plural, name string) string {
	t.Helper()
	code, body := st.adminDo(http.MethodGet, "/api/"+plural+"/"+name, "")
	var row struct {
		Metadata struct{ ID string } `json:"metadata"`
	}
	if code != http.StatusOK || json.Unmarshal(body, &row) != nil || row.Metadata.ID == "" {
		t.Fatalf("GET /api/%s/%s = %d %s", plural, name, code, body)
	}
	return row.Metadata.ID
}
