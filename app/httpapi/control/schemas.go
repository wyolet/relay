// schemas.go serves the JSON Schemas the exported manifests reference from
// their `$schema` directive. Public: an editor resolving the directive has
// no session, and a schema describes shapes, never data. Raw chi rather than
// huma so the response keeps its `application/schema+json` content type.
package control

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"path"
	"sort"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/schemas"
)

func registerSchemas(r chi.Router) {
	r.Get("/schemas/{version}", func(w http.ResponseWriter, req *http.Request) {
		version := chi.URLParam(req, "version")
		kinds, err := schemaKinds(version)
		if err != nil {
			http.Error(w, "unknown schema version", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			Version string   `json:"version"`
			Kinds   []string `json:"kinds"`
		}{version, kinds})
	})

	r.Get("/schemas/{version}/{kind}", func(w http.ResponseWriter, req *http.Request) {
		version := chi.URLParam(req, "version")
		kind := strings.TrimSuffix(chi.URLParam(req, "kind"), ".schema.json")
		// Reject traversal before touching the embedded FS: the version and
		// kind are both path segments. path.Base leaves "." and ".." intact,
		// so the whole path is checked with fs.ValidPath as well.
		name := version + "/" + kind + ".schema.json"
		if version != path.Base(version) || kind != path.Base(kind) || kind == "" || !fs.ValidPath(name) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		body, err := schemas.FS.ReadFile(name)
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/schema+json")
		_, _ = w.Write(body)
	})
}

func schemaKinds(version string) ([]string, error) {
	if version != path.Base(version) || !fs.ValidPath(version) {
		return nil, http.ErrMissingFile
	}
	entries, err := schemas.FS.ReadDir(version)
	if err != nil {
		return nil, err
	}
	kinds := make([]string, 0, len(entries))
	for _, e := range entries {
		kinds = append(kinds, strings.TrimSuffix(e.Name(), ".schema.json"))
	}
	sort.Strings(kinds)
	return kinds, nil
}
