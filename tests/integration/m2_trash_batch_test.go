package integration

import (
	"encoding/json"
	"fmt"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/ledger"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

func grantDomainForTest(t *testing.T, e *env, who authz.Context, actions []identity.HumanAction, validator identity.HumanTargets) (ids.ID, []identity.HumanGrantItem) {
	t.Helper()
	ctx := t.Context()
	ch, err := e.id.CreateDomainChallenge(ctx, who, actions, validator)
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
	return grant.GrantID, items
}
func TestM2OrdinaryTrashRetainsNameUntilExplicitAdminRelease(t *testing.T) {
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	ctx := t.Context()
	e.setRole(e.admin.Context.PrincipalID, identity.RoleOwner, true)
	who := e.login().Context
	v := e.ingest(who, "retained-name", []byte("synthetic retained namespace"), *rightsOwned())
	life, err := e.ledger.NewLifecycle(e.storage, lifecycleUsesFixture{e}, e.rights, e.id, e.catalog)
	if err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(4 * time.Hour)
	who = e.login().Context
	entry := trashForTest(t, e, life, who, v.AssetID)
	if entry.Grace || entry.RetentionDays != 30 {
		t.Fatal(entry)
	}
	claim, err := e.ledger.Claim(ctx, e.project.ProjectID, "retained-name")
	if err != nil || claim.State != commit.ClaimActive {
		t.Fatal("normal trash released name", claim, err)
	}
	result, err := life.Restore(ctx, who, e.key(), ledger.RestoreRequest{ProjectID: entry.ProjectID, TrashID: entry.ID, ExpectedRevision: entry.Revision, Reason: "restore at own retained name"})
	if err != nil {
		t.Fatal(err)
	}
	var restored ledger.TrashEntry
	if err = json.Unmarshal(result.ResponseSummary, &restored); err != nil || restored.RestoreGeneration != entry.OriginalGeneration+1 {
		t.Fatal(restored, err)
	}
	claim, err = e.ledger.Claim(ctx, e.project.ProjectID, "retained-name")
	if err != nil || claim.AssetID != v.AssetID || claim.State != commit.ClaimActive {
		t.Fatal(claim, err)
	}
	release := ledger.NameReleaseRequest{ProjectID: claim.ProjectID, AssetID: claim.AssetID, Slug: claim.Slug, Generation: claim.Generation, ExpectedRevision: claim.Revision, Reason: "explicit admin name release"}
	action, err := life.NameReleaseHumanAction(ctx, release)
	if err != nil {
		t.Fatal(err)
	}
	grant, items := grantDomainForTest(t, e, who, []identity.HumanAction{action}, life)
	if _, err = life.ReleaseNameHuman(ctx, who, release, grant, items[0].OperationID, e.id); errcode.CodeOf(err) != errcode.InvalidStateTransition {
		t.Fatal("live asset name released", err)
	}
	entry = trashForTest(t, e, life, who, v.AssetID)
	purged, err := mutateTrashForTest(t, e, life, who, entry, identity.ActPurge)
	if err != nil || purged.State != "purged" {
		t.Fatal(purged, err)
	}
	claim, err = e.ledger.Claim(ctx, e.project.ProjectID, "retained-name")
	if err != nil || claim.State != commit.ClaimActive {
		t.Fatal("purge released name", claim, err)
	}
	release.ExpectedRevision = claim.Revision
	action, err = life.NameReleaseHumanAction(ctx, release)
	if err != nil {
		t.Fatal(err)
	}
	_, agent := e.agent("release-agent@node", identity.RoleOwner)
	if _, err = e.id.CreateDomainChallenge(ctx, agent.Context, []identity.HumanAction{action}, life); err == nil {
		t.Fatal("agent obtained release approval")
	}
	grant, items = grantDomainForTest(t, e, who, []identity.HumanAction{action}, life)
	altered := release
	altered.Reason = "changed after approval"
	if _, err = life.ReleaseNameHuman(ctx, who, altered, grant, items[0].OperationID, e.id); errcode.CodeOf(err) != errcode.HumanGrantMismatch {
		t.Fatal(err)
	}
	receipt, err := life.ReleaseNameHuman(ctx, who, release, grant, items[0].OperationID, e.id)
	if err != nil {
		t.Fatal(err)
	}
	replacement := e.ingest(who, "retained-name", []byte("synthetic replacement asset"), *rightsOwned())
	after, err := e.ledger.Claim(ctx, e.project.ProjectID, "retained-name")
	if err != nil || after.AssetID != replacement.AssetID || after.Generation != claim.Generation+1 {
		t.Fatal(after, err)
	}
	replay, err := life.ReleaseNameHuman(ctx, who, release, grant, items[0].OperationID, e.id)
	if err != nil || replay.OperationID != receipt.OperationID {
		t.Fatal(replay, err)
	}
	still, err := e.ledger.Claim(ctx, e.project.ProjectID, "retained-name")
	if err != nil || still != after {
		t.Fatal("old release replay altered new claim", still, err)
	}
}
func TestM2DirectoryTrashExactBatchPartialFailureAndReplay(t *testing.T) {
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	ctx := t.Context()
	e.setRole(e.admin.Context.PrincipalID, identity.RoleOwner, true)
	who := e.login().Context
	_, agent := e.agent("batch-maker@node", identity.RoleContributor)
	first := e.ingest(agent.Context, "batch/first", []byte("first synthetic batch object"), *rightsOwned())
	second := e.ingest(agent.Context, "batch/second", []byte("second synthetic batch object"), *rightsOwned())
	outside := e.ingest(who, "batch-other/keep", []byte("outside directory"), *rightsOwned())
	life, err := e.ledger.NewLifecycle(e.storage, lifecycleUsesFixture{e}, e.rights, e.id, e.catalog)
	if err != nil {
		t.Fatal(err)
	}
	agentPreview, err := life.PreviewDirectoryTrash(ctx, agent.Context, e.project.ProjectID, "batch", "synthetic directory deletion")
	if err != nil || len(agentPreview) != 2 {
		t.Fatal(agentPreview, err)
	}
	if _, err = life.TrashOwn(ctx, agent.Context, e.key(), agentPreview[0]); errcode.CodeOf(err) != errcode.HumanProofRequired {
		t.Fatal("directory grace bypass", err)
	}
	requests, err := life.PreviewDirectoryTrash(ctx, who, e.project.ProjectID, "batch", "synthetic directory deletion")
	if err != nil || len(requests) != 2 {
		t.Fatal(requests, err)
	}
	if _, err = life.PreviewTrashBatch(ctx, who, []ledger.TrashSelector{requests[0].TrashSelector, requests[0].TrashSelector}); err == nil {
		t.Fatal("overlapping batch accepted")
	}
	actions, err := life.TrashBatchHumanActions(ctx, requests, false)
	if err != nil {
		t.Fatal(err)
	}
	grant, items := grantDomainForTest(t, e, who, actions, life)
	late := e.ingest(who, "batch/late", []byte("created after exact confirmation"), *rightsOwned())
	lock, err := e.ledger.Lock(ctx, who, e.key(), ledger.LockRequest{ProjectID: e.project.ProjectID, AssetID: second.AssetID, Reason: "intervening review lock"})
	if err != nil {
		t.Fatal(err)
	}
	results, err := life.TrashHumanBatch(ctx, who, grant, e.id)
	if err != nil || len(results) != 2 {
		t.Fatal(results, err)
	}
	succeeded := 0
	for _, r := range results {
		if r.ResourceID == first.AssetID {
			if r.Receipt == nil || r.ErrorCode != "" {
				t.Fatal(r)
			}
			succeeded++
		} else if r.ResourceID == second.AssetID {
			if r.Receipt != nil || r.ErrorCode != errcode.AssetLocked {
				t.Fatal(r)
			}
		} else {
			t.Fatal("batch target expanded", r)
		}
	}
	if succeeded != 1 {
		t.Fatal(results)
	}
	for _, v := range []ids.ID{late.VersionID, outside.VersionID} {
		state, err := e.ledger.VersionControl(ctx, v)
		if err != nil || state.Lifecycle != "active" {
			t.Fatal("unconfirmed object changed", state, err)
		}
	}
	unlock := ledger.ControlMutation{Action: "ledger.unlock", ProjectID: e.project.ProjectID, Kind: "lock", ID: lock.ID, ExpectedRevision: lock.Revision, Reason: "review complete"}
	ua, err := e.ledger.ControlHumanAction(ctx, unlock)
	if err != nil {
		t.Fatal(err)
	}
	ug, ui := grantDomainForTest(t, e, who, []identity.HumanAction{ua}, e.ledger)
	if _, err = e.ledger.ChangeControl(ctx, who, unlock, ug, ui[0].OperationID, e.id, nil); err != nil {
		t.Fatal(err)
	}
	replay, err := life.TrashHumanBatch(ctx, who, grant, e.id)
	if err != nil || len(replay) != 2 {
		t.Fatal(replay, err)
	}
	for i, r := range replay {
		if r.Receipt == nil || r.ErrorCode != "" || r.OperationID != items[i].OperationID {
			t.Fatal(r)
		}
		if results[i].Receipt != nil && r.Receipt.OperationID != results[i].Receipt.OperationID {
			t.Fatal("child operation changed")
		}
	}
	remaining, err := life.PreviewDirectoryTrash(ctx, who, e.project.ProjectID, "batch", "remaining exact scope")
	if err != nil || len(remaining) != 1 || remaining[0].AssetID != late.AssetID {
		t.Fatal(remaining, err)
	}
}

func TestM2AgentQuotaCountsOldReservationsAndCompletionTime(t *testing.T) {
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	ctx := t.Context()
	_, agent := e.agent("quota-maker@node", identity.RoleContributor)
	selectors := []ledger.TrashSelector{}
	for i := range 21 {
		v := e.ingest(agent.Context, fmt.Sprintf("quota/item-%02d", i), []byte(fmt.Sprintf("synthetic quota object %d", i)), *rightsOwned())
		selectors = append(selectors, ledger.TrashSelector{ProjectID: e.project.ProjectID, AssetID: v.AssetID, VersionID: v.VersionID, Reason: "normal own-version deletion"})
	}
	e.clk.Advance(4 * time.Hour)
	files := &lifecycleFaultFiles{s: e.storage}
	life, err := e.ledger.NewLifecycle(files, lifecycleUsesFixture{e}, e.rights, e.id, e.catalog)
	if err != nil {
		t.Fatal(err)
	}
	for _, selector := range selectors[:20] {
		request, err := life.PreviewTrash(ctx, agent.Context, selector)
		if err != nil {
			t.Fatal(err)
		}
		files.failAfter = true
		if _, err = life.TrashOwn(ctx, agent.Context, e.key(), request); err == nil {
			t.Fatal("file interruption was not injected")
		}
	}
	pending, err := life.Failures(ctx)
	if err != nil || len(pending) != 20 {
		t.Fatal(pending, err)
	}
	request, err := life.PreviewTrash(ctx, agent.Context, selectors[20])
	if err != nil {
		t.Fatal(err)
	}
	tryBlocked := func() {
		t.Helper()
		if _, err := life.TrashOwn(ctx, agent.Context, e.key(), request); errcode.CodeOf(err) != errcode.QuotaExceeded {
			t.Fatal("quota bypass", err)
		}
	}
	tryBlocked()
	e.clk.Advance(time.Hour + time.Millisecond)
	tryBlocked()
	completedAt := e.clk.Now()
	receipt, err := life.Resume(ctx, pending[0].OperationID)
	if err != nil {
		t.Fatal(err)
	}
	tryBlocked() // 19 open reservations + this hour's successful deletion.
	e.clk.Advance(time.Hour + time.Millisecond)
	if _, err = life.TrashOwn(ctx, agent.Context, e.key(), request); err != nil {
		t.Fatal("expired successful deletion still counted", err)
	}
	replay, err := life.Resume(ctx, pending[0].OperationID)
	if err != nil || replay.OperationID != receipt.OperationID {
		t.Fatal(replay, err)
	}
	var at int64
	var state string
	if err = e.inst.DB(ownership.Ledger).QueryRowContext(ctx, `SELECT created_at,state FROM ledger_trash_quota WHERE operation_id=?`, receipt.OperationID).Scan(&at, &state); err != nil || at != completedAt.UnixMilli() || state != "committed" {
		t.Fatal("replay changed quota completion time", at, state, err)
	}
}
