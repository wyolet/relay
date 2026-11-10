// Package cataloghook is the relay side of catalog release webhooks: the
// public endpoint the catalog server POSTs challenges and release nudges to,
// and the registration that subscribes this relay to them.
//
// A nudge only asks for a catalog check; the check (app/catalog.Follower)
// re-reads the index and verifies sums itself, so nothing in a request body
// is trusted and the endpoint needs no authentication. Out of scope: the
// check, and the catalog server's side of the contract.
package cataloghook

import (
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"
)

// ReleasePath is where the receiver is mounted on the inference listener.
const ReleasePath = "/catalog/hooks/release"

const (
	// maxBodyBytes caps a request body; both message types fit in far less.
	maxBodyBytes = 4 << 10
	// nudgeInterval bounds the checks nudges start on one pod, so a flood of
	// forged nudges costs at most one index read per interval.
	nudgeInterval = 10 * time.Second
)

type hookMessage struct {
	Type      string `json:"type"`
	Challenge string `json:"challenge"`
	Channel   string `json:"channel"`
	Tag       string `json:"tag"`
}

// ReleaseHandler answers the catalog server's challenge and release
// messages. A release on channel calls nudge with its tag at most once per
// 10 s; nudge must not wait for the check it starts.
func ReleaseHandler(channel string, nudge func(tag string)) http.Handler {
	return newReceiver(channel, nudge, time.Now)
}

type receiver struct {
	channel string
	nudge   *debouncer
}

func newReceiver(channel string, nudge func(tag string), now func() time.Time) *receiver {
	return &receiver{channel: channel, nudge: &debouncer{fn: nudge, every: nudgeInterval, now: now}}
}

func (h *receiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil || len(body) > maxBodyBytes {
		http.Error(w, "body must be a JSON object of at most 4 KiB", http.StatusBadRequest)
		return
	}
	var msg hookMessage
	if err := json.Unmarshal(body, &msg); err != nil {
		http.Error(w, "body must be a JSON object", http.StatusBadRequest)
		return
	}
	switch msg.Type {
	case "challenge":
		if msg.Challenge == "" {
			http.Error(w, "challenge is required", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			Challenge string `json:"challenge"`
		}{msg.Challenge})
	case "release":
		if msg.Channel == h.channel {
			h.nudge.fire(msg.Tag)
		}
		w.WriteHeader(http.StatusAccepted)
	default:
		http.Error(w, "type must be challenge or release", http.StatusBadRequest)
	}
}

// debouncer calls fn at most once per every; calls in between are dropped
// with their tag.
type debouncer struct {
	fn    func(tag string)
	every time.Duration
	now   func() time.Time

	mu   sync.Mutex
	last time.Time
}

func (d *debouncer) fire(tag string) {
	d.mu.Lock()
	n := d.now()
	if !d.last.IsZero() && n.Sub(d.last) < d.every {
		d.mu.Unlock()
		return
	}
	d.last = n
	d.mu.Unlock()
	d.fn(tag)
}
