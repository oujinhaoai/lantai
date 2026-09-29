package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/oujinhaoai/lantai/internal/application"
	"github.com/oujinhaoai/lantai/internal/catalog"
	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/ledger"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

func appendHistoryVersion(t *testing.T, e *env, who authz.Context, base catalog.VersionResult, data []byte) catalog.VersionResult {
	t.Helper()
	ctx := t.Context()
	u, err := e.storage.CreateUpload(ctx, storage.CreateUploadRequest{Who: who, IdempotencyKey: e.key(), ProjectID: base.ProjectID, Files: []storage.FileSpec{{SHA256: shaOf(data), Size: int64(len(data))}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.storage.PutPart(ctx, storage.PartRequest{Who: who, UploadID: u.UploadID, SHA256: shaOf(data), PartNumber: 1, PartSHA256: shaOf(data), Size: int64(len(data)), Body: bytes.NewReader(data)}); err != nil {
		t.Fatal(err)
	}
	if _, err = e.storage.CompleteFile(ctx, who, u.UploadID, shaOf(data)); err != nil {
		t.Fatal(err)
	}
	v, err := e.catalog.CommitVersion(ctx, catalog.VersionRequest{Who: who, IdempotencyKey: e.key(), UploadID: u.UploadID, AssetID: base.AssetID, BaseVersionID: base.VersionID, Content: catalog.ContentInput{AssetType: manifest.TypeDoc, Rights: rightsOwned(), Files: []manifest.InputFile{{Path: "content.txt", Role: "primary", SHA256: shaOf(data), Size: int64(len(data))}}}})
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func trashVersionHistoryForTest(t *testing.T, e *env, l *ledger.Lifecycle, who authz.Context, v catalog.VersionResult) ledger.TrashEntry {
	t.Helper()
	ctx := t.Context()
	request, err := l.PreviewTrash(ctx, who, ledger.TrashSelector{ProjectID: v.ProjectID, AssetID: v.AssetID, VersionID: v.VersionID, Reason: "synthetic independent historical trash"})
	if err != nil {
		t.Fatal(err)
	}
	a, err := l.TrashHumanAction(ctx, request, false)
	if err != nil {
		t.Fatal(err)
	}
	g, items := grantDomainForTest(t, e, who, []identity.HumanAction{a}, l)
	r, err := l.TrashHuman(ctx, who, request, false, g, items[0].OperationID, e.id)
	if err != nil {
		t.Fatal(err)
	}
	var out ledger.TrashEntry
	if err = json.Unmarshal(r.ResponseSummary, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
func TestM2WholeTrashPreservesIndependentHistoricalRetention(t *testing.T) {
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	ctx := t.Context()
	e.setRole(e.admin.Context.PrincipalID, identity.RoleOwner, true)
	who := e.login().Context
	_, other := e.agent("history-maker@node", identity.RoleContributor)
	v1 := e.ingest(who, "mixed-history", []byte("synthetic first version"), *rightsOwned())
	v2 := appendHistoryVersion(t, e, other.Context, v1, []byte("another author's historical version"))
	v3 := appendHistoryVersion(t, e, who, v2, []byte("synthetic current version"))
	l, err := e.ledger.NewLifecycle(e.storage, lifecycleUsesFixture{e}, e.rights, e.id, e.catalog)
	if err != nil {
		t.Fatal(err)
	}
	old1 := trashVersionHistoryForTest(t, e, l, who, v1)
	_, err = mutateTrashForTest(t, e, l, who, old1, identity.ActPurge)
	if err != nil {
		t.Fatal(err)
	}
	old2 := trashVersionHistoryForTest(t, e, l, who, v2)
	old2, err = mutateTrashForTest(t, e, l, who, old2, identity.ActHold)
	if err != nil {
		t.Fatal(err)
	}
	originalDue, originalRev := old2.DueAt, old2.Revision
	request, err := l.PreviewTrash(ctx, who, ledger.TrashSelector{ProjectID: v1.ProjectID, AssetID: v1.AssetID, WholeAsset: true, Reason: "mixed history whole asset"})
	if err != nil || len(request.Targets) != 1 || request.Targets[0].VersionID != v3.VersionID || len(request.History) != 2 {
		t.Fatal(request, err)
	}
	whole := trashForTest(t, e, l, who, v1.AssetID)
	if whole.Grace || len(whole.VersionIDs) != 1 || len(whole.History) != 2 {
		t.Fatal("historical foreign authorship was ignored", whole)
	}
	if _, err = mutateTrashForTest(t, e, l, who, whole, identity.ActPurge); errcode.CodeOf(err) != errcode.AssetInUse {
		t.Fatal("parent purge bypassed historical hold", err)
	}
	unchanged, err := l.Entry(ctx, who, v1.ProjectID, old2.ID)
	if err != nil || unchanged.DueAt != originalDue || unchanged.Revision != originalRev || !unchanged.Hold {
		t.Fatal("old retention mutated", unchanged, err)
	}
	if _, err = l.Restore(ctx, who, e.key(), ledger.RestoreRequest{ProjectID: whole.ProjectID, TrashID: whole.ID, ExpectedRevision: whole.Revision, Reason: "restore only newly removed versions"}); err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct {
		id    ids.ID
		state string
	}{{v1.VersionID, "purged"}, {v2.VersionID, "trashed"}, {v3.VersionID, "active"}} {
		st, err := e.ledger.VersionControl(ctx, check.id)
		if err != nil || st.Lifecycle != check.state {
			t.Fatal(st, err)
		}
	}
	whole = trashForTest(t, e, l, who, v1.AssetID)
	old2, err = mutateTrashForTest(t, e, l, who, old2, identity.ActUnhold)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = mutateTrashForTest(t, e, l, who, whole, identity.ActPurge); errcode.CodeOf(err) != errcode.AssetInUse {
		t.Fatal("parent purge bypassed independent retained trash", err)
	}
	_, err = mutateTrashForTest(t, e, l, who, old2, identity.ActPurge)
	if err != nil {
		t.Fatal(err)
	}
	whole, err = mutateTrashForTest(t, e, l, who, whole, identity.ActPurge)
	if err != nil || whole.State != "purged" {
		t.Fatal(whole, err)
	}
	state, err := e.ledger.AssetControl(ctx, v1.AssetID)
	if err != nil || state.Lifecycle != "purged" {
		t.Fatal(state, err)
	}
	app := &application.App{Instance: e.inst, Ledger: e.ledger, Storage: e.storage, Catalog: e.catalog, Rights: e.rights}
	report, err := app.FSCK(ctx, true)
	if err != nil || report.Err() != nil {
		t.Fatal(report.Findings, err)
	}
}

type countLifecycleFiles struct {
	*storage.Service
	calls int
}

func (f *countLifecycleFiles) ApplyFileIntent(ctx context.Context, op ids.ID, source storage.FileIntentSource) error {
	f.calls++
	return f.Service.ApplyFileIntent(ctx, op, source)
}
func TestM2AllPurgedAssetHasLogicalLifecycleWithoutFileAuthority(t *testing.T) {
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	ctx := t.Context()
	e.setRole(e.admin.Context.PrincipalID, identity.RoleOwner, true)
	who := e.login().Context
	v := e.ingest(who, "empty-history", []byte("synthetic last physical version"), *rightsOwned())
	files := &countLifecycleFiles{Service: e.storage}
	l, err := e.ledger.NewLifecycle(files, lifecycleUsesFixture{e}, e.rights, e.id, e.catalog)
	if err != nil {
		t.Fatal(err)
	}
	prior := trashVersionHistoryForTest(t, e, l, who, v)
	if _, err = mutateTrashForTest(t, e, l, who, prior, identity.ActPurge); err != nil {
		t.Fatal(err)
	}
	physicalCalls := files.calls
	request, err := l.PreviewTrash(ctx, who, ledger.TrashSelector{ProjectID: v.ProjectID, AssetID: v.AssetID, WholeAsset: true, Reason: "logical container recovery"})
	if err != nil {
		t.Fatal(err)
	}
	action, err := l.TrashHumanAction(ctx, request, false)
	if err != nil {
		t.Fatal(err)
	}
	grant, items := grantDomainForTest(t, e, who, []identity.HumanAction{action}, l)
	db := e.inst.DB(ownership.Ledger)
	if _, err = db.ExecContext(ctx, `CREATE TRIGGER fail_logical_completion BEFORE UPDATE OF state ON ledger_lifecycle_ops WHEN NEW.state='done' BEGIN SELECT RAISE(ABORT,'synthetic final transaction interruption'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err = l.TrashHuman(ctx, who, request, false, grant, items[0].OperationID, e.id); err == nil {
		t.Fatal("completion fault not injected")
	}
	control, err := e.ledger.AssetControl(ctx, v.AssetID)
	if err != nil || control.Lifecycle != "active" || control.PendingOperationID != items[0].OperationID || files.calls != physicalCalls {
		t.Fatal("logical intent lost its durable ban", control, err)
	}
	if _, err = db.ExecContext(ctx, `DROP TRIGGER fail_logical_completion`); err != nil {
		t.Fatal(err)
	}
	completed, err := l.Resume(ctx, items[0].OperationID)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := l.Resume(ctx, items[0].OperationID)
	if err != nil || !bytes.Equal(replay.ResponseSummary, completed.ResponseSummary) {
		t.Fatal("logical recovery replay changed its receipt", err)
	}
	var whole ledger.TrashEntry
	if err = json.Unmarshal(completed.ResponseSummary, &whole); err != nil {
		t.Fatal(err)
	}
	if len(whole.VersionIDs) != 0 || len(whole.History) != 1 || whole.OriginalSlug != "" || whole.Reason != "" {
		t.Fatal(whole)
	}
	r, err := l.Restore(ctx, who, e.key(), ledger.RestoreRequest{ProjectID: whole.ProjectID, TrashID: whole.ID, ExpectedRevision: whole.Revision, Reason: "restore logical asset container"})
	if err != nil {
		t.Fatal(err)
	}
	var restored ledger.TrashEntry
	if err = json.Unmarshal(r.ResponseSummary, &restored); err != nil || restored.State != "restored" || restored.RestoreSlug != "" {
		t.Fatal(restored, err)
	}
	whole = trashForTest(t, e, l, who, v.AssetID)
	if _, err = mutateTrashForTest(t, e, l, who, whole, identity.ActPurge); err != nil {
		t.Fatal(err)
	}
	if files.calls != physicalCalls {
		t.Fatal("logical container unexpectedly authorized files", files.calls, physicalCalls)
	}
	location, err := e.ledger.VersionFileLocation(ctx, v.AssetID, v.VersionID)
	if err != nil || !location.Purged {
		t.Fatal("logical restore revived old content", location, err)
	}
	app := &application.App{Instance: e.inst, Ledger: e.ledger, Storage: e.storage, Catalog: e.catalog, Rights: e.rights}
	report, err := app.FSCK(ctx, true)
	if err != nil || report.Err() != nil {
		t.Fatal(report.Findings, err)
	}
}

func TestM2WholeTrashRejectsChangedHistoricalVersion(t *testing.T) {
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	ctx := t.Context()
	e.setRole(e.admin.Context.PrincipalID, identity.RoleOwner, true)
	who := e.login().Context
	v1 := e.ingest(who, "history-conflict", []byte("synthetic historical version"), *rightsOwned())
	v2 := appendHistoryVersion(t, e, who, v1, []byte("synthetic retained version"))
	files := &countLifecycleFiles{Service: e.storage}
	l, err := e.ledger.NewLifecycle(files, lifecycleUsesFixture{e}, e.rights, e.id, e.catalog)
	if err != nil {
		t.Fatal(err)
	}
	prior := trashVersionHistoryForTest(t, e, l, who, v1)
	request, err := l.PreviewTrash(ctx, who, ledger.TrashSelector{ProjectID: v1.ProjectID, AssetID: v1.AssetID, WholeAsset: true, Reason: "fixed historical state"})
	if err != nil {
		t.Fatal(err)
	}
	action, err := l.TrashHumanAction(ctx, request, false)
	if err != nil {
		t.Fatal(err)
	}
	grant, items := grantDomainForTest(t, e, who, []identity.HumanAction{action}, l)
	if _, err = mutateTrashForTest(t, e, l, who, prior, identity.ActPurge); err != nil {
		t.Fatal(err)
	}
	calls := files.calls
	if _, err = l.TrashHuman(ctx, who, request, false, grant, items[0].OperationID, e.id); errcode.CodeOf(err) != errcode.PreconditionFailed {
		t.Fatal("changed history accepted with stale confirmation", err)
	}
	state, err := e.ledger.VersionControl(ctx, v2.VersionID)
	if err != nil || state.Lifecycle != "active" || state.PendingOperationID != "" || files.calls != calls {
		t.Fatal("rejected confirmation moved retained content", state, files.calls, calls, err)
	}
}

func TestM2PurgeProtectsFirstManifestUntilDescriptionAccepted(t *testing.T) {
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	ctx := t.Context()
	e.setRole(e.admin.Context.PrincipalID, identity.RoleOwner, true)
	who := e.login().Context
	v := e.ingest(who, "pending-description", []byte("synthetic metadata fallback"), *rightsOwned())
	files := &countLifecycleFiles{Service: e.storage}
	l, err := e.ledger.NewLifecycle(files, lifecycleUsesFixture{e}, e.rights, e.id, e.catalog)
	if err != nil {
		t.Fatal(err)
	}
	entry := trashVersionHistoryForTest(t, e, l, who, v)
	// Fault injection models an absent accepted initial description. The first
	// manifest remains its fallback; no deletion may run until authority returns.
	db := e.inst.DB(ownership.Ledger)
	var metadata string
	if err = db.QueryRowContext(ctx, `SELECT record FROM ledger_metadata_targets WHERE kind='asset' AND target_id=?`, v.AssetID).Scan(&metadata); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `UPDATE ledger_metadata_targets SET record=NULL WHERE kind='asset' AND target_id=?`, v.AssetID); err != nil {
		t.Fatal(err)
	}
	calls := files.calls
	if _, err = mutateTrashForTest(t, e, l, who, entry, identity.ActPurge); errcode.CodeOf(err) != errcode.OperationNeedsReconciliation {
		t.Fatal("purge destroyed metadata fallback", err)
	}
	state, err := e.ledger.VersionControl(ctx, v.VersionID)
	if err != nil || state.Lifecycle != "trashed" || state.PendingOperationID != "" || files.calls != calls {
		t.Fatal("rejected purge changed files or accepted an intent", state, err)
	}
	if _, err = db.ExecContext(ctx, `UPDATE ledger_metadata_targets SET record=? WHERE kind='asset' AND target_id=?`, metadata, v.AssetID); err != nil {
		t.Fatal(err)
	}
	if _, err = mutateTrashForTest(t, e, l, who, entry, identity.ActPurge); err != nil {
		t.Fatal(err)
	}
}
