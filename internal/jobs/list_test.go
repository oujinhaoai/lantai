package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/oujinhaoai/lantai/internal/contract/authz"
	dc "github.com/oujinhaoai/lantai/internal/contract/device"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/workflow"
	_ "modernc.org/sqlite"
)

type listAuthority struct{ Authority }

func (listAuthority) Authorize(context.Context, authz.Context, authz.Action, authz.Resource) (authz.Decision, error) {
	return authz.Decision{Allowed: true}, nil
}

type listNodes struct {
	Nodes
	deviceNodes
}

func (listNodes) OwnedDevice(context.Context, authz.Context, ids.ID, bool) (dc.Device, error) {
	return dc.Device{}, errcode.New(errcode.Forbidden, "")
}

func TestJobListHiddenDevicesDoNotEndAuthorizedPage(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "runtime.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.ExecContext(t.Context(), `CREATE TABLE jobs_jobs(project_id TEXT,job_id TEXT,record TEXT)`); err != nil {
		t.Fatal(err)
	}
	project := ids.New()
	names := []ids.ID{"01ARZ3NDEKTSV4RRFFQ69G5FAV", "01ARZ3NDEKTSV4RRFFQ69G5FAW", "01ARZ3NDEKTSV4RRFFQ69G5FAX"}
	for i, id := range names {
		j := Job{ID: id, Request: workflow.JobRequest{ProjectID: project}}
		if i < 2 {
			j.Device = &dc.JobData{DeviceID: ids.New()}
		}
		raw, _ := json.Marshal(j)
		if _, err = db.ExecContext(t.Context(), `INSERT INTO jobs_jobs VALUES(?,?,?)`, project, id, string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	s := &Service{d: Deps{DB: db, Authority: listAuthority{}, Nodes: listNodes{}}}
	page, err := s.List(t.Context(), authz.Context{}, project, "", 1)
	if err != nil || len(page) != 1 || page[0].ID != names[2] {
		t.Fatal("hidden device candidate truncated visible list", page, err)
	}
	end, err := s.List(t.Context(), authz.Context{}, project, page[0].ID, 1)
	if err != nil || len(end) != 0 {
		t.Fatal(end, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = s.List(ctx, authz.Context{}, project, "", 1); err == nil {
		t.Fatal("cancelled list succeeded")
	}
}
