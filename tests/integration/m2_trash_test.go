package integration

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/oujinhaoai/lantai/internal/application"
	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	"github.com/oujinhaoai/lantai/internal/query"
	"net/url"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/install"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/ledger"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

// Current task use is an explicit empty T05 fixture. Incoming uses are resolved
// by the real provenance authority and all selected physical files are real.
type lifecycleUsesFixture struct{ e *env }

func (f lifecycleUsesFixture) CurrentUses(ctx context.Context, who authz.Context, _ ids.ID, refs []ids.PermanentRef) ([]ledger.LifecycleUse, error) {
	uses, err := f.e.rights.IncomingUses(ctx, refs)
	if err != nil {
		return nil, err
	}
	out := []ledger.LifecycleUse{}
	seen := map[string]bool{}
	for _, use := range uses {
		key := string(use.Source.VersionID) + use.Relation
		if seen[key] {
			continue
		}
		seen[key] = true
		allowed, err := f.e.rights.EvaluateRiskAccess(ctx, who, use.Source)
		if err != nil {
			return nil, err
		}
		if err = allowed.Err(); err != nil {
			return nil, err
		}
		ref := use.Source
		out = append(out, ledger.LifecycleUse{Kind: use.Relation, ID: use.Source.VersionID, Revision: use.SourceRightsRevision, Ref: &ref, Digest: use.SourceManifestDigest})
	}
	return out, nil
}

type lifecycleFaultFiles struct {
	s         *storage.Service
	failAfter bool
}

func (f *lifecycleFaultFiles) SnapshotLifecycleRecords(ctx context.Context, p []install.Proof) ([]storage.LifecycleRecord, error) {
	return f.s.SnapshotLifecycleRecords(ctx, p)
}
func (f *lifecycleFaultFiles) ApplyFileIntent(ctx context.Context, id ids.ID, source storage.FileIntentSource) error {
	if err := f.s.ApplyFileIntent(ctx, id, source); err != nil {
		return err
	}
	if f.failAfter {
		f.failAfter = false
		return errors.New("synthetic interruption after durable file move")
	}
	return nil
}
func TestM2TrashDurableIntentAndExactFileRecovery(t *testing.T) {
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	ctx := t.Context()
	e.setRole(e.admin.Context.PrincipalID, identity.RoleOwner, true)
	who := e.login().Context
	version := e.ingest(who, "trash-recovery", []byte("synthetic recoverable content"), *rightsOwned())
	oldRead, err := e.storage.IssueReadGrant(ctx, storage.ReadRequest{Who: who, AssetID: version.AssetID, VersionID: version.VersionID, Path: "content.txt", Purpose: authz.PurposeArchiveReview})
	if err != nil {
		t.Fatal(err)
	}
	files := &lifecycleFaultFiles{s: e.storage, failAfter: true}
	life, err := e.ledger.NewLifecycle(files, lifecycleUsesFixture{e}, e.rights, e.id, e.catalog)
	if err != nil {
		t.Fatal(err)
	}
	req, err := life.PreviewTrash(ctx, who, ledger.TrashSelector{ProjectID: e.project.ProjectID, AssetID: version.AssetID, WholeAsset: true, Reason: "Synthetic lifecycle test"})
	if err != nil {
		t.Fatal(err)
	}
	action, err := life.TrashHumanAction(ctx, req, false)
	if err != nil {
		t.Fatal(err)
	}
	ch, err := e.id.CreateDomainChallenge(ctx, who, []identity.HumanAction{action}, life)
	if err != nil {
		t.Fatal(err)
	}
	grant, err := e.id.VerifyChallenge(ctx, who, ch.ChallengeID, e.fresh(), "192.0.2.10")
	if err != nil {
		t.Fatal(err)
	}
	items, err := e.id.DomainItems(ctx, who, grant.GrantID)
	if err != nil {
		t.Fatal(err)
	}
	op := items[0].OperationID
	if _, err = life.TrashHuman(ctx, who, req, false, grant.GrantID, op, e.id); err == nil {
		t.Fatal("interruption not injected")
	}
	state, err := e.ledger.VersionControl(ctx, version.VersionID)
	if err != nil || state.PendingOperationID != op {
		t.Fatal(state, err)
	}
	// Removing only the asset ban must invalidate the physical file authority,
	// even if every version still points at the accepted operation.
	if _, err = e.inst.DB(ownership.Ledger).ExecContext(ctx, `UPDATE ledger_asset_controls SET pending_operation_id='' WHERE asset_id=?`, version.AssetID); err != nil {
		t.Fatal(err)
	}
	if _, err = life.AcceptedFileIntent(ctx, op); errcode.CodeOf(err) != errcode.PreconditionFailed {
		t.Fatal("lost asset ban still authorizes files", err)
	}
	if _, err = e.inst.DB(ownership.Ledger).ExecContext(ctx, `UPDATE ledger_asset_controls SET pending_operation_id=? WHERE asset_id=?`, op, version.AssetID); err != nil {
		t.Fatal(err)
	}
	inv, err := e.ledger.RecoveryInventory(ctx)
	if err != nil || len(inv.Open) != 1 || inv.Open[0].LifecyclePlan == nil {
		t.Fatal(inv, err)
	}
	failures, err := life.Failures(ctx)
	if err != nil || len(failures) != 1 || failures[0].OperationID != op {
		t.Fatal(failures, err)
	}
	if err = e.ledger.CheckVersionRead(ctx, version.AssetID, version.VersionID); errcode.CodeOf(err) != errcode.NotFound {
		t.Fatal("pending file move remained readable", err)
	}
	if err = e.ledger.CheckAssetWrite(ctx, version.AssetID); errcode.CodeOf(err) != errcode.InvalidStateTransition {
		t.Fatal("whole asset remained writable", err)
	}
	receipt, err := life.TrashHuman(ctx, who, req, false, grant.GrantID, op, e.id)
	if err != nil {
		t.Fatal(err)
	}
	var trash ledger.TrashEntry
	if err = json.Unmarshal(receipt.ResponseSummary, &trash); err != nil {
		t.Fatal(err)
	}
	if trash.State != "trashed" || !trash.Grace || trash.RetentionDays != 7 || trash.PendingOperationID != "" {
		t.Fatal(trash)
	}
	claim, err := e.ledger.Claim(ctx, e.project.ProjectID, "trash-recovery")
	if err != nil || claim.State != "released" {
		t.Fatal(claim, err)
	}
	replay, err := life.TrashHuman(ctx, who, req, false, grant.GrantID, op, e.id)
	if err != nil || replay.OperationID != receipt.OperationID {
		t.Fatal(replay, err)
	}
	// The finished physical plan cannot be replayed as a new file mutation.
	if _, err = life.AcceptedFileIntent(ctx, op); errcode.CodeOf(err) != errcode.InvalidStateTransition {
		t.Fatal(err)
	}
	again := e.ingest(who, "trash-recovery", []byte("new unrelated object at released name"), *rightsOwned())
	if again.AssetID == version.AssetID {
		t.Fatal("released name reused permanent asset ID")
	}
	// The trusted risk port can inspect retained bytes, but no ordinary read.
	decision, err := e.rights.EvaluateRiskAccess(ctx, who, version.Ref)
	if err != nil || decision.Err() != nil {
		t.Fatal(decision, err)
	}
	if _, err = e.storage.IssueReadGrant(ctx, storage.ReadRequest{Who: who, AssetID: version.AssetID, VersionID: version.VersionID, Path: "file.bin", Purpose: authz.PurposeArchiveReview}); err == nil {
		t.Fatal("trashed bytes downloadable")
	}
	if _, err = e.storage.VersionFiles(ctx, who, version.AssetID, version.VersionID); errcode.CodeOf(err) != errcode.NotFound {
		t.Fatal("trashed file inventory disclosed", err)
	}
	restore := ledger.RestoreRequest{ProjectID: e.project.ProjectID, TrashID: trash.ID, ExpectedRevision: trash.Revision, Reason: "restore retained content"}
	if _, err = life.Restore(ctx, who, "restore-path-conflict", restore); errcode.CodeOf(err) != errcode.PathConflict {
		t.Fatal("expected explicit collision", err)
	}
	restore.NewSlug = "recovered/trash-recovery"
	files.failAfter = true
	if _, err = life.Restore(ctx, who, "restore-original", restore); err == nil {
		t.Fatal("restore interruption missing")
	}
	if err = e.ledger.CheckVersionRead(ctx, version.AssetID, version.VersionID); errcode.CodeOf(err) != errcode.NotFound {
		t.Fatal(err)
	}
	result, err := life.Restore(ctx, who, "restore-original", restore)
	if err != nil {
		t.Fatal(err)
	}
	var restored ledger.TrashEntry
	if err = json.Unmarshal(result.ResponseSummary, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.State != "restored" || restored.RestoreSlug != restore.NewSlug {
		t.Fatal(restored)
	}
	asset, err := e.ledger.Asset(ctx, version.AssetID)
	if err != nil || asset.Slug != restore.NewSlug {
		t.Fatal(asset, err)
	}
	if err = e.ledger.CheckVersionRead(ctx, version.AssetID, version.VersionID); err != nil {
		t.Fatal(err)
	}
	listed, err := e.storage.VersionFiles(ctx, who, version.AssetID, version.VersionID)
	if err != nil || len(listed) != 1 {
		t.Fatal(listed, err)
	}
	u, _ := url.Parse(oldRead.URL)
	if handle, err := e.storage.OpenRead(ctx, who, oldRead.GrantID, u.Query().Get("sig")); errcode.CodeOf(err) != errcode.TokenRevoked {
		if handle != nil {
			handle.Close()
		}
		t.Fatal("old grant revived after restore", err)
	}
	failures, err = life.Failures(ctx)
	if err != nil || len(failures) != 0 {
		t.Fatal(failures, err)
	}
	state, err = e.ledger.VersionControl(ctx, version.VersionID)
	if err != nil || state.ReviewState != "withdrawn" || state.EffectiveReviewID != "" || state.ReviewTargetID != "" {
		t.Fatal(state, err)
	}
	oldAllocation, err := e.ledger.AliasAllocation(ctx, e.project.ProjectID, "trash-recovery", trash.OriginalGeneration)
	if err != nil || oldAllocation.Asset.AssetID != version.AssetID {
		t.Fatal(oldAllocation, err)
	}
	newAllocation, err := e.ledger.AliasAllocation(ctx, e.project.ProjectID, restore.NewSlug, asset.Generation)
	if err != nil || newAllocation.Reason != "restore" {
		t.Fatal(newAllocation, err)
	}
	replayed, err := life.Restore(ctx, who, "restore-original", restore)
	if err != nil || replayed.OperationID != result.OperationID {
		t.Fatal(replayed, err)
	}
	// Neither old accepted file operation may run again after restoration.
	if _, err = life.AcceptedFileIntent(ctx, result.OperationID); errcode.CodeOf(err) != errcode.InvalidStateTransition {
		t.Fatal(err)
	}
	if _, err = life.AcceptedFileIntent(ctx, op); errcode.CodeOf(err) != errcode.InvalidStateTransition {
		t.Fatal(err)
	}
}

func trashForTest(t *testing.T, e *env, life *ledger.Lifecycle, who authz.Context, asset ids.ID) ledger.TrashEntry {
	t.Helper()
	ctx := t.Context()
	req, err := life.PreviewTrash(ctx, who, ledger.TrashSelector{ProjectID: e.project.ProjectID, AssetID: asset, WholeAsset: true, Reason: "synthetic lifecycle test"})
	if err != nil {
		t.Fatal(err)
	}
	action, err := life.TrashHumanAction(ctx, req, false)
	if err != nil {
		t.Fatal(err)
	}
	ch, err := e.id.CreateDomainChallenge(ctx, who, []identity.HumanAction{action}, life)
	if err != nil {
		t.Fatal(err)
	}
	grant, err := e.id.VerifyChallenge(ctx, who, ch.ChallengeID, e.fresh(), "192.0.2.10")
	if err != nil {
		t.Fatal(err)
	}
	items, err := e.id.DomainItems(ctx, who, grant.GrantID)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := life.TrashHuman(ctx, who, req, false, grant.GrantID, items[0].OperationID, e.id)
	if err != nil {
		t.Fatal(err)
	}
	var entry ledger.TrashEntry
	if err = json.Unmarshal(receipt.ResponseSummary, &entry); err != nil {
		t.Fatal(err)
	}
	return entry
}
func mutateTrashForTest(t *testing.T, e *env, life *ledger.Lifecycle, who authz.Context, entry ledger.TrashEntry, action authz.Action) (ledger.TrashEntry, error) {
	t.Helper()
	ctx := t.Context()
	in := ledger.TrashMutation{Action: action, ProjectID: entry.ProjectID, TrashID: entry.ID, ExpectedRevision: entry.Revision, FilesDigest: entry.FilesDigest, Reason: "synthetic admin decision"}
	a, err := life.MutationHumanAction(ctx, in)
	if err != nil {
		return entry, err
	}
	ch, err := e.id.CreateDomainChallenge(ctx, who, []identity.HumanAction{a}, life)
	if err != nil {
		return entry, err
	}
	grant, err := e.id.VerifyChallenge(ctx, who, ch.ChallengeID, e.fresh(), "192.0.2.10")
	if err != nil {
		return entry, err
	}
	items, err := e.id.DomainItems(ctx, who, grant.GrantID)
	if err != nil {
		return entry, err
	}
	result, err := life.MutateTrashHuman(ctx, who, in, grant.GrantID, items[0].OperationID, e.id)
	if err != nil {
		return entry, err
	}
	if err = json.Unmarshal(result.ResponseSummary, &entry); err != nil {
		return entry, err
	}
	return entry, nil
}
func TestM2HoldPurgeTombstoneAndSharedCAS(t *testing.T) {
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	ctx := t.Context()
	e.setRole(e.admin.Context.PrincipalID, identity.RoleOwner, true)
	who := e.login().Context
	data := []byte("shared retained blob")
	first := e.ingest(who, "purge-one", data, *rightsOwned())
	second := e.ingest(who, "keep-other", data, *rightsOwned())
	life, err := e.ledger.NewLifecycle(e.storage, lifecycleUsesFixture{e}, e.rights, e.id, e.catalog)
	if err != nil {
		t.Fatal(err)
	}
	entry := trashForTest(t, e, life, who, first.AssetID)
	held, err := mutateTrashForTest(t, e, life, who, entry, identity.ActHold)
	if err != nil || !held.Hold || held.DueAt != entry.DueAt {
		t.Fatal(held, err)
	}
	if _, err = mutateTrashForTest(t, e, life, who, held, identity.ActPurge); errcode.CodeOf(err) != errcode.TrashHeld {
		t.Fatal(err)
	}
	unheld, err := mutateTrashForTest(t, e, life, who, held, identity.ActUnhold)
	if err != nil || unheld.Hold || unheld.DueAt != entry.DueAt {
		t.Fatal(unheld, err)
	}
	purged, err := mutateTrashForTest(t, e, life, who, unheld, identity.ActPurge)
	if err != nil || purged.State != "purged" {
		t.Fatal(purged, err)
	}
	if err = e.ledger.CheckVersionRead(ctx, first.AssetID, first.VersionID); errcode.CodeOf(err) != errcode.AssetPurged {
		t.Fatal(err)
	}
	if _, err = life.Restore(ctx, who, e.key(), ledger.RestoreRequest{ProjectID: entry.ProjectID, TrashID: entry.ID, ExpectedRevision: purged.Revision, Reason: "cannot resurrect"}); errcode.CodeOf(err) != errcode.AssetPurged {
		t.Fatal(err)
	}
	raw, err := e.ledger.Version(ctx, second.AssetID, second.VersionID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.storage.ReadManifest(ctx, raw); err != nil {
		t.Fatal("shared survivor lost manifest", err)
	}
	roots, err := life.ManifestRoots(ctx)
	if err != nil || len(roots) != 1 || roots[0].Proof.VersionID != second.VersionID {
		t.Fatal(roots, err)
	}
	refs, err := e.storage.ManifestReferences(ctx, roots)
	if err != nil || len(refs) != 1 {
		t.Fatal(refs, err)
	}
	for sha := range refs {
		candidate, err := life.Candidate(ctx, sha)
		if err != nil || candidate.Since.IsZero() {
			t.Fatal(candidate, err)
		}
	}
	tombstone, err := life.Entry(ctx, who, entry.ProjectID, entry.ID)
	if err != nil || tombstone.State != "purged" || tombstone.OriginalSlug != "" || tombstone.RestoreSlug != "" || tombstone.Reason != "" {
		t.Fatal(tombstone, err)
	}
	app := &application.App{Instance: e.inst, Ledger: e.ledger, Storage: e.storage, Catalog: e.catalog, Rights: e.rights}
	check, err := app.FSCK(ctx, true)
	if err != nil || check.Err() != nil {
		t.Fatal(check.Findings, err)
	}
	_, q := e.eventQuery()
	if _, err = q.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}
	page, err := q.Search(ctx, query.SearchRequest{Who: who, Limit: 10})
	if err != nil || len(page.Items) != 1 || page.Items[0].AssetID != second.AssetID {
		t.Fatal(page, err)
	}
}

// Explicit T08 scheduler fixture: there is no production scheduler enabled by
// these domain tests, and a human/Agent session is not a scheduled job identity.
type dueJobFixture struct{ actor ids.ID }

func (f dueJobFixture) CheckLifecycleJob(_ context.Context, cmd commands.Context, kind string, _ ids.ID) error {
	if cmd.ActorID != f.actor || cmd.SessionID != "" || cmd.HumanGrantID != "" || kind != cmd.CommandType {
		return errcode.New(errcode.Forbidden, "")
	}
	return nil
}
func TestM2DueReminderAndPurgeRequireServerAuthority(t *testing.T) {
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	ctx := t.Context()
	e.setRole(e.admin.Context.PrincipalID, identity.RoleOwner, true)
	who := e.login().Context
	v := e.ingest(who, "scheduled-purge", []byte("synthetic expired object"), *rightsOwned())
	life, err := e.ledger.NewLifecycle(e.storage, lifecycleUsesFixture{e}, e.rights, e.id, e.catalog)
	if err != nil {
		t.Fatal(err)
	}
	entry := trashForTest(t, e, life, who, v.AssetID)
	due, err := clock.Parse(entry.DueAt)
	if err != nil {
		t.Fatal(err)
	}
	scheduler := dueJobFixture{ids.New()}
	run := func(kind string, entry ledger.TrashEntry, authority ledger.LifecycleJobs) (commands.Receipt, error) {
		request := ledger.DueTrashRequest{ProjectID: entry.ProjectID, TrashID: entry.ID, ExpectedRevision: entry.Revision}
		raw, err := canonjson.CanonicalizeValue(request)
		if err != nil {
			return commands.Receipt{}, err
		}
		hash, err := commands.RequestHash(commands.HashInput{CommandType: kind, ProjectID: entry.ProjectID, Body: raw})
		if err != nil {
			return commands.Receipt{}, err
		}
		epoch, err := e.inst.RecoveryEpoch(ctx)
		if err != nil {
			return commands.Receipt{}, err
		}
		cmd := commands.Context{OperationID: ids.New(), IdempotencyKey: e.key(), CommandType: kind, ProjectID: entry.ProjectID, ActorID: scheduler.actor, RequestHash: hash, RecoveryEpoch: epoch}
		return life.RunDueJob(ctx, cmd, request, authority)
	}
	if _, err = run("ledger.purge_due", entry, nil); errcode.CodeOf(err) != errcode.Forbidden {
		t.Fatal(err)
	}
	if _, err = run("ledger.purge_due", entry, scheduler); errcode.CodeOf(err) != errcode.NotDue {
		t.Fatal(err)
	}
	e.clk.Set(due.Add(-72*time.Hour - time.Millisecond))
	if _, err = run("ledger.purge_reminder", entry, scheduler); errcode.CodeOf(err) != errcode.NotDue {
		t.Fatal(err)
	}
	e.clk.Advance(time.Millisecond)
	if _, err = run("ledger.purge_reminder", entry, scheduler); err != nil {
		t.Fatal(err)
	}
	if _, err = run("ledger.purge_reminder", entry, scheduler); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = e.inst.DB(ownership.Ledger).QueryRowContext(ctx, `SELECT count(*) FROM outbox WHERE event_type='trash.purge_due'`).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	who = e.login().Context
	log, collaboration := collaborationForTest(t, e, nil, life, nil)
	e.relay(log)
	if _, err = collaboration.CatchUpInbox(ctx, 1000); err != nil {
		t.Fatal(err)
	}
	inbox, err := collaboration.Inbox(ctx, who, entry.ProjectID)
	if err != nil || len(inbox.Items) != 1 || inbox.Items[0].Object.Ref.ID != entry.ID || inbox.Items[0].Object.DueAt != entry.DueAt {
		t.Fatal(inbox, err)
	}
	held, err := mutateTrashForTest(t, e, life, who, entry, identity.ActHold)
	if err != nil {
		t.Fatal(err)
	}
	inbox, err = collaboration.Inbox(ctx, who, entry.ProjectID)
	if err != nil || len(inbox.Items) != 0 {
		t.Fatal("held trash still actionable", inbox, err)
	}
	e.clk.Set(due)
	if _, err = run("ledger.purge_due", held, scheduler); errcode.CodeOf(err) != errcode.TrashHeld {
		t.Fatal(err)
	}
	who = e.login().Context
	unheld, err := mutateTrashForTest(t, e, life, who, held, identity.ActUnhold)
	if err != nil {
		t.Fatal(err)
	}
	result, err := run("ledger.purge_due", unheld, scheduler)
	if err != nil {
		t.Fatal(err)
	}
	var purged ledger.TrashEntry
	if err = json.Unmarshal(result.ResponseSummary, &purged); err != nil || purged.State != "purged" {
		t.Fatal(purged, err)
	}
}
