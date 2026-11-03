package httpapi

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

func TestBackupPermitExpiryAndSessionBinding(t *testing.T) {
	job, principal, session := ids.New(), ids.New(), ids.New()
	token := strings.Repeat("a", 64)
	for _, tc := range []struct {
		name    string
		expires time.Time
		session ids.ID
		want    string
	}{
		{"expired", time.Now().Add(-time.Hour), session, "fresh package download approval required"},
		{"other session", time.Now().Add(time.Hour), ids.New(), "fresh package download approval required"},
		{"valid permit still needs live admin", time.Now().Add(time.Hour), session, "administrator required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handler{downloads: map[string]backupDownload{downloadHash(token): {principal: principal, session: session, job: job, expires: tc.expires}}}
			r := httptest.NewRequest("GET", "/api/v1/admin/backups/"+string(job)+"/package", nil)
			r.SetPathValue("id", string(job))
			r.Header.Set("X-Lantai-Backup-Download", token)
			err := h.backupDownloadPackage(httptest.NewRecorder(), r, authz.Context{PrincipalID: principal, SessionID: tc.session})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatal("permit boundary", err)
			}
		})
	}
}
