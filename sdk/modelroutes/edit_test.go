package modelroutes

import (
	"os"
	"strings"
	"testing"
)

func TestAddAppendsAlignedLines(t *testing.T) {
	ic := fixtureIndex(t, fixtureCatalog("v0.1.10"))
	for _, eol := range []string{"\n", "\r\n"} {
		path := writeRoutes(t, strings.ReplaceAll(sampleFile, "\n", eol))
		if err := Add(path, ic, ModelRoute{Key: "gpt-relay", Model: "gpt-5-5", Route: "relay"}); err != nil {
			t.Fatal(err)
		}
		if err := Add(path, ic, ModelRoute{Key: "qwen-next", Route: "myollama"}); err != nil {
			t.Fatal(err)
		}
		want := strings.Replace(sampleFile, "# self-hosted tag\n", `# self-hosted tag
"gpt-relay"       = { model = "gpt-5-5", route = "relay" }
"qwen-next"       = "myollama"
`, 1)
		if got := readFile(t, path); got != strings.ReplaceAll(want, "\n", eol) {
			t.Fatalf("eol %q: file:\n%s\nwant:\n%s", eol, got, want)
		}
	}
}

func TestAddCreatesModelsTable(t *testing.T) {
	path := writeRoutes(t, "catalog = \"v0.1.10\"\n[routes]\nlocal = { url = \"http://box:11434\", adapter = \"openai\", passthrough = true }")
	if err := Add(path, fixtureIndex(t, fixtureCatalog("v0.1.10")), ModelRoute{Key: "llama3", Route: "local"}); err != nil {
		t.Fatal(err)
	}
	want := "catalog = \"v0.1.10\"\n[routes]\nlocal = { url = \"http://box:11434\", adapter = \"openai\", passthrough = true }\n\n[models]\n\"llama3\" = \"local\"\n"
	if got := readFile(t, path); got != want {
		t.Fatalf("file:\n%q\nwant:\n%q", got, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want the original 0600", info.Mode().Perm())
	}
}

func TestAddRejects(t *testing.T) {
	ic := fixtureIndex(t, fixtureCatalog("v0.1.10"))
	body := strings.Replace(sampleFile, "[models]", "badauth = { url = \"https://x.example\", auth = \"oauth\" }\n\n[models]", 1)
	path := writeRoutes(t, body)
	cases := map[string]ModelRoute{
		"duplicate key":    {Key: "opus-work", Model: "gpt-5-5", Route: "codex"},
		"unknown route":    {Key: "gpt-x", Model: "gpt-5-5", Route: "nope"},
		"not on host":      {Key: "gpt-home", Model: "gpt-5-5", Route: "claude-home"},
		"not in catalog":   {Key: "no-such-model", Route: "relay"},
		"unusable route":   {Key: "gpt-bad", Model: "gpt-5-5", Route: "badauth"},
		"empty key":        {Route: "relay"},
		"missing in route": {Key: "glm-5-3-anthropic", Model: "glm-5-3", Route: "claude-work"},
	}
	for name, m := range cases {
		if err := Add(path, ic, m); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	if got := readFile(t, path); got != body {
		t.Fatalf("a rejected add changed the file:\n%s", got)
	}
}

func TestRemove(t *testing.T) {
	path := writeRoutes(t, sampleFile)
	if err := Remove(path, "opus-work"); err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(sampleFile, "\"opus-work\"       = { model = \"claude-opus-5-5\", route = \"claude-work\" }\n", "", 1)
	if got := readFile(t, path); got != want {
		t.Fatalf("file:\n%s\nwant:\n%s", got, want)
	}
	if err := Remove(path, "opus-work"); err == nil {
		t.Fatal("removing an unlisted key: want error")
	}
	if err := Remove(path, "claude-opus-5-5"); err == nil || !strings.Contains(err.Error(), "default") {
		t.Fatalf("removing the default: err = %v", err)
	}
	if got := readFile(t, path); got != want {
		t.Fatal("a refused remove changed the file")
	}
}

func TestSetCatalog(t *testing.T) {
	cases := []struct{ name, before, after string }{
		{"keeps comment", sampleFile, strings.Replace(sampleFile, `catalog = "v0.1.10" # pinned`, `catalog = "v0.1.11" # pinned`, 1)},
		{"literal string", "catalog  =  'v0.1.10'\n[models]\n", "catalog  =  \"v0.1.11\"\n[models]\n"},
		{"inserted above first key", "# head\n\ndefault = \"a\"\n", "# head\n\ncatalog = \"v0.1.11\"\ndefault = \"a\"\n"},
		{"inserted above first table", "# head\n[models]\n", "# head\ncatalog = \"v0.1.11\"\n[models]\n"},
		{"inserted at end", "# head", "# head\ncatalog = \"v0.1.11\"\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeRoutes(t, tc.before)
			if err := SetCatalog(path, "v0.1.11"); err != nil {
				t.Fatal(err)
			}
			if got := readFile(t, path); got != tc.after {
				t.Fatalf("file:\n%q\nwant:\n%q", got, tc.after)
			}
		})
	}
	if err := SetCatalog(writeRoutes(t, sampleFile), ""); err == nil {
		t.Fatal("empty tag: want error")
	}
}

func TestEditsRefuseUnsafeLayouts(t *testing.T) {
	ic := fixtureIndex(t, fixtureCatalog("v0.1.10"))
	add := func(path string) error {
		return Add(path, ic, ModelRoute{Key: "b", Model: "claude-opus-5-5", Route: "r"})
	}
	remove := func(path string) error { return Remove(path, "a") }
	setCatalog := func(path string) error { return SetCatalog(path, "v2") }
	routes := "[routes]\nr = { host = \"anthropic\" }\n"
	cases := []struct {
		name  string
		body  string
		edits []func(string) error
	}{
		{"inline table", "catalog = \"v1\"\nmodels = { a = \"r\" }\n" + routes, []func(string) error{add, remove}},
		{"multi-line entry", "catalog = \"v1\"\n" + routes + "[models]\na = { model = \"claude-opus-5-5\",\n  route = \"r\" }\n", []func(string) error{add, remove}},
		{"sub-table entry", "catalog = \"v1\"\n" + routes + "[models.a]\nmodel = \"claude-opus-5-5\"\nroute = \"r\"\n", []func(string) error{add, remove}},
		{"entry split by key", "catalog = \"v1\"\n" + routes + "[models]\na.model = \"claude-opus-5-5\"\na.route = \"r\"\n", []func(string) error{add, remove}},
		{"multi-line catalog", "catalog = \"\"\"\nv1\"\"\"\n" + routes + "[models]\na = \"r\"\n", []func(string) error{setCatalog}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeRoutes(t, tc.body)
			for i, edit := range tc.edits {
				if err := edit(path); err == nil || !strings.Contains(err.Error(), "by hand") {
					t.Errorf("edit %d: err = %v, want a refusal", i, err)
				}
			}
			if got := readFile(t, path); got != tc.body {
				t.Fatalf("a refused edit changed the file:\n%s", got)
			}
		})
	}
}
