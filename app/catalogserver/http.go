package catalogserver

import (
	"bytes"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	immutableCache = "public, max-age=31536000, immutable"
	shortCache     = "public, max-age=60"
	// channelDataAsset names the data tarball under /channels/, where the
	// client doesn't know the tag the file name carries.
	channelDataAsset = "relay-catalog-data.tar.gz"
)

// Handler serves the catalog release API. Nothing here compresses a
// response: clients check the sha256 of the stored bytes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /index.yaml", s.serveIndex)
	mux.HandleFunc("GET /releases/{tag}/{file}", s.serveRelease)
	mux.HandleFunc("GET /channels/{channel}/latest/{file}", s.serveChannelLatest)
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !s.ready() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.Handle("GET /metrics", promhttp.HandlerFor(s.metrics.registry, promhttp.HandlerOpts{}))
	return mux
}

func (s *Server) serveIndex(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	index, etag, synced := s.index, s.indexETag, s.synced
	s.mu.RUnlock()
	if !synced || index == nil {
		http.Error(w, "index not built yet", http.StatusServiceUnavailable)
		return
	}
	serveBytes(w, r, index, "application/yaml", shortCache, etag)
}

func (s *Server) serveRelease(w http.ResponseWriter, r *http.Request) {
	tag, file := r.PathValue("tag"), r.PathValue("file")
	if !isReleaseTag(tag) {
		http.NotFound(w, r)
		return
	}
	name, isSum := strings.CutSuffix(file, ".sha256")
	a, ok := assetNamed(tag, name)
	if !ok {
		http.NotFound(w, r)
		return
	}
	s.mu.RLock()
	st := s.tags[tag]
	var sumFile []byte
	if st != nil {
		sumFile = st.sums[a].file
	}
	s.mu.RUnlock()
	if st == nil {
		s.noteUnknown(tag)
		http.NotFound(w, r)
		return
	}
	if sumFile == nil {
		http.NotFound(w, r)
		return
	}
	if isSum {
		serveBytes(w, r, sumFile, "text/plain; charset=utf-8", immutableCache, `"`+sha256Hex(sumFile)+`"`)
		return
	}
	data, err := s.file(r.Context(), tag, a)
	switch {
	case errors.Is(err, errNotPublished):
		http.NotFound(w, r)
	case err != nil:
		w.Header().Set("Retry-After", "60")
		http.Error(w, "file unavailable", http.StatusServiceUnavailable)
	default:
		serveBytes(w, r, data, "application/gzip", immutableCache, `"`+sha256Hex(data)+`"`)
	}
}

func (s *Server) serveChannelLatest(w http.ResponseWriter, r *http.Request) {
	channel, file := r.PathValue("channel"), r.PathValue("file")
	s.mu.RLock()
	tag := s.latest[channel]
	s.mu.RUnlock()
	name, isSum := strings.CutSuffix(file, ".sha256")
	switch {
	case tag == "":
		http.NotFound(w, r)
		return
	case name == channelDataAsset:
		name = DataAsset(tag)
	case name != CatalogAsset:
		http.NotFound(w, r)
		return
	}
	if isSum {
		name += ".sha256"
	}
	w.Header().Set("Cache-Control", shortCache)
	http.Redirect(w, r, "/releases/"+tag+"/"+name, http.StatusFound)
}

// serveBytes answers If-None-Match with 304 and serves no Last-Modified, so
// the ETag alone decides.
func serveBytes(w http.ResponseWriter, r *http.Request, data []byte, contentType, cacheControl, etag string) {
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("Cache-Control", cacheControl)
	h.Set("ETag", etag)
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
}
