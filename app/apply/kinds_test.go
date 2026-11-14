package apply

import (
	"context"
	"errors"
	"testing"

	"github.com/wyolet/relay/app/license"
	"github.com/wyolet/relay/app/manifest"
)

// Settings and identity rows have their own loaders. Dropping them silently
// makes a bundle report success while half of it went nowhere.
func TestApplyRejectsKindsItCannotWrite(t *testing.T) {
	for _, doc := range []manifest.Document{
		{Setting: &manifest.SettingDTO{Kind: "Setting"}},
		{Foreign: "User"},
	} {
		b := &builder{rows: &Rows{}, lic: license.Community}
		err := b.run(context.Background(), []manifest.Document{doc})
		var unsupported *UnsupportedKindError
		if !errors.As(err, &unsupported) {
			t.Fatalf("run(%s) = %v, want an UnsupportedKindError", doc.Kind(), err)
		}
		if unsupported.Kind != doc.Kind() {
			t.Errorf("reported kind %q, want %q", unsupported.Kind, doc.Kind())
		}
	}
}
