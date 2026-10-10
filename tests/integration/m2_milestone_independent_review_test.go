package integration

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity"
)

// Replaying a durable receipt must recheck the caller's current project role.
// The original M2-07 suite covers changed-payload replay; this independently
// exercises role loss between a successful REST mutation and identical replay.
func TestM2MilestoneIndependentReviewRevokedReplay(t *testing.T) {
	f := newM207(t)
	v := f.create(f.project.ProjectID, "independent milestone authority")
	f.setRole(f.readerSession.Context.PrincipalID, identity.RoleOwner, true)
	base := "projects/" + string(f.project.ProjectID) + "/milestones"
	m := identity.Milestone{ProjectID: f.project.ProjectID, Name: "independent review", OwnerID: f.admin.Context.PrincipalID, TaskIDs: []ids.ID{v.Task.ID}}
	in := map[string]any{"expected_revision": 0, "milestone": m}
	key := f.key()
	var accepted, replay identity.Result
	if err := f.call(f.reader, http.MethodPost, base, key, in, &accepted); err != nil {
		t.Fatal(err)
	}
	if err := f.call(f.reader, http.MethodPost, base, key, in, &replay); err != nil || !replay.Replayed || replay.OperationID != accepted.OperationID {
		t.Fatal("authorized replay failed", replay, err)
	}
	if err := json.Unmarshal(accepted.Summary, &m); err != nil {
		t.Fatal(err)
	}
	f.setRole(f.readerSession.Context.PrincipalID, identity.RoleOwner, false)
	if err := f.call(f.reader, http.MethodPost, base, key, in, nil); m207Code(err) != errcode.Forbidden {
		t.Fatal("receipt bypassed revoked write role", err)
	}
	progressPath := base + "/" + string(m.ID) + "/progress"
	var progress identity.MilestoneProgress
	f.must(f.reader, http.MethodGet, progressPath, nil, &progress)
	if !reflect.DeepEqual(progress.Milestone, m) || progress.Completed != 0 || progress.Total != 1 {
		t.Fatal("rejected replay changed milestone", progress, m)
	}
	f.setRole(f.readerSession.Context.PrincipalID, identity.RoleContributor, false)
	for _, path := range []string{base, progressPath} {
		if err := f.call(f.reader, http.MethodGet, path, "", nil, nil); m207Code(err) != errcode.NotFound {
			t.Fatal("old session read revoked project", path, err)
		}
	}
	if err := f.call(f.reader, http.MethodPost, base, key, in, nil); m207Code(err) != errcode.NotFound {
		t.Fatal("old session replayed revoked project", err)
	}
	var final struct {
		Items []identity.Milestone `json:"items"`
	}
	f.must(f.owner, http.MethodGet, base, nil, &final)
	if len(final.Items) != 1 || !reflect.DeepEqual(final.Items[0], m) {
		t.Fatal("rejected access changed owner facts", final)
	}
	f.evidence(map[string]any{"milestone": m, "accepted_operation": accepted.OperationID, "write_role_revocation": "FORBIDDEN", "project_revocation": "NOT_FOUND", "retained_revision": final.Items[0].Revision})
}
