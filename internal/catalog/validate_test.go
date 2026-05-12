package catalog

import (
	"strings"
	"testing"
	"time"
)

// boolPtr returns a pointer to b, used for Enabled fields in tests.
func boolPtr(b bool) *bool { return &b }

// makeSnapshotWithCeiling builds a bare snapshot containing an "inference"
// ceiling RateLimit and the given candidate user RL. No providers — callers
// should call validateRateLimits directly, not the full validate().
func makeSnapshotWithCeiling(ceilingEnabled *bool, ceilingRules []RateLimitRule, userRL *RateLimit) *snapshot {
	s := newSnapshot()
	ceiling := &RateLimit{
		Metadata: Metadata{Name: "inference", Owner: Owner{Kind: OwnerSystem}},
		Spec: RateLimitSpec{
			Rules:   ceilingRules,
			Enabled: ceilingEnabled,
		},
	}
	s.rateLimits["inference"] = ceiling
	if userRL != nil {
		s.rateLimits[userRL.Metadata.Name] = userRL
	}
	return s
}

func TestValidateAgainstCeiling(t *testing.T) {
	t.Run("ceiling_disabled_user_exceeds_accepted", func(t *testing.T) {
		// Ceiling explicitly disabled → no validation, user RL is fine even if it exceeds.
		snap := makeSnapshotWithCeiling(
			boolPtr(false),
			[]RateLimitRule{{Meter: "requests", Amount: 1000, Window: time.Minute, Strategy: StrategySlidingWindow}},
			&RateLimit{
				Metadata: Metadata{Name: "user-rl", Owner: Owner{Kind: OwnerUser}},
				Spec: RateLimitSpec{
					Rules: []RateLimitRule{{Meter: "requests", Amount: 99999, Window: time.Minute, Strategy: StrategySlidingWindow}},
				},
			},
		)
		if err := validateAgainstCeiling(snap.rateLimits["user-rl"], snap); err != nil {
			t.Errorf("expected nil, got: %v", err)
		}
	})

	t.Run("user_within_ceiling_accepted", func(t *testing.T) {
		snap := makeSnapshotWithCeiling(
			nil, // nil treated as enabled
			[]RateLimitRule{{Meter: "requests", Amount: 1000, Window: time.Minute, Strategy: StrategySlidingWindow}},
			&RateLimit{
				Metadata: Metadata{Name: "user-rl", Owner: Owner{Kind: OwnerUser}},
				Spec: RateLimitSpec{
					Rules: []RateLimitRule{{Meter: "requests", Amount: 500, Window: time.Minute, Strategy: StrategySlidingWindow}},
				},
			},
		)
		if err := validateAgainstCeiling(snap.rateLimits["user-rl"], snap); err != nil {
			t.Errorf("expected nil, got: %v", err)
		}
	})

	t.Run("user_exceeds_ceiling_rejected", func(t *testing.T) {
		snap := makeSnapshotWithCeiling(
			nil,
			[]RateLimitRule{{Meter: "requests", Amount: 1000, Window: time.Minute, Strategy: StrategySlidingWindow}},
			&RateLimit{
				Metadata: Metadata{Name: "user-rl", Owner: Owner{Kind: OwnerUser}},
				Spec: RateLimitSpec{
					Rules: []RateLimitRule{{Meter: "requests", Amount: 2000, Window: time.Minute, Strategy: StrategySlidingWindow}},
				},
			},
		)
		err := validateAgainstCeiling(snap.rateLimits["user-rl"], snap)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "exceeds ceiling") {
			t.Errorf("unexpected error message: %v", err)
		}
	})

	t.Run("no_matching_meter_accepted", func(t *testing.T) {
		// Ceiling only has requests/1m; user has tokens/1m → no match → allowed.
		snap := makeSnapshotWithCeiling(
			nil,
			[]RateLimitRule{{Meter: "requests", Amount: 1000, Window: time.Minute, Strategy: StrategySlidingWindow}},
			&RateLimit{
				Metadata: Metadata{Name: "user-rl", Owner: Owner{Kind: OwnerUser}},
				Spec: RateLimitSpec{
					Rules: []RateLimitRule{{Meter: "tokens", Amount: 100000, Window: time.Minute, Strategy: StrategySlidingWindow}},
				},
			},
		)
		if err := validateAgainstCeiling(snap.rateLimits["user-rl"], snap); err != nil {
			t.Errorf("expected nil, got: %v", err)
		}
	})

	t.Run("multi_rule_one_exceeds_rejected", func(t *testing.T) {
		snap := makeSnapshotWithCeiling(
			nil,
			[]RateLimitRule{
				{Meter: "requests", Amount: 1000, Window: time.Minute, Strategy: StrategySlidingWindow},
				{Meter: "tokens", Amount: 50000, Window: time.Minute, Strategy: StrategySlidingWindow},
			},
			&RateLimit{
				Metadata: Metadata{Name: "user-rl", Owner: Owner{Kind: OwnerUser}},
				Spec: RateLimitSpec{
					Rules: []RateLimitRule{
						{Meter: "requests", Amount: 500, Window: time.Minute, Strategy: StrategySlidingWindow},  // fine
						{Meter: "tokens", Amount: 100000, Window: time.Minute, Strategy: StrategySlidingWindow}, // exceeds
					},
				},
			},
		)
		err := validateAgainstCeiling(snap.rateLimits["user-rl"], snap)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "tokens") {
			t.Errorf("expected error to cite tokens meter, got: %v", err)
		}
	})

	t.Run("system_owned_self_validation_skipped", func(t *testing.T) {
		// The inference ceiling itself must not be rejected — owner=system skips check.
		snap := makeSnapshotWithCeiling(
			nil,
			[]RateLimitRule{{Meter: "requests", Amount: 1000, Window: time.Minute, Strategy: StrategySlidingWindow}},
			nil, // don't add a second RL
		)
		// Call validateAgainstCeiling on the inference object itself.
		ceiling := snap.rateLimits["inference"]
		if err := validateAgainstCeiling(ceiling, snap); err != nil {
			t.Errorf("expected nil for system-owned self-check, got: %v", err)
		}
	})

	t.Run("no_ceiling_object_accepted", func(t *testing.T) {
		// Snapshot without inference ceiling at all — all user RLs are allowed.
		s := newSnapshot()
		userRL := &RateLimit{
			Metadata: Metadata{Name: "user-rl", Owner: Owner{Kind: OwnerUser}},
			Spec: RateLimitSpec{
				Rules: []RateLimitRule{{Meter: "requests", Amount: 99999, Window: time.Minute, Strategy: StrategySlidingWindow}},
			},
		}
		s.rateLimits["user-rl"] = userRL
		if err := validateAgainstCeiling(userRL, s); err != nil {
			t.Errorf("expected nil when ceiling absent, got: %v", err)
		}
	})

	t.Run("validateRateLimits_integration_ceiling_check", func(t *testing.T) {
		// Full validateRateLimits call with ceiling + violating user RL.
		snap := makeSnapshotWithCeiling(
			nil,
			[]RateLimitRule{{Meter: "requests", Amount: 1000, Window: time.Minute, Strategy: StrategySlidingWindow}},
			&RateLimit{
				Metadata: Metadata{Name: "bad-rl", Owner: Owner{Kind: OwnerUser}},
				Spec: RateLimitSpec{
					Rules: []RateLimitRule{{Meter: "requests", Amount: 5000, Window: time.Minute, Strategy: StrategySlidingWindow}},
				},
			},
		)
		err := validateRateLimits(snap)
		if err == nil {
			t.Fatal("expected error from validateRateLimits, got nil")
		}
		if !strings.Contains(err.Error(), "exceeds ceiling") {
			t.Errorf("unexpected error: %v", err)
		}
	})
}

// TestValidateRateLimits_OwnerKind verifies owner-kind validation.
func TestValidateRateLimits_OwnerKind(t *testing.T) {
	t.Run("valid_user_owner", func(t *testing.T) {
		s := newSnapshot()
		s.rateLimits["u"] = &RateLimit{
			Metadata: Metadata{Name: "u", Owner: Owner{Kind: OwnerUser}},
			Spec: RateLimitSpec{
				Rules: []RateLimitRule{{Meter: "requests", Amount: 10, Window: time.Minute, Strategy: StrategySlidingWindow}},
			},
		}
		if err := validateRateLimits(s); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("invalid_owner_kind_rejected", func(t *testing.T) {
		s := newSnapshot()
		s.rateLimits["u"] = &RateLimit{
			Metadata: Metadata{Name: "u", Owner: Owner{Kind: "robot"}},
			Spec: RateLimitSpec{
				Rules: []RateLimitRule{{Meter: "requests", Amount: 10, Window: time.Minute, Strategy: StrategySlidingWindow}},
			},
		}
		err := validateRateLimits(s)
		if err == nil || !strings.Contains(err.Error(), "owner.kind") {
			t.Errorf("expected owner.kind error, got: %v", err)
		}
	})

	t.Run("provider_owner_requires_id", func(t *testing.T) {
		s := newSnapshot()
		s.rateLimits["u"] = &RateLimit{
			Metadata: Metadata{Name: "u", Owner: Owner{Kind: OwnerProvider}},
			Spec: RateLimitSpec{
				Rules: []RateLimitRule{{Meter: "requests", Amount: 10, Window: time.Minute, Strategy: StrategySlidingWindow}},
			},
		}
		err := validateRateLimits(s)
		if err == nil || !strings.Contains(err.Error(), "owner.id") {
			t.Errorf("expected owner.id error, got: %v", err)
		}
	})
}

// TestValidateRateLimits_RequiredRuleFields verifies per-rule window+strategy required.
func TestValidateRateLimits_RequiredRuleFields(t *testing.T) {
	t.Run("missing_window_rejected", func(t *testing.T) {
		s := newSnapshot()
		s.rateLimits["u"] = &RateLimit{
			Metadata: Metadata{Name: "u", Owner: Owner{Kind: OwnerUser}},
			Spec: RateLimitSpec{
				Rules: []RateLimitRule{{Meter: "requests", Amount: 10, Strategy: StrategySlidingWindow}},
			},
		}
		err := validateRateLimits(s)
		if err == nil || !strings.Contains(err.Error(), "window is required") {
			t.Errorf("expected window error, got: %v", err)
		}
	})

	t.Run("missing_strategy_rejected", func(t *testing.T) {
		s := newSnapshot()
		s.rateLimits["u"] = &RateLimit{
			Metadata: Metadata{Name: "u", Owner: Owner{Kind: OwnerUser}},
			Spec: RateLimitSpec{
				Rules: []RateLimitRule{{Meter: "requests", Amount: 10, Window: time.Minute}},
			},
		}
		err := validateRateLimits(s)
		if err == nil || !strings.Contains(err.Error(), "strategy is required") {
			t.Errorf("expected strategy error, got: %v", err)
		}
	})
}
