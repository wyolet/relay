package stored

import (
	"context"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/wyolet/relay/secret/keyring"
)

// KeyStatus describes the key ring for operators. It never carries key material.
type KeyStatus struct {
	DataKeys []DataKeyStatus `json:"dataKeys"`
	// TargetKEK is the KEK new data keys are wrapped under; empty until the first data key exists.
	TargetKEK string `json:"targetKek,omitempty"`
	// HeldKEKs are the KEKs this process was configured with, RELAY_MASTER_KEY first.
	HeldKEKs []string `json:"heldKeks"`
	// LegacyRows are stored secrets still in the master-key shape.
	LegacyRows int64 `json:"legacyRows"`
}

// DataKeyStatus is one data key and how many stored secrets it seals.
type DataKeyStatus struct {
	ID        string    `json:"id"`
	Scope     string    `json:"scope"`
	State     string    `json:"state"`
	KEK       string    `json:"kek"`
	CreatedAt time.Time `json:"createdAt"`
	DemotedAt time.Time `json:"demotedAt,omitzero"`
	Rows      int64     `json:"rows"`
	// Locked is a live key this process cannot unwrap, normally because it lacks the key's KEK.
	Locked bool `json:"locked"`
}

// Status reads the key ring and the stored secrets' distribution over it, fresh from the database.
func (s *Backend) Status(ctx context.Context) (KeyStatus, error) {
	var out KeyStatus
	if s.keys.set != nil {
		for _, id := range s.keys.set.IDs() {
			out.HeldKEKs = append(out.HeldKEKs, hex.EncodeToString(id[:]))
		}
	}
	n, err := s.q.CountLegacySecretValues(ctx)
	if err != nil {
		return out, fmt.Errorf("secret: count legacy rows: %w", err)
	}
	out.LegacyRows = n
	ring, err := s.keys.load(ctx)
	if err != nil {
		return out, err
	}
	if err := ring.ReloadAll(ctx); err != nil {
		return out, err
	}
	if t := ring.Target(); t != ([16]byte{}) {
		out.TargetKEK = hex.EncodeToString(t[:])
	}
	counts, err := s.q.CountSecretValuesByDataKey(ctx)
	if err != nil {
		return out, fmt.Errorf("secret: count rows per data key: %w", err)
	}
	rows := make(map[[16]byte]int64, len(counts))
	for _, c := range counts {
		rows[c.DekID.Bytes] = c.Rows
	}
	for _, k := range ring.Keys() {
		out.DataKeys = append(out.DataKeys, DataKeyStatus{
			ID:        keyring.FormatID(k.ID),
			Scope:     k.Scope,
			State:     string(k.State),
			KEK:       hex.EncodeToString(k.KEKID[:]),
			CreatedAt: k.CreatedAt,
			DemotedAt: k.DemotedAt,
			Rows:      rows[k.ID],
			Locked:    k.Locked,
		})
	}
	return out, nil
}
