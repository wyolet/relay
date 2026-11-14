package bitwarden

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestDecryptCipher(t *testing.T) {
	key := symKey(0)
	enc := func(s string) *string {
		v := encryptType2String(t, key, s)
		return &v
	}
	foreign := func() *string {
		v := encryptType2String(t, symKey(9), "x")
		return &v
	}
	empty := ""
	uris := func(vals ...*string) []struct {
		URI *string `json:"uri"`
	} {
		out := make([]struct {
			URI *string `json:"uri"`
		}, len(vals))
		for i, v := range vals {
			out[i].URI = v
		}
		return out
	}
	const macFailed = ": bitwarden: MAC verification failed"

	for _, tc := range []struct {
		name    string
		in      syncCipher
		want    decryptedItem
		wantErr string
	}{
		{"every field", syncCipher{
			ID: "c1", Name: *enc("item"), Notes: enc("notes"),
			Login:  &syncLogin{Username: enc("user"), Password: enc("pass"), URI: enc("https://a.example"), URIs: uris(enc("https://b.example"))},
			Fields: []syncField{{Name: enc("k1"), Value: enc("v1")}, {Name: enc("k2")}, {Value: enc("orphan")}, {Name: &empty, Value: enc("orphan")}, {Name: enc("k1"), Value: enc("last wins")}},
		}, decryptedItem{id: "c1", name: "item", notes: "notes", username: "user", password: "pass", uri: "https://a.example", fields: map[string]string{"k1": "last wins", "k2": ""}}, ""},
		{"first of the uri list when there is no single uri", syncCipher{ID: "c1", Name: *enc("item"), Login: &syncLogin{URI: &empty, URIs: uris(enc("https://b.example"), foreign())}},
			decryptedItem{id: "c1", name: "item", uri: "https://b.example", fields: map[string]string{}}, ""},
		{"empty first uri is not skipped", syncCipher{ID: "c1", Name: *enc("item"), Login: &syncLogin{URIs: uris(&empty, enc("https://b.example"))}},
			decryptedItem{id: "c1", name: "item", fields: map[string]string{}}, ""},
		{"no login, empty values", syncCipher{ID: "c1", Name: *enc("item"), Notes: &empty},
			decryptedItem{id: "c1", name: "item", fields: map[string]string{}}, ""},
		{"empty name", syncCipher{ID: "c1"}, decryptedItem{id: "c1", fields: map[string]string{}}, ""},
		{"name", syncCipher{ID: "c1", Name: *foreign()}, decryptedItem{id: "c1", fields: map[string]string{}}, "decrypt name" + macFailed},
		{"notes", syncCipher{ID: "c1", Name: *enc("item"), Notes: foreign()}, decryptedItem{id: "c1", name: "item", fields: map[string]string{}}, "decrypt notes" + macFailed},
		{"username", syncCipher{ID: "c1", Login: &syncLogin{Username: foreign()}}, decryptedItem{id: "c1", fields: map[string]string{}}, "decrypt username" + macFailed},
		{"password", syncCipher{ID: "c1", Login: &syncLogin{Password: foreign()}}, decryptedItem{id: "c1", fields: map[string]string{}}, "decrypt password" + macFailed},
		{"uri", syncCipher{ID: "c1", Login: &syncLogin{URI: foreign()}}, decryptedItem{id: "c1", fields: map[string]string{}}, "decrypt uri" + macFailed},
		{"first listed uri", syncCipher{ID: "c1", Login: &syncLogin{URIs: uris(foreign())}}, decryptedItem{id: "c1", fields: map[string]string{}}, "decrypt uri" + macFailed},
		{"field name", syncCipher{ID: "c1", Fields: []syncField{{Name: foreign()}}}, decryptedItem{id: "c1", fields: map[string]string{}}, "decrypt field name" + macFailed},
		{"field value", syncCipher{ID: "c1", Fields: []syncField{{Name: enc("k"), Value: foreign()}}}, decryptedItem{id: "c1", fields: map[string]string{}}, "decrypt field value" + macFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decryptCipher(tc.in, key)
			if d := cmp.Diff(tc.want, got, cmp.AllowUnexported(decryptedItem{})); d != "" || !wantErr(err, tc.wantErr) {
				t.Errorf("decryptCipher: %v, want %q (-want +got):\n%s", err, tc.wantErr, d)
			}
		})
	}
}
