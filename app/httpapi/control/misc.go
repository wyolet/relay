package control

import (
	"context"
	"crypto/rand"
	"encoding/base64"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/httpapi"
)

type versionOutput struct {
	Body struct {
		Version string `json:"version" doc:"Relay build version."`
	}
}

func registerVersion(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "version",
		Method:      "GET",
		Path:        "/version",
		Summary:     "Relay build version",
		Tags:        []string{"system"},
	}, func(_ context.Context, _ *struct{}) (*versionOutput, error) {
		out := &versionOutput{}
		out.Body.Version = httpapi.Version
		return out, nil
	})
}

type masterKeyGenerateOutput struct {
	Body struct {
		Key string `json:"key" doc:"32 random bytes, base64-encoded. Set as RELAY_MASTER_KEY in the deployment env."`
	}
}

func registerMisc(api huma.API, d Deps, protect huma.Middlewares) {
	huma.Register(api, huma.Operation{
		OperationID: "master_key_generate",
		Method:      "POST",
		Path:        "/master-key/generate",
		Summary:     "Generate a new 32-byte master key for stored-mode HostKey encryption",
		Description: "Returns a candidate key — the server does NOT install it. " +
			"Operator copies the value into RELAY_MASTER_KEY and restarts the " +
			"deployment. Rotation requires re-encrypting all stored HostKey values.",
		Tags:        []string{"system"},
		Middlewares: protect,
		Errors:      []int{401, 500},
	}, func(_ context.Context, _ *struct{}) (*masterKeyGenerateOutput, error) {
		buf := make([]byte, 32)
		if _, err := rand.Read(buf); err != nil {
			return nil, huma.Error500InternalServerError("rand: " + err.Error())
		}
		out := &masterKeyGenerateOutput{}
		out.Body.Key = base64.StdEncoding.EncodeToString(buf)
		return out, nil
	})

	type masterKeyRotateOutput struct {
		Body struct {
			Key        string `json:"key"        doc:"New 32-byte master key, base64-encoded. Returned once — operator MUST update RELAY_MASTER_KEY in the deployment env before the next process restart."`
			Rotated    int    `json:"rotated"    doc:"Number of stored-mode HostKey rows re-encrypted."`
			NewVersion int32  `json:"newVersion" doc:"value_key_version assigned to every rotated row."`
		}
	}
	huma.Register(api, huma.Operation{
		OperationID: "master_key_rotate",
		Method:      "POST",
		Path:        "/master-key/rotate",
		Summary:     "Rotate the master key, re-encrypting every stored HostKey",
		Description: "Generates a new 32-byte master key, re-encrypts every " +
			"stored-mode HostKey row in a single transaction, and swaps the " +
			"in-process key on success. The new key is returned once — the " +
			"operator MUST persist it to RELAY_MASTER_KEY in the deployment " +
			"env before the next restart, or the process will fail to decrypt " +
			"stored rows on boot.",
		Tags:        []string{"system"},
		Middlewares: protect,
		Errors:      []int{401, 500},
	}, func(ctx context.Context, _ *struct{}) (*masterKeyRotateOutput, error) {
		if d.Stores == nil || d.Stores.HostKey == nil {
			return nil, huma.Error500InternalServerError("hostkey store not wired")
		}
		newKey := make([]byte, 32)
		if _, err := rand.Read(newKey); err != nil {
			return nil, huma.Error500InternalServerError("rand: " + err.Error())
		}
		res, err := d.Stores.HostKey.Rotate(ctx, newKey)
		if err != nil {
			return nil, huma.Error500InternalServerError("rotate: " + err.Error())
		}
		// Force a catalog reload so the snapshot picks up the new ciphertext
		// decrypted under the new key. The keys themselves don't change, but
		// reloading exercises the decrypt path with the new key — surfacing
		// any mismatch immediately rather than at next NOTIFY.
		if d.Catalog != nil {
			if err := d.Catalog.Reload(ctx); err != nil {
				return nil, huma.Error500InternalServerError("post-rotate reload: " + err.Error())
			}
		}
		out := &masterKeyRotateOutput{}
		out.Body.Key = base64.StdEncoding.EncodeToString(newKey)
		out.Body.Rotated = res.Rotated
		out.Body.NewVersion = res.NewVersion
		return out, nil
	})

	type reloadOutput struct {
		Body struct {
			Status string `json:"status"`
		}
	}
	huma.Register(api, huma.Operation{
		OperationID: "reload",
		Method:      "POST",
		Path:        "/reload",
		Summary:     "Force a full catalog reload from Postgres",
		Description: "Rebuilds the in-memory snapshot from PG. Normally unnecessary " +
			"— NOTIFY/LISTEN propagates writes within ~1s. Use this if you suspect " +
			"the snapshot has drifted (e.g. after a manual DB edit).",
		Tags:        []string{"system"},
		Middlewares: protect,
		Errors:      []int{401, 500},
	}, func(ctx context.Context, _ *struct{}) (*reloadOutput, error) {
		if d.Catalog == nil {
			return nil, huma.Error500InternalServerError("catalog not wired")
		}
		if err := d.Catalog.Reload(ctx); err != nil {
			return nil, huma.Error500InternalServerError("reload: " + err.Error())
		}
		out := &reloadOutput{}
		out.Body.Status = "ok"
		return out, nil
	})
}
