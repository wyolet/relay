package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/wyolet/relay/pkg/kv"
)

// sessionKV is the part of kv.Store the session store uses.
type sessionKV interface {
	Get(ctx context.Context, key string) ([]byte, error)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	Del(ctx context.Context, key string) error
	Range(ctx context.Context, prefix string) ([]kv.Entry, error)
}

// kvStore implements the auth module's session.Store on kv. scs's store methods take no context, so each gets a short timeout of its own.
type kvStore struct {
	kv sessionKV
}

type kvEntry struct {
	Data   []byte    `json:"d"`
	Expiry time.Time `json:"e"`
}

const storeTimeout = 2 * time.Second

func (s *kvStore) Find(token string) ([]byte, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	defer cancel()
	raw, err := s.kv.Get(ctx, sessionKey(token))
	if err != nil {
		// scs convention: (nil, false, nil) for "no such session."
		if errors.Is(err, kv.ErrNotFound) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("session find (kv get): %w", err)
	}
	if raw == nil {
		return nil, false, nil
	}
	var e kvEntry
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil, false, fmt.Errorf("session find (decode): %w", err)
	}
	if time.Now().After(e.Expiry) {
		return nil, false, nil
	}
	return e.Data, true, nil
}

func (s *kvStore) Commit(token string, b []byte, expiry time.Time) error {
	ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	defer cancel()
	raw, err := json.Marshal(kvEntry{Data: b, Expiry: expiry})
	if err != nil {
		return fmt.Errorf("session commit (encode): %w", err)
	}
	if err := s.kv.Set(ctx, sessionKey(token), raw, time.Until(expiry)); err != nil {
		return fmt.Errorf("session commit (kv set): %w", err)
	}
	return nil
}

func (s *kvStore) Delete(token string) error {
	ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	defer cancel()
	if err := s.kv.Del(ctx, sessionKey(token)); err != nil {
		return fmt.Errorf("session delete (kv del): %w", err)
	}
	return nil
}

func (s *kvStore) IndexUserToken(ctx context.Context, userID, token string, ttl time.Duration) error {
	return s.kv.Set(ctx, userIndexKey(userID, token), []byte(token), ttl)
}

func (s *kvStore) UserTokens(ctx context.Context, userID string) ([]string, error) {
	entries, err := s.kv.Range(ctx, userIndexPrefix(userID))
	if err != nil {
		return nil, fmt.Errorf("session destroy user (kv range): %w", err)
	}
	tokens := make([]string, len(entries))
	for i, e := range entries {
		tokens[i] = string(e.Value)
	}
	return tokens, nil
}

func (s *kvStore) DeleteUserToken(ctx context.Context, userID, token string) error {
	if err := s.kv.Del(ctx, sessionKey(token)); err != nil {
		return fmt.Errorf("session destroy user (kv del): %w", err)
	}
	if err := s.kv.Del(ctx, userIndexKey(userID, token)); err != nil {
		return fmt.Errorf("session destroy user (kv del): %w", err)
	}
	return nil
}
