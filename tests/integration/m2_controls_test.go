package integration

import (
	"net/url"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/ledger"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

func TestM2HumanControlsAndOldReadGrant(t *testing.T) {
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	ctx := t.Context()
	e.setRole(e.admin.Context.PrincipalID, identity.RoleOwner, true)
	who := e.login().Context
	v := e.ingest(who, "controlled", []byte("synthetic control bytes"), *rightsOwned())
	grantFor := func(r ledger.ControlMutation) (ids.ID, ids.ID) {
		t.Helper()
		a, err := e.ledger.ControlHumanAction(ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		ch, err := e.id.CreateDomainChallenge(ctx, who, []identity.HumanAction{a}, e.ledger)
		if err != nil {
			t.Fatal(err)
		}
		g, err := e.id.VerifyChallenge(ctx, who, ch.ChallengeID, e.fresh(), "192.0.2.10")
		if err != nil {
			t.Fatal(err)
		}
		items, err := e.id.DomainItems(ctx, who, g.GrantID)
		if err != nil {
			t.Fatal(err)
		}
		return g.GrantID, items[0].OperationID
	}
	read, err := e.storage.IssueReadGrant(ctx, storage.ReadRequest{Who: who, AssetID: v.AssetID, VersionID: v.VersionID, Path: "content.txt", Purpose: authz.PurposeArchiveReview})
	if err != nil {
		t.Fatal(err)
	}
	req := ledger.ControlMutation{Action: "ledger.disable_version", ProjectID: e.project.ProjectID, Kind: "version", ID: v.VersionID, ExpectedRevision: 1, Reason: "Safety investigation"}
	g, op := grantFor(req)
	changed := req
	changed.Reason = "different reason"
	if _, err = e.ledger.ChangeControl(ctx, who, changed, g, op, e.id, nil); errcode.CodeOf(err) != errcode.HumanGrantMismatch {
		t.Fatal(err)
	}
	first, err := e.ledger.ChangeControl(ctx, who, req, g, op, e.id, nil)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(read.URL)
	if h, err := e.storage.OpenRead(ctx, who, read.GrantID, u.Query().Get("sig")); errcode.CodeOf(err) != errcode.UseRestricted {
		if h != nil {
			h.Close()
		}
		t.Fatal("old read grant bypassed disable", err)
	}
	e.clk.Advance(6 * time.Minute)
	replay, err := e.ledger.ChangeControl(ctx, who, req, g, op, e.id, nil)
	if err != nil || replay.OperationID != first.OperationID {
		t.Fatal(replay, err)
	}
	req.Action = "ledger.enable_version"
	req.ExpectedRevision = 2
	req.Reason = "Investigation resolved"
	g, op = grantFor(req)
	if _, err = e.ledger.ChangeControl(ctx, who, req, g, op, e.id, nil); err != nil {
		t.Fatal(err)
	}
	lock, err := e.ledger.Lock(ctx, who, e.key(), ledger.LockRequest{ProjectID: e.project.ProjectID, AssetID: v.AssetID, Reason: "Review freeze"})
	if err != nil {
		t.Fatal(err)
	}
	if err = e.ledger.CheckAssetWrite(ctx, v.AssetID); errcode.CodeOf(err) != errcode.AssetLocked {
		t.Fatal(err)
	}
	req = ledger.ControlMutation{Action: "ledger.unlock", ProjectID: e.project.ProjectID, Kind: "lock", ID: lock.ID, ExpectedRevision: 1, Reason: "Review complete"}
	g, op = grantFor(req)
	if _, err = e.ledger.ChangeControl(ctx, who, req, g, op, e.id, nil); err != nil {
		t.Fatal(err)
	}
	if err = e.ledger.CheckAssetWrite(ctx, v.AssetID); err != nil {
		t.Fatal(err)
	}
	req = ledger.ControlMutation{Action: "ledger.archive", ProjectID: e.project.ProjectID, Kind: "asset", ID: v.AssetID, ExpectedRevision: 1, Reason: "Archive complete work"}
	g, op = grantFor(req)
	if _, err = e.ledger.ChangeControl(ctx, who, req, g, op, e.id, nil); errcode.CodeOf(err) != errcode.AssetInUse {
		t.Fatal("archive without T05 activity check", err)
	}
}
