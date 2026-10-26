package clickhouse

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/payload"
)

// fillFields sets every field of the struct v, byte slices empty but non-nil
// when empty is set. A field of a kind it doesn't know fails the test, so a
// new Record field gets checked against decodedRecord.
func fillFields(t *testing.T, v reflect.Value, empty bool) {
	t.Helper()
	for i := range v.NumField() {
		f, name := v.Field(i), v.Type().Field(i).Name
		switch {
		case f.Type() == reflect.TypeFor[time.Time]():
			f.Set(reflect.ValueOf(time.Now()))
		case f.Kind() == reflect.String:
			f.SetString("v-" + name)
		case f.Kind() == reflect.Bool:
			f.SetBool(true)
		case f.Type() == reflect.TypeFor[[]byte]():
			if empty {
				f.SetBytes([]byte{})
			} else {
				f.SetBytes([]byte("body " + name))
			}
		default:
			t.Fatalf("field %s (%s): extend this test and check decodedRecord", name, f.Type())
		}
	}
}

// decodedRecord gives what the WAL's JSON round trip gives, for every field.
func TestDecodedRecordMatchesJSON(t *testing.T) {
	for _, variant := range []string{"zero", "filled", "empty"} {
		var r payload.Record
		if variant != "zero" {
			fillFields(t, reflect.ValueOf(&r).Elem(), variant == "empty")
		}
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		var want payload.Record
		if err := json.Unmarshal(b, &want); err != nil {
			t.Fatal(err)
		}
		got, ok := decodedRecord(r)
		if !ok || !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: decodedRecord = %+v, %v\nwant %+v", variant, got, ok, want)
		}
	}
}
