package inference

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policybinding"
)

func benchKey(f principalFixture, plaintext string) *key.Key {
	return &key.Key{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "bench", Owner: meta.Owner{Kind: meta.OwnerProject, ID: f.project.Meta.ID}},
		Spec: key.Spec{
			Principal: key.Principal{Kind: key.PrincipalServiceAccount, ID: f.sa.Meta.ID},
			KeyHash:   sha(plaintext),
		},
	}
}

// benchFixture binds a policy at the project and one at the service account, so the principal's caller plan has two policy rungs.
func benchFixture() principalFixture {
	f := newPrincipalFixture()
	f.bindings = []*policybinding.PolicyBinding{
		boundTo(f, "b-project", 1, f.boundPol.Meta.ID, "project:"+f.project.Meta.ID),
		boundTo(f, "b-sa", 1, f.keyPol.Meta.ID, "serviceaccount:"+f.sa.Meta.ID),
	}
	return f
}

func serve(h http.Handler, bearer string) {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("Authorization", "Bearer "+bearer)
	h.ServeHTTP(httptest.NewRecorder(), r)
}

// BenchmarkKeyPrincipal is the key half of the auth path: hash, key plan lookup, caller ladder check.
func BenchmarkKeyPrincipal(b *testing.B) {
	f := benchFixture()
	st := f.stack(b, benchKey(f, "sk-wr-bench"))
	b.ReportAllocs()
	for b.Loop() {
		serve(st.handler, "sk-wr-bench")
	}
}

// BenchmarkTokenPrincipal is the token half: signature verification (cached after the first request), claim checks, the user plan and the revoked set.
func BenchmarkTokenPrincipal(b *testing.B) {
	f := benchFixture()
	st := f.stack(b)
	token := f.mint(b, nil)
	serve(st.handler, token) // warm the cache the way steady state is
	b.ReportAllocs()
	for b.Loop() {
		serve(st.handler, token)
	}
}

// BenchmarkTokenPrincipalColdCache pays the Ed25519 verification on every iteration.
func BenchmarkTokenPrincipalColdCache(b *testing.B) {
	f := benchFixture()
	f.tokens.SetCacheSize(-1)
	st := f.stack(b)
	token := f.mint(b, nil)
	b.ReportAllocs()
	for b.Loop() {
		serve(st.handler, token)
	}
}
