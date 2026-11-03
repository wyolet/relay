package inference

// principal_alloc_test.go puts a ceiling on what the credential middleware allocates per request: it runs on every /v1/* call, so a change that allocates more has to raise the ceiling on purpose.

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// discardWriter allocates nothing, so AllocsPerRun reports the middleware's own cost rather than a recorder's buffers.
type discardWriter struct{ header http.Header }

func (d *discardWriter) Header() http.Header         { return d.header }
func (d *discardWriter) Write(b []byte) (int, error) { return len(b), nil }
func (d *discardWriter) WriteHeader(int)             {}

// authOnly is the credential middleware alone over the published plans, with the classification already on the request.
func authOnly(t *testing.T, st *principalStack, bearer string) (http.Handler, *http.Request) {
	t.Helper()
	var served bool
	h := PrincipalMiddleware(st.auth)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { served = true }))
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r = r.WithContext(WithClassification(r.Context(), Classification{Mode: ModeNormal, Key: bearer}))
	// A rejected credential would measure the error path instead.
	h.ServeHTTP(&discardWriter{header: http.Header{}}, r)
	if !served {
		t.Fatal("the bearer was rejected; the ceiling would measure the rejection path")
	}
	return h, r
}

func assertAllocCeiling(t *testing.T, what string, h http.Handler, r *http.Request, ceiling int) {
	t.Helper()
	w := &discardWriter{header: http.Header{}}
	got := int(testing.AllocsPerRun(500, func() { h.ServeHTTP(w, r) }))
	if raceInstrumented {
		ceiling = ceiling * 3 / 2
	}
	if got > ceiling {
		t.Fatalf("%s allocates %d objects per request, over the ceiling of %d; raise it deliberately if the change is intended", what, got, ceiling)
	}
}

// The key half: hash the bearer, look its plan up, check the caller ladder, stash the principal.
func TestKeyPathAllocations(t *testing.T) {
	f := benchFixture()
	st := f.stack(t, benchKey(f, "sk-wr-alloc"))
	h, r := authOnly(t, st, "sk-wr-alloc")
	assertAllocCeiling(t, "the key path", h, r, 3)
}

// The token half in steady state: the verified claims come from the cache, so what is left is the claim checks, the plan reads and the principal.
func TestTokenPathAllocations(t *testing.T) {
	f := benchFixture()
	st := f.stack(t)
	h, r := authOnly(t, st, f.mint(t, nil))
	assertAllocCeiling(t, "the token path", h, r, 4)
}
