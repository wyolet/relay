package modelroutes

import (
	"slices"
	"strings"
	"testing"
)

const tableRoutesFile = `# Routes for CI.
catalog = "v0.1.10"
default = "claude-opus-5-5"

[routes]
relay = { url = "https://relay.example" }

# Claude via the Max plan.
[routes.claude-dev]
host = "anthropic"
auth = "subscription" # renewed monthly
account = "dev@example.com"

# Codex via ChatGPT login.
[routes.codex]
host = "openai"
# subscription login
auth = "subscription"
adapter = "codex"

# picker order matters
[models]
"claude-opus-5-5" = "claude-dev"
"opus-dev"        = { model = "claude-opus-5-5", route = "claude-dev" }
"glm-5-3"         = "relay"
`

const codexTable = `# Codex via ChatGPT login.
[routes.codex]
host = "openai"
# subscription login
auth = "subscription"
adapter = "codex"
`

func TestAddRouteAfterLastRoutesTable(t *testing.T) {
	ic := fixtureIndex(t, fixtureCatalog("v0.1.10"))
	path := writeRoutes(t, tableRoutesFile)
	work := Route{Name: "claude-work", Host: "anthropic", Auth: "subscription", Account: "dev@example.com", DisplayName: "Claude Max (dev@example.com)"}
	if err := AddRoute(path, ic, work); err != nil {
		t.Fatal(err)
	}
	if err := AddRoute(path, nil, Route{Name: "a.b", URL: "https://ab.example", Adapter: "openai", Passthrough: true}); err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(tableRoutesFile, "adapter = \"codex\"\n", `adapter = "codex"

[routes.claude-work]
host = "anthropic"
auth = "subscription"
account = "dev@example.com"
displayName = "Claude Max (dev@example.com)"

[routes."a.b"]
url = "https://ab.example"
adapter = "openai"
passthrough = true
`, 1)
	if got := readFile(t, path); got != want {
		t.Fatalf("file:\n%s\nwant:\n%s", got, want)
	}

	if err := Add(path, ic, ModelRoute{Key: "opus-work", Model: "claude-opus-5-5", Route: "claude-work"}); err != nil {
		t.Fatal(err)
	}
	f, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, r := range f.Routes {
		names = append(names, r.Name)
	}
	if !slices.Equal(names, []string{"relay", "claude-dev", "codex", "claude-work", "a.b"}) {
		t.Fatalf("route order = %v", names)
	}
	entries, err := Resolve(f, ic)
	if err != nil {
		t.Fatal(err)
	}
	last := entries[len(entries)-1]
	if last.Key != "opus-work" || last.Route != work || last.Binding.Name != "claude-opus-5-5" {
		t.Fatalf("resolved = %+v", last)
	}
}

func TestAddRoutePlacement(t *testing.T) {
	relay := Route{Name: "relay", URL: "https://relay.example"}
	cases := []struct{ name, before, after string }{
		{"before [models] and its comment",
			"catalog = \"v0.1.10\"\n\n# picker\n[models]\n",
			"catalog = \"v0.1.10\"\n\n[routes.relay]\nurl = \"https://relay.example\"\n\n# picker\n[models]\n"},
		{"end of file",
			"catalog = \"v0.1.10\"",
			"catalog = \"v0.1.10\"\n\n[routes.relay]\nurl = \"https://relay.example\"\n"},
		{"flush tables stay flush",
			"catalog = \"v0.1.10\"\n[routes]\nr = { url = \"https://r.example\" }\n[models]\n",
			"catalog = \"v0.1.10\"\n[routes]\nr = { url = \"https://r.example\" }\n[routes.relay]\nurl = \"https://relay.example\"\n[models]\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeRoutes(t, tc.before)
			if err := AddRoute(path, nil, relay); err != nil {
				t.Fatal(err)
			}
			if got := readFile(t, path); got != tc.after {
				t.Fatalf("file:\n%q\nwant:\n%q", got, tc.after)
			}
		})
	}
}

func TestAddRouteRejects(t *testing.T) {
	ic := fixtureIndex(t, fixtureCatalog("v0.1.10"))
	path := writeRoutes(t, tableRoutesFile)
	cases := map[string]Route{
		"duplicate inline name": {Name: "relay", URL: "https://other.example"},
		"duplicate table name":  {Name: "codex", Host: "openai"},
		"no host or url":        {Name: "empty", Auth: "key"},
		"bad auth":              {Name: "bad", URL: "https://x.example", Auth: "oauth"},
		"passthrough adapter":   {Name: "loose", URL: "https://x.example", Passthrough: true},
		"unknown host":          {Name: "ghost", Host: "no-such-host"},
		"empty name":            {URL: "https://x.example"},
	}
	for name, r := range cases {
		if err := AddRoute(path, ic, r); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	if got := readFile(t, path); got != tableRoutesFile {
		t.Fatalf("a rejected add changed the file:\n%s", got)
	}

	// Without a catalog the host is unchecked; Resolve still catches it.
	if err := AddRoute(path, nil, Route{Name: "ghost", Host: "no-such-host"}); err != nil {
		t.Fatal(err)
	}
	f, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(f, ic); err == nil || !strings.Contains(err.Error(), `unknown catalog host "no-such-host"`) {
		t.Fatalf("Resolve: err = %v", err)
	}
}

func TestRemoveRoute(t *testing.T) {
	path := writeRoutes(t, tableRoutesFile)
	err := RemoveRoute(path, "claude-dev")
	if err == nil || !strings.Contains(err.Error(), `used by "claude-opus-5-5", "opus-dev"`) {
		t.Fatalf("route in use: err = %v", err)
	}
	if err := RemoveRoute(path, "nope"); err == nil {
		t.Fatal("unknown route: want error")
	}
	if got := readFile(t, path); got != tableRoutesFile {
		t.Fatal("a refused remove changed the file")
	}

	if err := RemoveRoute(path, "codex"); err != nil {
		t.Fatal(err)
	}
	// The comment flush on the header and the one inside the table go with it.
	want := strings.Replace(tableRoutesFile, codexTable+"\n", "", 1)
	if got := readFile(t, path); got != want {
		t.Fatalf("file:\n%s\nwant:\n%s", got, want)
	}

	if err := Remove(path, "glm-5-3"); err != nil {
		t.Fatal(err)
	}
	if err := RemoveRoute(path, "relay"); err != nil {
		t.Fatal(err)
	}
	want = strings.Replace(strings.Replace(want, "relay = { url = \"https://relay.example\" }\n", "", 1), "\"glm-5-3\"         = \"relay\"\n", "", 1)
	if got := readFile(t, path); got != want {
		t.Fatalf("file:\n%s\nwant:\n%s", got, want)
	}
}

func TestRemoveRouteComments(t *testing.T) {
	const body = `catalog = "v0.1.10"

[routes]
# relay box
relay = { url = "https://relay.example" }
other = { url = "https://o.example" }

# separated by a blank line, so not about [routes.a] alone

[routes.a]
url = "https://a.example"

[routes.b]
url = "https://b.example"
`
	path := writeRoutes(t, body)
	if err := RemoveRoute(path, "relay"); err != nil {
		t.Fatal(err)
	}
	if err := RemoveRoute(path, "a"); err != nil {
		t.Fatal(err)
	}
	want := `catalog = "v0.1.10"

[routes]
other = { url = "https://o.example" }

# separated by a blank line, so not about [routes.a] alone

[routes.b]
url = "https://b.example"
`
	if got := readFile(t, path); got != want {
		t.Fatalf("file:\n%s\nwant:\n%s", got, want)
	}
}

func TestRemoveRouteCollapsesOneBlankLine(t *testing.T) {
	const a, b, c = "[routes.a]\nurl = \"https://a.example\"\n", "[routes.b]\nurl = \"https://b.example\"\n", "[routes.c]\nurl = \"https://c.example\"\n"
	body := "catalog = \"v0.1.10\"\n\n" + a + "\n" + b + "\n" + c
	for route, want := range map[string]string{
		"b": "catalog = \"v0.1.10\"\n\n" + a + "\n" + c,
		"c": "catalog = \"v0.1.10\"\n\n" + a + "\n" + b,
	} {
		path := writeRoutes(t, body)
		if err := RemoveRoute(path, route); err != nil {
			t.Fatal(err)
		}
		if got := readFile(t, path); got != want {
			t.Fatalf("remove %s: file:\n%q\nwant:\n%q", route, got, want)
		}
	}
}

func TestSetRouteDisplayName(t *testing.T) {
	path := writeRoutes(t, tableRoutesFile)
	steps := []struct{ displayName, line string }{
		{"Claude Max (dev@example.com)", "account = \"dev@example.com\"\ndisplayName = \"Claude Max (dev@example.com)\"\n"},
		{`Claude "work" \ max`, "account = \"dev@example.com\"\ndisplayName = \"Claude \\\"work\\\" \\\\ max\"\n"},
		{"", "account = \"dev@example.com\"\n"},
	}
	for _, s := range steps {
		if err := SetRouteDisplayName(path, "claude-dev", s.displayName); err != nil {
			t.Fatal(err)
		}
		want := strings.Replace(tableRoutesFile, "account = \"dev@example.com\"\n", s.line, 1)
		if got := readFile(t, path); got != want {
			t.Fatalf("displayName %q: file:\n%s\nwant:\n%s", s.displayName, got, want)
		}
		f, err := Read(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := f.Routes[1].DisplayName; got != s.displayName {
			t.Fatalf("parsed displayName = %q, want %q", got, s.displayName)
		}
	}

	for name, route := range map[string]string{"inline entry": "relay", "unknown route": "nope"} {
		if err := SetRouteDisplayName(path, route, "x"); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	if err := SetRouteDisplayName(path, "relay", "x"); err == nil || !strings.Contains(err.Error(), "inline entry") {
		t.Fatalf("inline route: err = %v", err)
	}
	if got := readFile(t, path); got != tableRoutesFile {
		t.Fatal("a refused edit changed the file")
	}
}

func TestRouteEditsRefuseUnsafeLayouts(t *testing.T) {
	cases := []struct {
		name, body string
		parses     bool // a file Parse rejects fails before any edit, with a parse error
	}{
		{"inline routes", "catalog = \"v1\"\nroutes = { a = { url = \"https://a.example\" } }\n", true},
		{"multi-line entry", "catalog = \"v1\"\n[routes]\na = { url = \"https://a.example\",\n  adapter = \"openai\" }\n", true},
		{"dotted across lines", "catalog = \"v1\"\n[routes]\na.url = \"https://a.example\"\na.adapter = \"openai\"\n", true},
		{"multi-line string", "catalog = \"v1\"\n[routes.a]\nurl = \"\"\"\nhttps://a.example\"\"\"\n", true},
		{"array of route tables", "catalog = \"v1\"\n[[routes]]\nurl = \"https://a.example\"\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeRoutes(t, tc.body)
			edits := map[string]error{
				"AddRoute":            AddRoute(path, nil, Route{Name: "b", URL: "https://b.example"}),
				"RemoveRoute":         RemoveRoute(path, "a"),
				"SetRouteDisplayName": SetRouteDisplayName(path, "a", "A"),
			}
			for edit, err := range edits {
				if err == nil || tc.parses && !strings.Contains(err.Error(), "by hand") {
					t.Errorf("%s: err = %v, want a refusal", edit, err)
				}
			}
			if got := readFile(t, path); got != tc.body {
				t.Fatalf("a refused edit changed the file:\n%s", got)
			}
		})
	}
}
