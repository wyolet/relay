package control

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/internal/storage/gen"
)

func putSetting(t *testing.T, h http.Handler, token, section, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/settings/"+section, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestSettingsRetention(t *testing.T) {
	deps := mountDeps(t)
	deps.Stores.Settings = settings.NewStore(gen.New(execOnlyDBTX{}))
	r := chi.NewRouter()
	Mount(r, deps)

	const payloadRest = `"enabled":false,"backend":"file","maxBytes":0,"file":{"path":""},"s3":{"endpoint":"","bucket":"","useSSL":true,"accessKey":{"kind":""},"secretKey":{"kind":""}},"clickhouse":{}`
	const usageRest = `"backend":"clickhouse","file":{},"clickhouse":{}`
	cases := []struct {
		section string
		body    string
		want    int
	}{
		{settings.SectionPayloadLogging, `{` + payloadRest + `,"retentionDays":0}`, 0},
		{settings.SectionPayloadLogging, `{` + payloadRest + `,"retentionDays":7}`, 7},
		{settings.SectionUsageLogging, `{` + usageRest + `,"retentionDays":14}`, 14},
		{settings.SectionAudit, `{"retentionDays":90}`, 90},
	}
	for _, c := range cases {
		w := putSetting(t, r, deps.AdminToken, c.section, c.body)
		if w.Code != http.StatusOK {
			t.Fatalf("PUT %s %s: status = %d: %s", c.section, c.body, w.Code, w.Body.String())
		}
		var out struct {
			Value struct {
				RetentionDays int `json:"retentionDays"`
			} `json:"value"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if out.Value.RetentionDays != c.want {
			t.Fatalf("PUT %s %s: retentionDays = %d, want %d", c.section, c.body, out.Value.RetentionDays, c.want)
		}
	}

	// Omitting retentionDays must be rejected, not read as keep-forever.
	for _, body := range []string{`{` + payloadRest + `}`, `{` + usageRest + `}`, `{` + usageRest + `,"retentionDays":-1}`} {
		section := settings.SectionUsageLogging
		if strings.Contains(body, "enabled") {
			section = settings.SectionPayloadLogging
		}
		if w := putSetting(t, r, deps.AdminToken, section, body); w.Code == http.StatusOK {
			t.Fatalf("PUT %s %s accepted: %s", section, body, w.Body.String())
		}
	}
}
