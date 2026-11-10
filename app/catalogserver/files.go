package catalogserver

import (
	"context"
	"errors"
	"time"
)

// file returns the verified bytes of one release file. Concurrent callers
// share one build.
func (s *Server) file(ctx context.Context, tag string, a asset) ([]byte, error) {
	k := fileKey{tag, a}
	if b, ok := s.cache.get(k); ok {
		return b, nil
	}
	ch := s.builds.DoChan(tag+"/"+a.label(), func() (any, error) {
		if b, ok := s.cache.get(k); ok {
			return b, nil
		}
		b, err := s.produce(tag, a)
		if err != nil {
			return nil, err
		}
		s.cache.put(k, b)
		return b, nil
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-ch:
		if r.Err != nil {
			return nil, r.Err
		}
		return r.Val.([]byte), nil
	}
}

// produce runs detached from any one request because callers share it.
func (s *Server) produce(tag string, a asset) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), buildTime)
	defer cancel()
	s.mu.RLock()
	st := s.tags[tag]
	if st == nil {
		s.mu.RUnlock()
		return nil, errNotPublished
	}
	t, want, prev, failedAt := st.Tag, st.sums[a].sum, st.verified[a], st.failedAt[a]
	s.mu.RUnlock()
	if want == "" {
		return nil, errNotPublished
	}
	if !failedAt.IsZero() && s.now().Sub(failedAt) < failureRetry {
		return nil, errUnavailable
	}
	if err := s.acquire(ctx); err != nil {
		return nil, err
	}
	defer s.release()

	name := a.fileName(tag)
	if prev == nil || !prev.fromRelease {
		data, err := s.rebuild(ctx, t, a)
		if err == nil && sha256Hex(data) == want {
			s.setVerified(st, a, &verifiedFile{})
			return data, nil
		}
		if prev == nil {
			if err != nil {
				s.log.Warn("catalog-server: rebuild failed; serving the published file", "tag", tag, "file", name, "err", err)
			} else {
				s.metrics.mismatches.WithLabelValues(a.label()).Inc()
				s.log.Warn("catalog-server: rebuild differs from the published file; serving the published file", "tag", tag, "file", name)
			}
		}
	}
	data, err := s.releases.get(ctx, tag, name, maxAssetBytes)
	if err == nil && sha256Hex(data) != want {
		err = errors.New("does not match its published sha256")
	}
	if err != nil {
		s.metrics.releaseFailures.WithLabelValues(a.label()).Inc()
		s.log.Error("catalog-server: published file unavailable", "tag", tag, "file", name, "err", err)
		s.mu.Lock()
		st.failedAt[a] = s.now()
		s.mu.Unlock()
		return nil, errUnavailable
	}
	s.setVerified(st, a, &verifiedFile{fromRelease: true})
	return data, nil
}

func (s *Server) rebuild(ctx context.Context, t Tag, a asset) ([]byte, error) {
	s.metrics.builds.WithLabelValues(a.label()).Inc()
	data, err := s.mirror.Archive(ctx, t.Commit)
	if err == nil && a == assetCatalog {
		data, err = BuildCatalog(data, t.Committed, t.Name)
	}
	if err != nil {
		s.metrics.buildFailures.WithLabelValues(a.label()).Inc()
	}
	return data, err
}

func (s *Server) setVerified(st *tagState, a asset, v *verifiedFile) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st.verified[a] = v
	st.failedAt[a] = time.Time{}
}

func (s *Server) acquire(ctx context.Context) error {
	select {
	case s.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Server) release() { <-s.slots }
