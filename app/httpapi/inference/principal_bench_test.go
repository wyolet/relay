package inference

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/rolebinding"
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

// benchFixture adds `bindings` non-matching policy bindings ahead of one
// that matches, so the binding scan the auth path runs is representative of
// a project with real bindings on it.
func benchFixture(bindings int) principalFixture {
	f := newPrincipalFixture()
	for i := 0; i < bindings; i++ {
		pb := &policybinding.PolicyBinding{Meta: meta.Metadata{ID: meta.NewID(), Name: "miss-" + meta.NewID()[:8]}}
		pb.Spec.ProjectID = f.project.Meta.ID
		pb.Spec.PolicyID = f.boundPol.Meta.ID
		pb.Spec.Priority = i + 1
		pb.Spec.Subjects = []rolebinding.Subject{
			{Kind: rolebinding.SubjectGroup, Name: "no-match-" + meta.NewID()[:8]},
			{Kind: rolebinding.SubjectUser, ID: meta.NewID()},
		}
		pb.StampOwner()
		f.bindings = append(f.bindings, pb)
	}
	match := &policybinding.PolicyBinding{Meta: meta.Metadata{ID: meta.NewID(), Name: "bind-all"}}
	match.Spec.ProjectID = f.project.Meta.ID
	match.Spec.PolicyID = f.boundPol.Meta.ID
	match.Spec.Priority = bindings + 1
	match.Spec.Subjects = []rolebinding.Subject{{Kind: rolebinding.SubjectGroup, Name: "system:authenticated"}}
	match.StampOwner()
	f.bindings = append(f.bindings, match)
	return f
}

func serve(h http.Handler, bearer string) {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("Authorization", "Bearer "+bearer)
	h.ServeHTTP(httptest.NewRecorder(), r)
}

// BenchmarkKeyPrincipal is the key half of the auth path: hash, snapshot
// lookup, subject copy, binding scan.
func BenchmarkKeyPrincipal(b *testing.B) {
	f := benchFixture(8)
	st := f.stack(b, benchKey(f, "sk-wr-bench"))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		serve(st.handler, "sk-wr-bench")
	}
}

// BenchmarkTokenPrincipal is the token half: signature verification (cached
// after the first request), claim checks, and subject resolution.
func BenchmarkTokenPrincipal(b *testing.B) {
	f := benchFixture(8)
	st := f.stack(b)
	token := f.mint(b, nil)
	serve(st.handler, token) // warm the cache the way steady state is
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		serve(st.handler, token)
	}
}

// BenchmarkTokenPrincipalColdCache pays the Ed25519 verification on every
// iteration — the shape of the path before the verified-claims cache.
func BenchmarkTokenPrincipalColdCache(b *testing.B) {
	f := benchFixture(8)
	f.tokens.SetCacheSize(-1)
	st := f.stack(b)
	token := f.mint(b, nil)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		serve(st.handler, token)
	}
}

// BenchmarkBindingMatch isolates the per-binding subject comparison.
func BenchmarkBindingMatch(b *testing.B) {
	f := benchFixture(16)
	subjects := []string{"serviceaccount:x", "group:system:serviceaccounts", "group:system:authenticated"}
	bindings := f.bindings
	for _, pb := range bindings {
		pb.IndexSubjects()
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, pb := range bindings {
			_ = bindingMatches(pb, subjects)
		}
	}
}
