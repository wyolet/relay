package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/wyolet/relay/internal/catalog"
	"github.com/wyolet/relay/internal/ratelimit"
	"github.com/wyolet/relay/pkg/reqid"
)

type reloader interface {
	Reload(ctx context.Context) error
}

// adminReloadRules constructs the synthetic ResolvedRule slice for the /admin/reload limiter.
// ParentKind=Admin, ParentName=reload, meter=requests, sliding-window of 60s.
func adminReloadRules(rpm int64) []catalog.ResolvedRule {
	rl := &catalog.RateLimit{
		APIVersion: catalog.APIVersion,
		Kind:       catalog.KindRateLimit,
		Metadata:   catalog.Metadata{Name: "admin-reload-rpm"},
		Spec: catalog.RateLimitSpec{
			Strategy: catalog.StrategySlidingWindow,
			Window:   60 * time.Second,
			Rules:    []catalog.RateLimitRule{{Meter: string(catalog.MeterRequests), Amount: rpm}},
		},
	}
	return []catalog.ResolvedRule{
		{
			ParentKind: "Admin",
			ParentName: "reload",
			Meter:      catalog.MeterRequests,
			RateLimit:  rl,
			Rule:       rl.Spec.Rules[0],
		},
	}
}

// sourceIP resolves the client IP from X-Forwarded-For (first hop) or RemoteAddr.
// Precedence: X-Forwarded-For leftmost non-empty token > RemoteAddr (port stripped).
func sourceIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		for _, p := range parts {
			if ip := strings.TrimSpace(p); ip != "" {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// adminReloadHandler returns an http.HandlerFunc that calls store.Reload.
// token must be non-empty; callers are responsible for not registering when token is empty.
// lim enforces a per-source-IP rate limit using the provided rpm (default 10).
func adminReloadHandler(token string, store reloader, lim *ratelimit.Limiter, rpm int) http.HandlerFunc {
	tok := []byte(token)
	if rpm <= 0 {
		rpm = 10
	}
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		log := reqid.Logger(ctx)
		ip := sourceIP(r)

		// Admin token may be passed as X-Relay-Admin-Token or as Authorization: Bearer.
		// When the outer caller-auth middleware is active, Authorization is consumed by
		// the API-key check; operators should use X-Relay-Admin-Token for the admin secret.
		adminTok := r.Header.Get("X-Relay-Admin-Token")
		if adminTok == "" {
			adminTok = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
		if subtle.ConstantTimeCompare([]byte(adminTok), tok) != 1 {
			log.Warn("admin/reload: unauthorized", "ip", ip)
			http.NotFound(w, r)
			return
		}

		// Rate-limit by source IP. Build rules with IP-scoped parent name.
		rules := adminReloadRules(int64(rpm))
		// Scope state key to source IP by embedding it in ParentName.
		rules[0].ParentName = fmt.Sprintf("reload:%s", ip)

		res, err := lim.Reserve(ctx, "admin", rules)
		if err != nil {
			var exceeded *ratelimit.ExceededError
			if errors.As(err, &exceeded) {
				retryAfterSec := int(exceeded.RetryAfter.Seconds())
				if retryAfterSec < 1 {
					retryAfterSec = 1
				}
				log.Warn("admin/reload: rate limited", "ip", ip, "retry_after_s", retryAfterSec)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", strconv.Itoa(retryAfterSec))
				w.WriteHeader(http.StatusTooManyRequests)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error": map[string]string{
						"type":    "rate_limit_exceeded",
						"code":    "admin_rate_limit_exceeded",
						"message": fmt.Sprintf("admin reload rate limit exceeded; retry after %ds", retryAfterSec),
					},
				})
				return
			}
			// Reserve error (non-exceeded): log and proceed fail-open.
			log.Warn("admin/reload: limiter reserve error (fail-open)", "ip", ip, "err", err)
		} else {
			defer func() { _ = lim.Commit(ctx, res, ratelimit.Observations{}) }()
		}

		if err := store.Reload(r.Context()); err != nil {
			log.Error("admin/reload: failed", "ip", ip, "err", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}

		log.Info("admin/reload: ok", "ip", ip)
		w.WriteHeader(http.StatusOK)
	}
}
