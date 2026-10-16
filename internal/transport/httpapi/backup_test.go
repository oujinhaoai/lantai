package httpapi

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/oujinhaoai/lantai/internal/backupmgmt"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity"
)

func TestBackupMaintenanceWhitelistIsExact(t *testing.T) {
	id := string(ids.New())
	for _, tc := range []struct {
		method, path string
		want         bool
	}{
		{"GET", "/api/v1/whoami", true}, {"POST", "/api/v1/whoami", false}, {"HEAD", "/api/v1/whoami", false},
		{"GET", "/api/v1/admin/backups", true}, {"GET", "/api/v1/admin/backups/history", true},
		{"GET", "/api/v1/admin/backups/schedule", true}, {"GET", "/api/v1/admin/backups/" + id, true},
		{"POST", "/api/v1/admin/backups/" + id + "/cancel", true},
		{"POST", "/api/v1/admin/backups/" + id + "/resume", false}, {"POST", "/api/v1/admin/backups", false},
		{"PUT", "/api/v1/admin/backups/schedule", false}, {"GET", "/api/v1/admin/backups/invalid", false},
		{"GET", "/api/v1/admin/backups/history/", false}, {"GET", "/api/v1/admin/backups//history", false},
		{"GET", "/api/v1/admin/backups/%68istory", false}, {"GET", "/api/v1/projects", false},
		{"POST", "/api/v1/admin/backups/" + id + "/%63ancel", false}, {"DELETE", "/api/v1/admin/backups/" + id, false},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, tc.path, nil)
			if got := BackupMaintenanceRequest(r); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}
func TestBackupSensitiveParserBindsActionToKind(t *testing.T) {
	for _, tc := range []struct {
		action string
		body   string
		valid  bool
	}{
		{string(identity.ActBackupStart), `{"kind":"start"}`, true},
		{string(identity.ActBackupStart), `{"kind":"resume"}`, false},
		{string(identity.ActBackupStart), `{"kind":"start","destination":"/host/path"}`, false},
		{string(identity.ActBackupSchedule), `{"kind":"schedule","schedule":{"enabled":true,"timezone":"UTC","time":"03:00","revision":0}}`, true},
		{string(identity.ActBackupSchedule), `{"kind":"schedule","schedule":{"enabled":true,"timezone":"UTC","time":"03:00","next_run_at":"2026-01-01"}}`, false},
	} {
		in := SensitiveRequest{}
		_ = json.Unmarshal([]byte(`{"action":"`+tc.action+`","command":`+tc.body+`}`), &in)
		_, err := parseSensitive(in)
		if (err == nil) != tc.valid {
			t.Fatalf("%s: %v", tc.body, err)
		}
	}
}
func TestBackupErrorClassificationDoesNotLeakPaths(t *testing.T) {
	for _, tc := range []struct {
		cause error
		want  errcode.Code
	}{
		{backupmgmt.ErrBusy, errcode.InvalidStateTransition}, {backupmgmt.ErrUnavailable, errcode.StorageUnavailable},
		{backupmgmt.ErrIdempotencyConflict, errcode.IdempotencyConflict}, {backupmgmt.ErrRevisionConflict, errcode.PreconditionFailed},
		{backupmgmt.ErrInvalid, errcode.SchemaInvalid}, {backupmgmt.ErrNotFound, errcode.NotFound},
	} {
		if got := errcode.CodeOf(backupError(errors.Join(tc.cause, errors.New("/private/root")))); got != tc.want {
			t.Fatal(tc.cause, got)
		}
	}
}
