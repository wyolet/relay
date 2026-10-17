package apply

import (
	"context"
	"testing"

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/manifest"
)

// apply goes through the row's Validate, so a manifest cannot point a key at relay's own configuration either.
func TestApplyRefusesAHostKeyOnAReservedEnvRef(t *testing.T) {
	for env, refused := range map[string]bool{"RELAY_MASTER_KEY": true, "WYOLET_OIDC_CLIENT_SECRET": true, "OPENAI_API_KEY": false} {
		t.Run(env, func(t *testing.T) {
			rows, _ := storedHostKeyRows(hostkey.ValueKindStored)
			rows.HostKeys = nil
			doc := &manifest.HostKeyDTO{APIVersion: manifest.APIVersion, Kind: "HostKey"}
			doc.Metadata.Name = "env-key"
			doc.Spec = manifest.HostKeySpec{
				HostID: "acme-api", PolicyID: "acme-tier",
				ValueFrom: manifest.HostKeyValueFrom{Kind: string(hostkey.ValueKindEnv), Env: env},
			}
			b := &builder{opts: Options{Stores: &Stores{}}, rows: rows, idx: newIndex(rows)}
			err := b.run(context.Background(), []manifest.Document{{HostKey: doc}})
			if !refused {
				if err != nil {
					t.Fatalf("plan: %v", err)
				}
				return
			}
			wantRefused(t, err, "HostKey", "env-key", hostkey.ErrReservedEnv)
		})
	}
}
