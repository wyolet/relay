// Package catalogembed flattens relay's live catalog snapshot into the SDK
// catalog document. The flattening itself lives in pkg/catalogkind/embed,
// which the catalog tools share without depending on the relay server.
package catalogembed

import (
	"time"

	"github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/pkg/catalogkind/binding"
	"github.com/wyolet/relay/pkg/catalogkind/embed"
	"github.com/wyolet/relay/pkg/catalogkind/model"
	sdkcatalog "github.com/wyolet/relay/sdk/catalog"
)

// FlattenScoped is embed.Flatten over snap: only the enabled bindings keep
// accepts; nil keep keeps all.
func FlattenScoped(snap *catalog.Snapshot, at time.Time, keep func(*model.Model, *binding.Binding) bool) *sdkcatalog.Catalog {
	return embed.Flatten(snap, at, keep)
}
