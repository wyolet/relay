package curation

import (
	"testing"

	"github.com/wyolet/relay/app/manifest"
)

func modelDoc(name, family, line string) manifest.Document {
	return manifest.Document{Model: &manifest.ModelDTO{
		Metadata: manifest.WireMeta{Name: name},
		Spec:     manifest.ModelSpec{Family: family, ProductLine: line},
	}}
}

func TestCheckFamilies(t *testing.T) {
	issues := CheckFamilies([]manifest.Document{
		modelDoc("ok", "gemini", ""),
		modelDoc("versioned", "gemini-3-flash", ""),
		modelDoc("none", "", ""),
	}, map[string]map[string]bool{"gemini": nil})
	if len(issues) != 2 || issues[0].Source.Name != "versioned" || issues[1].Source.Name != "none" {
		t.Fatalf("issues = %+v", issues)
	}
}

func TestCheckProductLines(t *testing.T) {
	lines := map[string]map[string]bool{
		"claude": {"opus": true},
		"gpt":    {"o": true},
		"falcon": {},
	}
	issues := CheckProductLines([]manifest.Document{
		modelDoc("ok", "claude", "opus"),
		modelDoc("empty", "gpt", ""),
		modelDoc("other-family", "gpt", "opus"),
		modelDoc("line-alias", "gpt", "o-mini"),
		modelDoc("no-lines", "falcon", "falcon"),
		modelDoc("unknown-family", "gemini-3", "flash"),
	}, lines)
	want := []string{"other-family", "line-alias", "no-lines"}
	if len(issues) != len(want) {
		t.Fatalf("issues = %+v", issues)
	}
	for i, name := range want {
		if issues[i].Source.Name != name || issues[i].Source.Field != "spec.productLine" {
			t.Fatalf("issues[%d] = %+v, want %s", i, issues[i], name)
		}
	}
}
