package integration

import (
	"context"
	"testing"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/ledger"
	"github.com/oujinhaoai/lantai/internal/provenance"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

// Explicit T05 read-port fixture. No production task runtime is claimed here.
type archiveActivityFixture struct {
	busy        map[ids.ID]bool
	projectBusy bool
}

type assetOnlyArchiveActivity struct{ activity *archiveActivityFixture }

func (a assetOnlyArchiveActivity) RequireIdle(ctx context.Context, project ids.ID, assets []ids.ID) error {
	return a.activity.RequireIdle(ctx, project, assets)
}

func TestM2ArchiveRechecksPrivacyAndRequiresProjectActivity(t *testing.T) {
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	ctx := t.Context()
	e.setRole(e.admin.Context.PrincipalID, identity.RoleOwner, true)
	who := e.login().Context
	v := e.ingest(who, "archive-private/one", []byte("synthetic newly private content"), *rightsOwned())
	activity := &archiveActivityFixture{}
	a, err := e.ledger.NewArchival(e.rights, assetOnlyArchiveActivity{activity})
	if err != nil {
		t.Fatal(err)
	}
	project, err := a.PreviewProject(ctx, who, v.ProjectID, true, "project needs its own idle authority")
	if err != nil {
		t.Fatal(err)
	}
	action, err := a.ProjectHumanAction(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	projectGrant, projectItems := grantDomainForTest(t, e, who, []identity.HumanAction{action}, a)
	if _, err = a.ChangeProject(ctx, who, project, projectGrant, projectItems[0].OperationID, e.id); errcode.CodeOf(err) != errcode.AssetInUse {
		t.Fatal("per-asset activity was treated as project authority", err)
	}
	a, err = e.ledger.NewArchival(e.rights, activity)
	if err != nil {
		t.Fatal(err)
	}
	requests, err := a.PreviewDirectory(ctx, who, v.ProjectID, "archive-private", true, "permissions must remain current")
	if err != nil {
		t.Fatal(err)
	}
	actions, err := a.BatchActions(ctx, requests)
	if err != nil {
		t.Fatal(err)
	}
	batchGrant, _ := grantDomainForTest(t, e, who, actions, a)
	evidence := rightsEvidence(t, e, who, v, false)
	personal := "personal"
	if _, err = e.rights.ApplyAssertion(ctx, who, e.key(), provenance.AssertionRequest{Subject: v.Ref, ManifestDigest: v.ManifestDigest, ExpectedRevision: 1, Kind: "restrict", Fields: provenance.AssertionFields{Sensitivity: &personal}, EvidenceIDs: []ids.ID{evidence.RecordID}, Reason: "synthetic privacy change after confirmation"}); err != nil {
		t.Fatal(err)
	}
	if _, err = a.PreviewDirectory(ctx, who, v.ProjectID, "archive-private", true, "hidden targets must not be enumerated"); errcode.CodeOf(err) != errcode.UseRestricted {
		t.Fatal("private directory enumeration accepted", err)
	}
	results, err := a.ChangeBatch(ctx, who, batchGrant, e.id)
	if err != nil || len(results) != 1 || results[0].ErrorCode != errcode.UseRestricted {
		t.Fatal("batch used stale privacy authorization", results, err)
	}
	if _, err = a.ChangeProject(ctx, who, project, projectGrant, projectItems[0].OperationID, e.id); errcode.CodeOf(err) != errcode.UseRestricted {
		t.Fatal("project used stale privacy authorization", err)
	}
	state, err := e.ledger.AssetControl(ctx, v.AssetID)
	if err != nil || state.Lifecycle != "active" {
		t.Fatal(state, err)
	}
}

func (a *archiveActivityFixture) RequireIdle(_ context.Context, _ ids.ID, assets []ids.ID) error {
	for _, asset := range assets {
		if a.busy[asset] {
			return errcode.New(errcode.AssetInUse, "synthetic active modification flow")
		}
	}
	return nil
}
func (a *archiveActivityFixture) RequireProjectIdle(context.Context, ids.ID) error {
	if a.projectBusy {
		return errcode.New(errcode.AssetInUse, "synthetic project task without asset")
	}
	return nil
}

func TestM2DirectoryArchiveFixedBatchPartialResults(t *testing.T) {
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	ctx := t.Context()
	e.setRole(e.admin.Context.PrincipalID, identity.RoleOwner, true)
	who := e.login().Context
	one := e.ingest(who, "archive/one", []byte("first synthetic asset"), *rightsOwned())
	two := e.ingest(who, "archive/two", []byte("second synthetic asset"), *rightsOwned())
	three := e.ingest(who, "archive/three", []byte("third synthetic asset"), *rightsOwned())
	outside := e.ingest(who, "archived-outside", []byte("outside selector"), *rightsOwned())
	activity := &archiveActivityFixture{busy: map[ids.ID]bool{}}
	a, err := e.ledger.NewArchival(e.rights, activity)
	if err != nil {
		t.Fatal(err)
	}
	requests, err := a.PreviewDirectory(ctx, who, one.ProjectID, "archive", true, "archive exact directory selection")
	if err != nil || len(requests) != 3 {
		t.Fatal(requests, err)
	}
	actions, err := a.BatchActions(ctx, requests)
	if err != nil {
		t.Fatal(err)
	}
	grant, _ := grantDomainForTest(t, e, who, actions, a)
	late := e.ingest(who, "archive/late", []byte("created after confirmation"), *rightsOwned())
	appendHistoryVersion(t, e, who, two, []byte("new content after confirmation"))
	activity.busy[one.AssetID] = true
	results, err := a.ChangeBatch(ctx, who, grant, e.id)
	if err != nil || len(results) != 3 {
		t.Fatal(results, err)
	}
	byAsset := map[ids.ID]ledger.TrashBatchItemResult{}
	for _, result := range results {
		byAsset[result.ResourceID] = result
	}
	if byAsset[one.AssetID].ErrorCode != errcode.AssetInUse || byAsset[two.AssetID].ErrorCode != errcode.PreconditionFailed || byAsset[three.AssetID].Receipt == nil {
		t.Fatal("fixed targets or partial outcomes were lost", results)
	}
	activity.busy[one.AssetID] = false
	repeated, err := a.ChangeBatch(ctx, who, grant, e.id)
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range repeated {
		if result.OperationID != byAsset[result.ResourceID].OperationID {
			t.Fatal("batch retry changed child operation", result)
		}
		if result.ResourceID == two.AssetID {
			if result.ErrorCode != errcode.PreconditionFailed {
				t.Fatal("stale version binding was relaxed", result)
			}
		} else if result.Receipt == nil || result.Receipt.Status != commands.ReceiptSucceeded {
			t.Fatal(result)
		}
	}
	for _, id := range []ids.ID{two.AssetID, late.AssetID, outside.AssetID} {
		state, err := e.ledger.AssetControl(ctx, id)
		if err != nil || state.Lifecycle != "active" {
			t.Fatal("unconfirmed target archived", state, err)
		}
	}
	unarchive, err := a.PreviewDirectory(ctx, who, one.ProjectID, "archive", false, "restore archived selection")
	if err != nil || len(unarchive) != 2 {
		t.Fatal(unarchive, err)
	}
	actions, err = a.BatchActions(ctx, unarchive)
	if err != nil {
		t.Fatal(err)
	}
	grant, _ = grantDomainForTest(t, e, who, actions, a)
	results, err = a.ChangeBatch(ctx, who, grant, e.id)
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range results {
		if result.Receipt == nil {
			t.Fatal(result)
		}
	}
}

func TestM2ProjectArchiveExactTargetsAndReadOnlyState(t *testing.T) {
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	ctx := t.Context()
	e.setRole(e.admin.Context.PrincipalID, identity.RoleOwner, true)
	who := e.login().Context
	one := e.ingest(who, "project-archive/one", []byte("synthetic active version"), *rightsOwned())
	separate := e.ingest(who, "separate/one", []byte("individually archived asset"), *rightsOwned())
	activity := &archiveActivityFixture{}
	a, err := e.ledger.NewArchival(e.rights, activity)
	if err != nil {
		t.Fatal(err)
	}
	requests, err := a.PreviewDirectory(ctx, who, one.ProjectID, "separate", true, "separate archive")
	if err != nil {
		t.Fatal(err)
	}
	actions, err := a.BatchActions(ctx, requests)
	if err != nil {
		t.Fatal(err)
	}
	g, _ := grantDomainForTest(t, e, who, actions, a)
	if results, err := a.ChangeBatch(ctx, who, g, e.id); err != nil || len(results) != 1 || results[0].Receipt == nil {
		t.Fatal(results, err)
	}
	request, err := a.PreviewProject(ctx, who, one.ProjectID, true, "archive whole project")
	if err != nil || len(request.Targets) != 2 {
		t.Fatal(request, err)
	}
	action, err := a.ProjectHumanAction(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	g, items := grantDomainForTest(t, e, who, []identity.HumanAction{action}, a)
	e.ingest(who, "project-late", []byte("new asset after confirmation"), *rightsOwned())
	if _, err = a.ChangeProject(ctx, who, request, g, items[0].OperationID, e.id); errcode.CodeOf(err) != errcode.PreconditionFailed {
		t.Fatal("project silently expanded confirmed targets", err)
	}
	request, err = a.PreviewProject(ctx, who, one.ProjectID, true, "archive fresh whole project")
	if err != nil || len(request.Targets) != 3 {
		t.Fatal(request, err)
	}
	action, err = a.ProjectHumanAction(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	g, items = grantDomainForTest(t, e, who, []identity.HumanAction{action}, a)
	activity.projectBusy = true
	if _, err = a.ChangeProject(ctx, who, request, g, items[0].OperationID, e.id); errcode.CodeOf(err) != errcode.AssetInUse {
		t.Fatal("asset-free project task was ignored", err)
	}
	activity.projectBusy = false
	db := e.inst.DB(ownership.Ledger)
	if _, err = db.ExecContext(ctx, `CREATE TRIGGER fail_project_archive BEFORE UPDATE ON ledger_projects BEGIN SELECT RAISE(ABORT,'synthetic project transition failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err = a.ChangeProject(ctx, who, request, g, items[0].OperationID, e.id); err == nil {
		t.Fatal("project transaction failure ignored")
	}
	p, err := e.ledger.Project(ctx, one.ProjectID)
	if err != nil || p.State != commit.ProjectActive {
		t.Fatal(p, err)
	}
	if _, err = db.ExecContext(ctx, `DROP TRIGGER fail_project_archive`); err != nil {
		t.Fatal(err)
	}
	archived, err := a.ChangeProject(ctx, who, request, g, items[0].OperationID, e.id)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.ledger.CheckAssetWrite(ctx, one.AssetID); errcode.CodeOf(err) != errcode.InvalidStateTransition {
		t.Fatal("archived project remained writable", err)
	}
	if err = e.ledger.CheckVersionSearch(ctx, one.AssetID, one.VersionID, false); errcode.CodeOf(err) != errcode.NotFound {
		t.Fatal("archived project appeared in default search", err)
	}
	if err = e.ledger.CheckVersionSearch(ctx, one.AssetID, one.VersionID, true); err != nil {
		t.Fatal(err)
	}
	if _, err = e.catalog.GetVersion(ctx, who, one.AssetID, one.VersionID); err != nil {
		t.Fatal("project archive removed exact historical read", err)
	}
	replay, err := a.ChangeProject(ctx, who, request, g, items[0].OperationID, e.id)
	if err != nil || replay.OperationID != archived.OperationID {
		t.Fatal(replay, err)
	}
	unarchive, err := a.PreviewProject(ctx, who, one.ProjectID, false, "unarchive without restarting tasks")
	if err != nil {
		t.Fatal(err)
	}
	unarchiveAction, err := a.ProjectHumanAction(ctx, unarchive)
	if err != nil {
		t.Fatal(err)
	}
	unarchiveGrant, unarchiveItems := grantDomainForTest(t, e, who, []identity.HumanAction{unarchiveAction}, a)
	if _, err = a.ChangeProject(ctx, who, unarchive, unarchiveGrant, unarchiveItems[0].OperationID, e.id); err != nil {
		t.Fatal(err)
	}
	if err = e.ledger.CheckAssetWrite(ctx, one.AssetID); err != nil {
		t.Fatal(err)
	}
	if err = e.ledger.CheckAssetWrite(ctx, separate.AssetID); errcode.CodeOf(err) != errcode.InvalidStateTransition {
		t.Fatal("project restore removed separate asset archive", err)
	}
	if _, err = a.ChangeProject(ctx, who, request, g, items[0].OperationID, e.id); err != nil {
		t.Fatal(err)
	}
	p, err = e.ledger.Project(ctx, one.ProjectID)
	if err != nil || p.State != commit.ProjectActive {
		t.Fatal("old archive receipt changed current state", p, err)
	}
}
