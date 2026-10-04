package settings

import (
	"reflect"
	"testing"

	"github.com/wyolet/relay/pkg/secret"
)

// Every secret.Ref a registered section holds must come back from
// SecretRefs, or deleting the secret it names would go unchecked.
func TestSecretRefsCoversEverySection(t *testing.T) {
	refType := reflect.TypeOf(secret.Ref{})
	var count func(reflect.Type) int
	count = func(t reflect.Type) int {
		if t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		if t == refType {
			return 1
		}
		if t.Kind() != reflect.Struct {
			return 0
		}
		n := 0
		for i := range t.NumField() {
			n += count(t.Field(i).Type)
		}
		return n
	}
	for _, name := range Names() {
		sec, _ := Lookup(name)
		v := sec.Defaults()
		if want, got := count(reflect.TypeOf(v)), len(SecretRefs(v)); got != want {
			t.Errorf("section %s holds %d secret refs, SecretRefs returns %d", name, want, got)
		}
	}
}
