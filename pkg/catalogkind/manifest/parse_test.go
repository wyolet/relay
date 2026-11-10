package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestParseCatalogKinds(t *testing.T) {
	docs, err := Parse(strings.NewReader(`apiVersion: relay.wyolet.dev/v1alpha3
kind: Provider
metadata: {name: acme}
---
---
apiVersion: relay.wyolet.dev/v1alpha3
kind: HostBinding
metadata: {name: m-on-h}
spec: {model: m, host: h, adapter: openai}
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 2 || docs[0].Provider == nil || docs[1].HostBinding == nil {
		t.Fatalf("docs = %+v", docs)
	}
	if docs[1].HostBinding.Spec.Model != "m" {
		t.Fatalf("spec not decoded: %+v", docs[1].HostBinding.Spec)
	}
}

func TestParseRefusals(t *testing.T) {
	for _, tc := range []struct{ name, doc, want string }{
		{"unregistered kind", "apiVersion: relay.wyolet.dev/v1alpha3\nkind: HostKey\nmetadata: {name: k}\n",
			`wire: doc 0 kind=HostKey name=k: unknown kind "HostKey"`},
		{"unknown apiVersion", "apiVersion: v9\nkind: Provider\nmetadata: {name: p}\n",
			`wire: doc 0: unsupported apiVersion "v9" (want "relay.wyolet.dev/v1alpha3")`},
		{"no name", "apiVersion: relay.wyolet.dev/v1alpha3\nkind: Provider\n",
			"wire: doc 0 kind=Provider: metadata.name required"},
		{"reshaped kind", "apiVersion: relay.wyolet.dev/v1alpha2\nkind: Policy\nmetadata: {name: p}\n",
			"wire: doc 0: Policy under relay.wyolet.dev/v1alpha2 is the pre-v0.20 shape"},
		{"removed field", "apiVersion: relay.wyolet.dev/v1alpha3\nkind: Policy\nmetadata: {name: p}\nspec: {rateLimit: r, hostKeys: [k]}\n",
			"wire: doc 0 kind=Policy name=p: Policy no longer has spec.hostKeys (use pools), spec.rateLimit (use rateLimits: [{ref: <name>}])"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(tc.doc))
			if err == nil || !strings.HasPrefix(err.Error(), tc.want) {
				t.Fatalf("err = %v, want prefix %q", err, tc.want)
			}
		})
	}
}

func TestParsePreviousAPIVersionForUnchangedKind(t *testing.T) {
	docs, err := Parse(strings.NewReader("apiVersion: relay.wyolet.dev/v1alpha2\nkind: Host\nmetadata: {name: h}\n"))
	if err != nil || len(docs) != 1 || docs[0].Host == nil {
		t.Fatalf("docs = %+v, err = %v", docs, err)
	}
}

type widget struct{ name string }

func TestRegisterAndIntercept(t *testing.T) {
	p := NewParser()
	p.Register("Widget", KindDecoder{
		Decode: func(env *Envelope) (Document, error) {
			return Document{Ext: widget{name: env.Metadata.Name}}, nil
		},
		RemovedFields: map[string]string{"size": "dimensions"},
	})
	p.Intercept(func(_ int, env *Envelope, _ *yaml.Node) (Document, bool, error) {
		if env.Kind == "User" {
			return Document{Foreign: env.Kind}, true, nil
		}
		return Document{}, false, nil
	})

	docs, err := p.Parse(strings.NewReader(`apiVersion: relay.wyolet.dev/v1alpha3
kind: Widget
metadata: {name: w}
---
apiVersion: other/v1
kind: User
metadata: {name: u}
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 2 || docs[0].Ext != (widget{name: "w"}) || docs[1].Foreign != "User" {
		t.Fatalf("docs = %+v", docs)
	}
	_, err = p.Parse(strings.NewReader("apiVersion: relay.wyolet.dev/v1alpha3\nkind: Widget\nmetadata: {name: w}\nspec: {size: 1}\n"))
	if err == nil || !strings.Contains(err.Error(), "Widget no longer has spec.size (use dimensions)") {
		t.Fatalf("err = %v", err)
	}
	if _, err := Parse(strings.NewReader("apiVersion: relay.wyolet.dev/v1alpha3\nkind: Widget\nmetadata: {name: w}\n")); err == nil {
		t.Fatal("the package parser accepted a kind registered on another parser")
	}
}

func TestRegisterTwicePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("no panic")
		}
	}()
	NewParser().Register("Provider", KindDecoder{})
}

func TestLoadDirSkipsDraftsAndForeign(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	provider := func(name string) string {
		return "apiVersion: relay.wyolet.dev/v1alpha3\nkind: Provider\nmetadata: {name: " + name + "}\n"
	}
	write("providers/live/provider.yaml", provider("live"))
	write("drafts/providers/d/provider.yaml", provider("drafted-dir"))
	write("providers/live/extra.draft.yaml", provider("drafted-file"))
	write("users/u.yaml", "apiVersion: other/v1\nkind: User\nmetadata: {name: u}\n")

	p := NewParser()
	p.Intercept(func(_ int, env *Envelope, _ *yaml.Node) (Document, bool, error) {
		return Document{Foreign: env.Kind}, env.Kind == "User", nil
	})
	docs, err := p.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || docs[0].Provider == nil || docs[0].Provider.Metadata.Name != "live" {
		t.Fatalf("docs = %+v", docs)
	}
}
