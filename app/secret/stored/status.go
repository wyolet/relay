package stored

import (
	"context"
	"fmt"
	"time"

	"github.com/wyolet/relay/internal/storage/gen"
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
	// UndecryptableRows are the ids of stored secrets the answering process cannot decrypt: legacy rows none of its master keys opens, and rows under a data key that is locked here, destroyed or unknown.
	UndecryptableRows []string `json:"undecryptableRows"`
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
			out.HeldKEKs = append(out.HeldKEKs, FormatKEKID(id))
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
		out.TargetKEK = FormatKEKID(t)
	}
	counts, err := s.q.CountSecretValuesByDataKey(ctx)
	if err != nil {
		return out, fmt.Errorf("secret: count rows per data key: %w", err)
	}
	rows := make(map[[16]byte]int64, len(counts))
	for _, c := range counts {
		rows[c.DekID.Bytes] = c.Rows
	}
	keys := ring.Keys()
	for _, k := range keys {
		out.DataKeys = append(out.DataKeys, dataKeyStatus(k, rows[k.ID]))
	}
	out.UndecryptableRows, err = s.undecryptableRows(ctx, ring, keys)
	return out, err
}

// undecryptableRows tries every legacy row against the held master keys; a sealed row is judged by its data key alone, without decrypting it.
func (s *Backend) undecryptableRows(ctx context.Context, ring *keyring.Ring, keys []keyring.KeyInfo) ([]string, error) {
	usable := make(map[[16]byte]bool, len(keys))
	for _, k := range keys {
		usable[k.ID] = !k.Locked && k.State != keyring.Destroyed
	}
	out := []string{}
	after := ""
	for {
		page, err := s.q.ListSecretValues(ctx, gen.ListSecretValuesParams{After: after, MaxRows: rewriteBatch})
		if err != nil {
			return out, fmt.Errorf("secret: list stored secrets: %w", err)
		}
		if len(page) == 0 {
			return out, nil
		}
		for _, r := range page {
			after = r.ID
			if r.DekID.Valid {
				if !usable[r.DekID.Bytes] {
					out = append(out, r.ID)
				}
				continue
			}
			if _, _, err := ring.DecryptLegacy(r.Ciphertext, r.Nonce); err != nil {
				out = append(out, r.ID)
			}
		}
	}
}

func dataKeyStatus(k keyring.KeyInfo, rows int64) DataKeyStatus {
	return DataKeyStatus{
		ID:        keyring.FormatID(k.ID),
		Scope:     k.Scope,
		State:     string(k.State),
		KEK:       FormatKEKID(k.KEKID),
		CreatedAt: k.CreatedAt,
		DemotedAt: k.DemotedAt,
		Rows:      rows,
		Locked:    k.Locked,
	}
}
