package catalogserver

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/wyolet/relay/pkg/httpmw"
)

const maxHookBody = 4 << 10

// webhooks holds what registration and nudges need: who may register, where
// receivers are dialed from, and the receivers themselves.
type webhooks struct {
	policy      receiverPolicy
	client      *http.Client
	userAgent   string
	timeout     time.Duration // per challenge or nudge request
	subs        *subscriptions
	retryDelays []time.Duration // waits before each retry of a failed nudge
	sendSlots   chan struct{}

	mu          sync.Mutex
	deliverySeq map[string]uint64 // per channel, the newest delivery's number
}

type registerRequest struct {
	URL     string `json:"url"`
	Channel string `json:"channel"`
}

func (s *Server) serveRegister(w http.ResponseWriter, r *http.Request) {
	h := s.hooks
	client, _ := netip.ParseAddr(httpmw.ClientIP(r, s.cfg.TrustedProxies))
	if !h.subs.allowRegistration(client, s.now()) {
		s.registrationFailed(w, "rate_limited", http.StatusTooManyRequests, "too many registrations; retry in a minute")
		return
	}
	var req registerRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxHookBody)).Decode(&req); err != nil {
		s.registrationFailed(w, "invalid", http.StatusBadRequest, "body must be JSON {\"url\", \"channel\"}")
		return
	}
	if !s.knowsChannel(req.Channel) {
		s.registrationFailed(w, "invalid", http.StatusBadRequest, "unknown channel")
		return
	}
	target, err := h.policy.parseURL(req.URL)
	if err != nil {
		s.registrationFailed(w, "invalid", http.StatusBadRequest, "invalid url: "+err.Error())
		return
	}
	addr, err := h.policy.resolve(r.Context(), target.Hostname())
	switch {
	case errors.Is(err, errRefusedAddr):
		s.registrationFailed(w, "refused", http.StatusForbidden, "url resolves to a refused address")
		return
	case err != nil:
		s.registrationFailed(w, "invalid", http.StatusBadRequest, "url host does not resolve")
		return
	}
	key := subKey{url: target.String(), channel: req.Channel}
	if !h.subs.hasRoom(key) {
		s.registrationFailed(w, "full", http.StatusServiceUnavailable, "subscription limit reached")
		return
	}
	if err := h.challenge(r.Context(), key.url, addr); err != nil {
		s.log.Info("catalog-server: webhook challenge failed", "url", key.url, "err", err)
		s.registrationFailed(w, "challenge_failed", http.StatusForbidden, "challenge failed")
		return
	}
	expires, renewed, ok := h.subs.put(key, addr, s.now())
	if !ok {
		s.registrationFailed(w, "full", http.StatusServiceUnavailable, "subscription limit reached")
		return
	}
	result := "created"
	if renewed {
		result = "renewed"
	}
	s.metrics.registrations.WithLabelValues(result).Inc()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]string{"expiresAt": expires.UTC().Format(time.RFC3339)})
}

func (s *Server) registrationFailed(w http.ResponseWriter, result string, code int, msg string) {
	s.metrics.registrations.WithLabelValues(result).Inc()
	http.Error(w, msg, code)
}

func (s *Server) knowsChannel(channel string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.latest[channel]
	return ok
}

// challenge proves the receiver answers for its URL: it must echo a fresh
// random value within hookTimeout.
func (h *webhooks) challenge(ctx context.Context, url string, addr netip.Addr) error {
	var raw [16]byte
	_, _ = rand.Read(raw[:])
	want := hex.EncodeToString(raw[:])
	status, body, err := h.post(ctx, url, addr, map[string]string{"type": "challenge", "challenge": want})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("receiver answered %d", status)
	}
	var echo struct {
		Challenge string `json:"challenge"`
	}
	if err := json.Unmarshal(body, &echo); err != nil || echo.Challenge != want {
		return errors.New("receiver did not echo the challenge")
	}
	return nil
}

// post sends payload to url over addr and returns the status and up to
// maxHookBody of the answer.
func (h *webhooks) post(ctx context.Context, url string, addr netip.Addr, payload any) (int, []byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, err
	}
	ctx, cancel := context.WithTimeout(withPinnedAddr(ctx, addr), h.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", h.userAgent)
	resp, err := h.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, maxHookBody))
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, answer, nil
}

// serveGitHubHook asks for an early sync. The payload is never read: the
// sync fetches the repository itself, so a forged call costs one fetch.
func (s *Server) serveGitHubHook(w http.ResponseWriter, _ *http.Request) {
	s.metrics.githubHooks.Inc()
	s.requestSync()
	w.WriteHeader(http.StatusAccepted)
}
