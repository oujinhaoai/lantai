package ledger

import (
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
)

func TestControlLocksRejectLateCommitAndNewDescendants(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.az.Grant(f.who.PrincipalID, f.project, "ledger.lock")
	p, proof := f.prepare(t, "art/character")
	lock, err := f.s.Lock(ctx, f.who, "lock-art", LockRequest{ProjectID: f.project, Path: "art", Reason: "Review freeze", ExpiresAt: clock.Format(f.clk.Now().Add(time.Minute))})
	if err != nil {
		t.Fatal(err)
	}
	same, err := f.s.Lock(ctx, f.who, "lock-art", LockRequest{ProjectID: f.project, Path: "art", Reason: "Review freeze", ExpiresAt: lock.ExpiresAt})
	if err != nil || same.ID != lock.ID {
		t.Fatal(same, err)
	}
	_, err = f.s.Commit(ctx, p.OperationID, f.who, proof)
	wantCode(t, err, errcode.AssetLocked)
	// Passing the advertised expiry cannot silently let an agent write again.
	f.clk.Advance(2 * time.Minute)
	req := f.req("art/another")
	_, err = f.s.Prepare(ctx, f.command(commit.CommandType, f.project, "new-after-lock", req), req)
	wantCode(t, err, errcode.AssetLocked)
	unlockedReq := f.req("artifact")
	if _, err = f.s.Prepare(ctx, f.command(commit.CommandType, f.project, "other-path", unlockedReq), unlockedReq); err != nil {
		t.Fatal(err)
	}
}
func TestControlReadAndWriteDimensions(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	p, proof := f.prepare(t, "controls")
	v, err := f.s.Commit(ctx, p.OperationID, f.who, proof)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.s.CheckVersionRead(ctx, v.AssetID, v.VersionID); err != nil {
		t.Fatal(err)
	}
	if _, err = f.db.ExecContext(ctx, `UPDATE ledger_version_states SET lifecycle='archived' WHERE version_id=?`, v.VersionID); err != nil {
		t.Fatal(err)
	}
	if err = f.s.CheckVersionRead(ctx, v.AssetID, v.VersionID); err != nil {
		t.Fatal("archived exact read", err)
	}
	wantCode(t, f.s.CheckVersionSearch(ctx, v.AssetID, v.VersionID, false), errcode.NotFound)
	if err = f.s.CheckVersionSearch(ctx, v.AssetID, v.VersionID, true); err != nil {
		t.Fatal(err)
	}
	if _, err = f.db.ExecContext(ctx, `UPDATE ledger_version_states SET availability='disabled' WHERE version_id=?`, v.VersionID); err != nil {
		t.Fatal(err)
	}
	wantCode(t, f.s.CheckVersionRead(ctx, v.AssetID, v.VersionID), errcode.UseRestricted)
	if _, err = f.db.ExecContext(ctx, `UPDATE ledger_version_states SET availability='enabled',lifecycle='purged' WHERE version_id=?`, v.VersionID); err != nil {
		t.Fatal(err)
	}
	wantCode(t, f.s.CheckVersionRead(ctx, v.AssetID, v.VersionID), errcode.AssetPurged)
	// Raw authority remains available for tombstones/recovery.
	if _, err = f.s.Version(ctx, v.AssetID, v.VersionID); err != nil {
		t.Fatal(err)
	}
}

func TestPreparedBaseCannotResumeAfterLifecycleRevisionChange(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	p, proof := f.prepare(t, "base-state")
	base, err := f.s.Commit(ctx, p.OperationID, f.who, proof)
	if err != nil {
		t.Fatal(err)
	}
	req := f.req("next")
	req.Slug = ""
	req.AssetID = base.AssetID
	req.BaseVersionID = base.VersionID
	pending, err := f.s.Prepare(ctx, f.command(commit.CommandType, f.project, "before-risk", req), req)
	if err != nil {
		t.Fatal(err)
	}
	installed, err := f.in.Install(ctx, pending.InstallRequest())
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the final restored state: same immutable version, new control epoch.
	if _, err = f.db.ExecContext(ctx, `UPDATE ledger_version_states SET revision=revision+2 WHERE version_id=?`, base.VersionID); err != nil {
		t.Fatal(err)
	}
	_, err = f.s.Commit(ctx, pending.OperationID, f.who, installed)
	wantCode(t, err, errcode.PreconditionFailed)
}
