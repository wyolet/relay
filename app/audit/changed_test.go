package audit

import (
	"reflect"
	"testing"
	"time"
)

type testSpec struct {
	Enabled bool   `json:"enabled"`
	Value   string `json:"value"`
	KeyHash string `json:"keyHash"`
	Weight  int    `json:"weight"`
}

type testMeta struct {
	ID          string    `json:"id"`
	DisplayName string    `json:"displayName"`
	UpdatedAt   time.Time `json:"updatedAt,omitempty"`
	CreatedAt   time.Time `json:"createdAt,omitempty"`
	Dirty       bool      `json:"dirty,omitempty"`
}

type testEntity struct {
	Meta testMeta `json:"metadata"`
	Spec testSpec `json:"spec"`
}

func TestDiffFields(t *testing.T) {
	base := func() *testEntity {
		return &testEntity{
			Meta: testMeta{ID: "p-1", DisplayName: "Prod"},
			Spec: testSpec{Enabled: true, Value: "sk-old", Weight: 3},
		}
	}

	tests := []struct {
		name     string
		mutate   func(*testEntity)
		want     []string
		wantKept []string
	}{
		{
			name:     "display name and a spec flag",
			mutate:   func(e *testEntity) { e.Meta.DisplayName = "Staging"; e.Spec.Enabled = false },
			want:     []string{"metadata.displayName", "spec.enabled"},
			wantKept: []string{"metadata.displayName", "spec.enabled"},
		},
		{
			name: "server-owned paths never appear",
			mutate: func(e *testEntity) {
				e.Meta.UpdatedAt = time.Now()
				e.Meta.CreatedAt = time.Now().Add(-time.Hour)
				e.Meta.Dirty = true
			},
			want:     nil,
			wantKept: nil,
		},
		{
			name:     "unchanged entity diffs to nothing",
			mutate:   func(*testEntity) {},
			want:     nil,
			wantKept: nil,
		},
		{
			name:     "secret-named paths are stripped",
			mutate:   func(e *testEntity) { e.Spec.Value = "sk-new"; e.Spec.KeyHash = "h"; e.Spec.Weight = 9 },
			want:     []string{"spec.keyHash", "spec.value", "spec.weight"},
			wantKept: []string{"spec.weight"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			existing, incoming := base(), base()
			tt.mutate(incoming)
			got := DiffFields(existing, incoming)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("DiffFields = %v, want %v", got, tt.want)
			}
			f := &inflight{}
			Changed(withInflight(t.Context(), f), got)
			var kept []string
			if f.change != nil {
				kept = f.change.Fields
			}
			if !reflect.DeepEqual(kept, tt.wantKept) {
				t.Fatalf("Changed kept %v, want %v", kept, tt.wantKept)
			}
		})
	}
}

func TestChangedDropsEverySecretSegment(t *testing.T) {
	paths := []string{
		"spec.keyHash", "spec.previousKeyHash", "spec.value", "spec.password",
		"spec.passwordHash", "spec.token", "spec.secret", "value", "metadata.token",
	}
	f := &inflight{}
	Changed(withInflight(t.Context(), f), append(paths, "spec.enabled"))
	if f.change == nil || !reflect.DeepEqual(f.change.Fields, []string{"spec.enabled"}) {
		t.Fatalf("kept = %+v, want only spec.enabled", f.change)
	}
}

func TestChangedWholeRow(t *testing.T) {
	f := &inflight{}
	Changed(withInflight(t.Context(), f), []string{AnyField})
	if f.change == nil || !reflect.DeepEqual(f.change.Fields, []string{"*"}) {
		t.Fatalf("fields = %+v, want [*]", f.change)
	}
}
