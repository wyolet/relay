package clickhouse

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/usage"
)

// fillFields sets every field of the struct v, maps and byte slices empty but
// non-nil when empty is set. A field of a kind it doesn't know fails the
// test, so a new Event field gets checked against decodedEvent.
func fillFields(t *testing.T, v reflect.Value, empty bool) {
	t.Helper()
	for i := range v.NumField() {
		f, name := v.Field(i), v.Type().Field(i).Name
		switch {
		case f.Type() == reflect.TypeFor[time.Time]():
			f.Set(reflect.ValueOf(time.Now()))
		case f.Kind() == reflect.String:
			f.SetString("v-" + name)
		case f.CanInt():
			f.SetInt(int64(i + 1))
		case f.Kind() == reflect.Bool:
			f.SetBool(true)
		case f.Kind() == reflect.Map && f.Type().Key().Kind() == reflect.String:
			m := reflect.MakeMap(f.Type())
			if !empty {
				val := reflect.ValueOf("v")
				if f.Type().Elem().Kind() != reflect.String {
					val = reflect.ValueOf(3)
				}
				m.SetMapIndex(reflect.ValueOf("k").Convert(f.Type().Key()), val.Convert(f.Type().Elem()))
			}
			f.Set(m)
		case f.Kind() == reflect.Pointer && f.Type().Elem().Kind() == reflect.Struct:
			p := reflect.New(f.Type().Elem())
			fillFields(t, p.Elem(), empty)
			f.Set(p)
		case f.Kind() == reflect.Pointer && f.Type().Elem().Kind() == reflect.Int64:
			p := reflect.New(f.Type().Elem())
			p.Elem().SetInt(int64(i + 1))
			f.Set(p)
		default:
			t.Fatalf("field %s (%s): extend this test and check decodedEvent", name, f.Type())
		}
	}
}

// decodedEvent gives what the WAL's JSON round trip gives, for every field.
func TestDecodedEventMatchesJSON(t *testing.T) {
	for _, variant := range []string{"zero", "filled", "empty"} {
		var ev usage.Event
		if variant != "zero" {
			fillFields(t, reflect.ValueOf(&ev).Elem(), variant == "empty")
		}
		b, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		var want usage.Event
		if err := json.Unmarshal(b, &want); err != nil {
			t.Fatal(err)
		}
		got, ok := decodedEvent(ev)
		if !ok || !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: decodedEvent = %+v, %v\nwant %+v", variant, got, ok, want)
		}
	}
}
