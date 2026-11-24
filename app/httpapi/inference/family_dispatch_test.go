package inference

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/adapter"
	"github.com/wyolet/relay/pkg/adapters"
	"github.com/wyolet/relay/pkg/clientprofile"
	v1 "github.com/wyolet/relay/sdk/v1"
)

const (
	shapeBase   adapters.Name = "shape-base"
	shapeMember adapters.Name = "shape-member"
	shapeOther  adapters.Name = "shape-other"
)

const canonicalReply = `{"id":"r","object":"response","model":"test-model","status":"completed"}`

// familyTranslator is the translator both family shapes share: one comparable value, so one serialized body serves both.
type familyTranslator struct{ v1.IdentityTranslator }

// familyRegistry registers a base shape, a member of its family that names its own path and auth and wraps the body in an envelope, and an unrelated inbound shape.
func familyRegistry() *adapter.Registry { return countingFamilyRegistry("", "") }

// countingFamilyRegistry is familyRegistry with the family shapes' count paths.
func countingFamilyRegistry(baseCount, memberCount string) *adapter.Registry {
	return adapter.NewRegistry(
		(&adapter.Spec{Name: shapeBase, DefaultPath: "/base/generate", CountPath: baseCount, Auth: adapter.AuthStrategy{Header: "Authorization", Scheme: "Bearer"}, Translator: familyTranslator{}}).Build(),
		(&adapter.Spec{Name: shapeMember, Family: shapeBase, DefaultPath: "/member/generate", CountPath: memberCount, Auth: adapter.AuthStrategy{Header: "X-Member-Key"}, Translator: familyTranslator{}, Envelope: &adapter.RequestEnvelope{
			DropMembers:    []string{"model"},
			DefaultMembers: []adapter.BodyMember{{Key: "version", Value: []byte(`"v1"`)}},
			JoinHeaders:    []string{"X-Beta"},
		}}).Build(),
		(&adapter.Spec{Name: shapeOther, Translator: v1.IdentityTranslator{}}).Build(),
	)
}

// familyCall is one request an upstream received: its path, the two credentials headers, the X-Beta lines joined by "|", and the body.
type familyCall struct{ Path, Auth, MemberKey, Beta, Body string }

func familyUpstream(t *testing.T, status int) (string, func() []familyCall) {
	t.Helper()
	got := make(chan familyCall, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- familyCall{r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("X-Member-Key"), strings.Join(r.Header.Values("X-Beta"), "|"), string(b)}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(canonicalReply))
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() []familyCall {
		var out []familyCall
		for len(got) > 0 {
			out = append(out, <-got)
		}
		return out
	}
}

// dispatchWithBeta is dispatch with the caller sending X-Beta on two lines.
func dispatchWithBeta(d Deps, pr *Principal, in DispatchInput) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.Header["X-Beta"] = []string{"a", "b"}
	w := httptest.NewRecorder()
	Dispatch(d, w, withNormalContext(r, pr), in)
	return w
}

const familyBody = `{"model":"test-model","input":"hi"}`

// A caller speaking a shape byte-passes to a member of its family, which gets the body in its envelope, its own path and auth, and the caller's repeated header lines joined; a failover from a base hop sends the base hop the body as sent.
func TestDispatch_FamilyBytePass(t *testing.T) {
	member := familyCall{"/member/generate", "", "sk-member", "a,b", `{"input":"hi","version":"v1"}`}
	for name, tc := range map[string]struct {
		baseFirst bool
		wantBase  []familyCall
	}{
		"member hop only":                {false, nil},
		"base hop fails, member answers": {true, []familyCall{{"/base/generate", "Bearer sk-base", "", "a|b", familyBody}}},
	} {
		t.Run(name, func(t *testing.T) {
			baseURL, baseCalls := familyUpstream(t, http.StatusInternalServerError)
			memberURL, memberCalls := familyUpstream(t, http.StatusOK)
			hops := []hopFixture{{host: "member", url: memberURL, adapter: shapeMember}}
			if tc.baseFirst {
				hops = append([]hopFixture{{host: "base", url: baseURL, adapter: shapeBase}}, hops...)
			}
			d, pr := buildHopsDeps(t, hops...)
			useSpecs(&d, familyRegistry())

			w := dispatchWithBeta(d, pr, DispatchInput{Inbound: shapeBase, Body: []byte(familyBody), ModelName: "test-model"})

			if w.Code != http.StatusOK || w.Body.String() != canonicalReply {
				t.Fatalf("status %d: %s; want the member's answer verbatim", w.Code, w.Body)
			}
			if diff := cmp.Diff(tc.wantBase, baseCalls()); diff != "" {
				t.Errorf("base hop (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff([]familyCall{member}, memberCalls()); diff != "" {
				t.Errorf("member hop (-want +got):\n%s", diff)
			}
		})
	}
}

// A caller speaking another shape goes through canonical: the body is serialized once by the family's translator and fails over between the family's hops, each sent it in its own form, path and auth.
func TestDispatch_FamilyCanonicalFailover(t *testing.T) {
	baseURL, baseCalls := familyUpstream(t, http.StatusInternalServerError)
	memberURL, memberCalls := familyUpstream(t, http.StatusOK)
	d, pr := buildHopsDeps(t,
		hopFixture{host: "base", url: baseURL, adapter: shapeBase},
		hopFixture{host: "member", url: memberURL, adapter: shapeMember})
	useSpecs(&d, familyRegistry())

	w := dispatchWithBeta(d, pr, DispatchInput{Inbound: shapeOther, Body: []byte(familyBody), ModelName: "test-model"})

	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	base, member := baseCalls(), memberCalls()
	if len(base) != 1 || len(member) != 1 {
		t.Fatalf("calls: base %+v, member %+v; want one each", base, member)
	}
	if got := (familyCall{base[0].Path, base[0].Auth, base[0].MemberKey, base[0].Beta, ""}); got != (familyCall{"/base/generate", "Bearer sk-base", "", "a|b", ""}) {
		t.Errorf("base hop = %+v", got)
	}
	if got := (familyCall{member[0].Path, member[0].Auth, member[0].MemberKey, member[0].Beta, ""}); got != (familyCall{"/member/generate", "", "sk-member", "a,b", ""}) {
		t.Errorf("member hop = %+v", got)
	}
	baseBody, memberBody := members(t, base[0].Body), members(t, member[0].Body)
	if string(baseBody["model"]) != `"test-model"` || baseBody["version"] != nil {
		t.Errorf("base body = %s, want the serialized body with its model and no version", base[0].Body)
	}
	if memberBody["model"] != nil || string(memberBody["version"]) != `"v1"` {
		t.Errorf("member body = %s, want no model and the default version", member[0].Body)
	}
	delete(baseBody, "model")
	delete(memberBody, "version")
	if diff := cmp.Diff(baseBody, memberBody); diff != "" {
		t.Errorf("the hops' bodies differ beyond the envelope (-base +member):\n%s", diff)
	}
}

// shapedCountProfile is countProfile speaking shape.
type shapedCountProfile struct {
	countProfile
	shape adapters.Name
}

func (p shapedCountProfile) Shape() string { return string(p.shape) }

// A family does not widen the upstream token count: it needs the first hop's own shape, fails over only to hops of that shape, and sends the count body without the generation envelope.
func TestCountTokens_FamilyKeepsExactShape(t *testing.T) {
	for name, tc := range map[string]struct {
		speaks             adapters.Name
		baseFirst          bool
		tier               string
		wantBase, wantMemb []hopCall
	}{
		"member shape counts without its envelope": {speaks: shapeMember, tier: "exact", wantMemb: []hopCall{{"/member/count", "test-model"}}},
		"family shape does not count on a member":  {speaks: shapeBase, tier: "estimated"},
		"no failover from base to member":          {speaks: shapeBase, baseFirst: true, wantBase: []hopCall{{"/base/count", "test-model"}}},
	} {
		t.Run(name, func(t *testing.T) {
			baseURL, baseCalls := hopUpstream(t, http.StatusInternalServerError, downReply)
			memberURL, memberCalls := hopUpstream(t, http.StatusOK, `{"input_tokens":7}`)
			hops := []hopFixture{{host: "member", url: memberURL, adapter: shapeMember}}
			if tc.baseFirst {
				hops = append([]hopFixture{{host: "base", url: baseURL, adapter: shapeBase}}, hops...)
			}
			d, pr := buildHopsDeps(t, hops...)
			useSpecs(&d, countingFamilyRegistry("/base/count", "/member/count"))

			r := httptest.NewRequest(http.MethodPost, "/counting/v1/count", strings.NewReader(countBody))
			r = withNormalContext(r, pr)
			r = r.WithContext(clientprofile.WithProfile(r.Context(), shapedCountProfile{shape: tc.speaks}))
			w := httptest.NewRecorder()
			handleCountTokens(d, w, r)

			if tc.tier != "" {
				wantTokenCount(t, w, map[string]string{"exact": "7", "estimated": "9"}[tc.tier], tc.tier)
			} else if w.Code == http.StatusOK {
				t.Errorf("answer = 200 %s, want the base hop's failure", w.Body)
			}
			if diff := cmp.Diff(tc.wantBase, baseCalls()); diff != "" {
				t.Errorf("base hop (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.wantMemb, memberCalls()); diff != "" {
				t.Errorf("member hop (-want +got):\n%s", diff)
			}
		})
	}
}

func members(t *testing.T, body string) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("body %s: %v", body, err)
	}
	return m
}
