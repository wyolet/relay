package inference

// token_fuzz_test.go fuzzes the two functions every inbound bearer reaches
// before anything is trusted: the shape sniff that decides which lookup runs,
// and the token verification that turns claims into a Principal.

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/pkg/crypto"
)

func fuzzBearerSeeds(valid string) []string {
	header, rest, _ := strings.Cut(valid, ".")
	body, sig, _ := strings.Cut(rest, ".")
	return []string{
		"", ".", "..", "ey", "ey..", "ey.a.b", "a.b.c",
		valid,
		valid + ".",
		valid[:len(valid)-1],
		header + "." + body,
		"." + body + "." + sig,
		"sk-wr-0123456789abcdef",
		"sk-wr-with.two.dots",
		"Bearer " + valid,
		"ey☃.é.𝄞",
		strings.Repeat("ey.", 1000),
		"ey" + strings.Repeat("A", 64<<10) + ".a.b",
		"\x00ey.a.b",
	}
}

// FuzzLooksLikeToken pins the classifier as a total function and holds it to
// its stated rule: three dot-separated segments whose header starts "ey". A
// bearer with any other dot count must never be routed to the token lookup,
// because the key lookup is the only one that can authenticate it.
func FuzzLooksLikeToken(f *testing.F) {
	valid := newPrincipalFixture().mint(f, nil)
	for _, seed := range fuzzBearerSeeds(valid) {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, bearer string) {
		got := looksLikeToken(bearer)
		want := strings.HasPrefix(bearer, "ey") && strings.Count(bearer, ".") == 2
		if got != want {
			t.Fatalf("looksLikeToken(%q) = %v, want %v", bearer, got, want)
		}
		if got && strings.Count(bearer, ".") != 2 {
			t.Fatalf("bearer %q classified as a token with the wrong segment count", bearer)
		}
	})
}

// FuzzTokenPrincipal drives the whole token half of the credential middleware
// with arbitrary bearers against a snapshot holding exactly one user and one
// project. No input may panic, and only the correctly signed token may resolve
// a principal — anything else must have written a rejection instead.
func FuzzTokenPrincipal(f *testing.F) {
	fx := newPrincipalFixture()
	st := fx.stack(f)
	snap := st.cat.Current()
	valid := fx.mint(f, nil)

	// Corpus entries that differ from the valid token only in the claims, so
	// the fuzzer starts from inputs that reach past the signature check.
	wrongIssuer := fx.mint(f, func(c *crypto.TokenClaims) { c.Iss = "not-relay" })
	wrongSubject := fx.mint(f, func(c *crypto.TokenClaims) { c.Sub = "serviceaccount:" + meta.NewID() })
	wrongProject := fx.mint(f, func(c *crypto.TokenClaims) { c.Prj = meta.NewID() })
	staleVersion := fx.mint(f, func(c *crypto.TokenClaims) { c.Ver = 99 })
	expired := fx.mint(f, func(c *crypto.TokenClaims) { c.Exp = c.Iat - 1 })

	seeds := append(fuzzBearerSeeds(valid),
		wrongIssuer, wrongSubject, wrongProject, staleVersion, expired)
	for _, seed := range seeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, bearer string) {
		w := httptest.NewRecorder()
		p, ok := tokenPrincipal(w, snap, fx.tokens, bearer)
		if !ok {
			if p != nil {
				t.Fatalf("rejected bearer %q still returned a principal", bearer)
			}
			if w.Code < 400 {
				t.Fatalf("rejected bearer %q answered %d, want a 4xx", bearer, w.Code)
			}
			return
		}
		if bearer != valid {
			t.Fatalf("bearer %q resolved a principal; only the correctly signed token may", bearer)
		}
		if p.UserID != fx.user || p.ProjectID != fx.project.Meta.ID || p.TeamID != fx.team.Meta.ID {
			t.Fatalf("principal = %+v, want the fixture's user and its project", p)
		}
		if p.CredentialKind != CredentialToken || p.CredentialID == "" {
			t.Fatalf("credential = (%q, %q), want the token kind and its jti",
				p.CredentialKind, p.CredentialID)
		}
	})
}
