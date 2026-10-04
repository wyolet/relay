package inference

// principal_alloc_test.go pins the allocation cost of the credential
// middleware. The numbers are the measured current cost, not a budget with
// headroom: a change that allocates more per request has to be seen and
// re-pinned deliberately, because this runs on every /v1/* call.

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// discardWriter is a ResponseWriter that allocates nothing, so what
// AllocsPerRun reports is the middleware's own cost rather than the
// recorder's buffers.
type discardWriter struct{ header http.Header }

func (d *discardWriter) Header() http.Header         { return d.header }
func (d *discardWriter) Write(b []byte) (int, error) { return len(b), nil }
func (d *discardWriter) WriteHeader(int)             {}

// authOnly is the credential middleware alone, with the classification the
// upstream classifier would have produced already on the context — the
// measurement is of PrincipalMiddleware, not of the chain around it.
func authOnly(t testing.TB, st *principalStack, tokens *TokenVerifier, bearer string) (http.Handler, *http.Request) {
	t.Helper()
	var served bool
	h := PrincipalMiddleware(st.cat, tokens)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		served = true
	}))
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r = r.WithContext(WithClassification(r.Context(), Classification{Mode: ModeNormal, Key: bearer}))

	// A rejected credential would measure the error path instead of the one
	// this test claims to pin.
	h.ServeHTTP(&discardWriter{header: http.Header{}}, r)
	if !served {
		t.Fatalf("bearer was rejected; the alloc gate would measure the rejection path")
	}
	return h, r
}

func allocsFor(t *testing.T, h http.Handler, r *http.Request) int {
	t.Helper()
	w := &discardWriter{header: http.Header{}}
	return int(testing.AllocsPerRun(500, func() { h.ServeHTTP(w, r) }))
}

// TestKeyPathAllocations pins the key half: hash the bearer, look it up in
// the snapshot, take the precomputed subject slice, scan the project's
// policy bindings, and stash the principal on the context.
func TestKeyPathAllocations(t *testing.T) {
	const want = 7

	f := benchFixture(8)
	st := f.stack(t, benchKey(f, "sk-wr-alloc"))
	h, r := authOnly(t, st, f.tokens, "sk-wr-alloc")

	if got := allocsFor(t, h, r); got != want {
		t.Fatalf("key path allocates %d objects per request, pinned at %d — "+
			"re-pin deliberately if the change is intended", got, want)
	}
}

// TestTokenPathAllocations pins the token half in steady state: the verified
// claims come from the cache, so what is left is the claim checks, the
// snapshot reads and the Principal.
func TestTokenPathAllocations(t *testing.T) {
	const want = 7

	f := benchFixture(8)
	st := f.stack(t)
	token := f.mint(t, nil)
	h, r := authOnly(t, st, f.tokens, token)

	if got := allocsFor(t, h, r); got != want {
		t.Fatalf("token path allocates %d objects per request, pinned at %d — "+
			"re-pin deliberately if the change is intended", got, want)
	}
}

// TestTokenPathAllocationsColdCache pins what a first-sight token costs: the
// Ed25519 verification and the claims decode the cache exists to avoid.
func TestTokenPathAllocationsColdCache(t *testing.T) {
	const want = 23

	f := benchFixture(8)
	f.tokens.SetCacheSize(-1)
	st := f.stack(t)
	token := f.mint(t, nil)
	h, r := authOnly(t, st, f.tokens, token)

	if got := allocsFor(t, h, r); got != want {
		t.Fatalf("cold-cache token path allocates %d objects per request, pinned at %d — "+
			"re-pin deliberately if the change is intended", got, want)
	}
}
