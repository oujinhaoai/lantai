package integration

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/application"
	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/ledger"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

type cancellableTrashFiles struct {
	*storage.Service
	failBefore bool
	failAfter  bool
	verified   chan struct{}
	proceed    chan struct{}
}

func (f *cancellableTrashFiles) VerifyUnmovedTrash(ctx context.Context, plan storage.FileIntent) error {
	if err := f.Service.VerifyUnmovedTrash(ctx, plan); err != nil {
		return err
	}
	if f.verified != nil {
		close(f.verified)
		select {
		case <-f.proceed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (f *cancellableTrashFiles) ApplyFileIntent(ctx context.Context, op ids.ID, source storage.FileIntentSource) error {
	if f.failBefore {
		return errors.New("synthetic failure before file action")
	}
	if err := f.Service.ApplyFileIntent(ctx, op, source); err != nil {
		return err
	}
	if f.failAfter {
		return errors.New("synthetic failure after file action")
	}
	return nil
}

func TestM2CancelUnmovedTrashReleasesQuotaAtomically(t *testing.T) {
	for _, whole := range []bool{false, true} {
		name := "version"
		if whole {
			name = "asset"
		}
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
			ctx := t.Context()
			e.setRole(e.admin.Context.PrincipalID, identity.RoleOwner, true)
			owner := e.login().Context
			_, agent := e.agent("cancel-maker@node", identity.RoleContributor)
			v := e.ingest(agent.Context, "unmoved-delete", []byte("synthetic unmoved bytes"), *rightsOwned())
			if !whole {
				e.clk.Advance(4 * time.Hour)
			}
			files := &cancellableTrashFiles{Service: e.storage, failBefore: true}
			l, err := e.ledger.NewLifecycle(files, lifecycleUsesFixture{e}, e.rights, e.id, e.catalog)
			if err != nil {
				t.Fatal(err)
			}
			selector := ledger.TrashSelector{ProjectID: v.ProjectID, AssetID: v.AssetID, WholeAsset: whole, Reason: "synthetic failed deletion"}
			if !whole {
				selector.VersionID = v.VersionID
			}
			request, err := l.PreviewTrash(ctx, agent.Context, selector)
			if err != nil {
				t.Fatal(err)
			}
			trashKey := e.key()
			if _, err = l.TrashOwn(ctx, agent.Context, trashKey, request); err == nil {
				t.Fatal("failure not injected")
			}
			state, err := e.ledger.VersionControl(ctx, v.VersionID)
			if err != nil || state.PendingOperationID == "" {
				t.Fatal(state, err)
			}
			op := state.PendingOperationID
			db := e.inst.DB(ownership.Ledger)
			var hash digest.Digest
			if err = db.QueryRowContext(ctx, `SELECT request_hash FROM command_receipts WHERE operation_id=?`, op).Scan(&hash); err != nil {
				t.Fatal(err)
			}
			in := ledger.CancelTrashRequest{ProjectID: v.ProjectID, AssetID: v.AssetID, OperationID: op, RequestHash: hash, Reason: "cancel only after verifying original files"}
			if _, err = l.CancelTrash(ctx, agent.Context, e.key(), in); errcode.CodeOf(err) != errcode.Forbidden {
				t.Fatal("non-owner cancelled deletion", err)
			}
			bad := in
			bad.RequestHash = digest.Of([]byte("different deletion"))
			if _, err = l.CancelTrash(ctx, owner, e.key(), bad); errcode.CodeOf(err) != errcode.PreconditionFailed {
				t.Fatal("cancellation accepted wrong request", err)
			}
			key := e.key()
			if _, err = db.ExecContext(ctx, `CREATE TRIGGER fail_quota_release BEFORE UPDATE OF state ON ledger_trash_quota WHEN NEW.state='released' BEGIN SELECT RAISE(ABORT,'synthetic cancellation transaction failure'); END`); err != nil {
				t.Fatal(err)
			}
			if _, err = l.CancelTrash(ctx, owner, key, in); err == nil {
				t.Fatal("transaction failure ignored")
			}
			state, err = e.ledger.VersionControl(ctx, v.VersionID)
			var quota, receiptStatus string
			if err != nil || state.PendingOperationID != op {
				t.Fatal("failed cancellation cleared ban", state, err)
			}
			if err = db.QueryRowContext(ctx, `SELECT state FROM ledger_trash_quota WHERE operation_id=?`, op).Scan(&quota); err != nil || quota != "reserved" {
				t.Fatal(quota, err)
			}
			if err = db.QueryRowContext(ctx, `SELECT status FROM command_receipts WHERE operation_id=?`, op).Scan(&receiptStatus); err != nil || receiptStatus != "in_progress" {
				t.Fatal("failure receipt committed before quota release", receiptStatus, err)
			}
			if _, err = db.ExecContext(ctx, `DROP TRIGGER fail_quota_release`); err != nil {
				t.Fatal(err)
			}
			cancelled, err := l.CancelTrash(ctx, owner, key, in)
			if err != nil || cancelled.Status != commands.ReceiptSucceeded {
				t.Fatal(cancelled, err)
			}
			replay, err := l.CancelTrash(ctx, owner, key, in)
			if err != nil || replay.OperationID != cancelled.OperationID || !bytes.Equal(replay.ResponseSummary, cancelled.ResponseSummary) {
				t.Fatal("cancellation replay differed", replay, err)
			}
			if err = db.QueryRowContext(ctx, `SELECT state FROM ledger_trash_quota WHERE operation_id=?`, op).Scan(&quota); err != nil || quota != "released" {
				t.Fatal(quota, err)
			}
			state, err = e.ledger.VersionControl(ctx, v.VersionID)
			if err != nil || state.Lifecycle != "active" || state.PendingOperationID != "" {
				t.Fatal(state, err)
			}
			if err = e.ledger.CheckAssetWrite(ctx, v.AssetID); err != nil {
				t.Fatal("asset ban remained after verified cancellation", err)
			}
			files.failBefore = false
			old, err := l.TrashOwn(ctx, agent.Context, trashKey, request)
			if err != nil || old.Status != commands.ReceiptFailed || old.OperationID != op {
				t.Fatal("old operation regained deletion authority", old, err)
			}
			if err = e.storage.ApplyFileIntent(ctx, op, l); err == nil {
				t.Fatal("cancelled plan authorized files")
			}
			fresh, err := l.PreviewTrash(ctx, agent.Context, selector)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = l.TrashOwn(ctx, agent.Context, e.key(), fresh); err != nil {
				t.Fatal("fresh exact deletion failed after cancellation", err)
			}
			app := &application.App{Instance: e.inst, Ledger: e.ledger, Storage: e.storage, Catalog: e.catalog, Rights: e.rights}
			report, err := app.FSCK(ctx, true)
			if err != nil || report.Err() != nil {
				t.Fatal(report.Findings, err)
			}
		})
	}
}

func TestM2CancelTrashRefusesCompletedFileMove(t *testing.T) {
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	ctx := t.Context()
	e.setRole(e.admin.Context.PrincipalID, identity.RoleOwner, true)
	owner := e.login().Context
	_, agent := e.agent("moved-maker@node", identity.RoleContributor)
	v := e.ingest(agent.Context, "moved-delete", []byte("synthetic moved bytes"), *rightsOwned())
	files := &cancellableTrashFiles{Service: e.storage, failAfter: true}
	l, err := e.ledger.NewLifecycle(files, lifecycleUsesFixture{e}, e.rights, e.id, e.catalog)
	if err != nil {
		t.Fatal(err)
	}
	request, err := l.PreviewTrash(ctx, agent.Context, ledger.TrashSelector{ProjectID: v.ProjectID, AssetID: v.AssetID, WholeAsset: true, Reason: "synthetic moved deletion"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = l.TrashOwn(ctx, agent.Context, e.key(), request); err == nil {
		t.Fatal("post-move fault not injected")
	}
	state, err := e.ledger.VersionControl(ctx, v.VersionID)
	if err != nil || state.PendingOperationID == "" {
		t.Fatal(state, err)
	}
	op := state.PendingOperationID
	db := e.inst.DB(ownership.Ledger)
	var hash digest.Digest
	if err = db.QueryRowContext(ctx, `SELECT request_hash FROM command_receipts WHERE operation_id=?`, op).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	in := ledger.CancelTrashRequest{ProjectID: v.ProjectID, AssetID: v.AssetID, OperationID: op, RequestHash: hash, Reason: "must not roll back moved files"}
	if _, err = l.CancelTrash(ctx, owner, e.key(), in); errcode.CodeOf(err) != errcode.OperationNeedsReconciliation {
		t.Fatal("moved deletion was cancelled", err)
	}
	state, err = e.ledger.VersionControl(ctx, v.VersionID)
	var quota string
	if err != nil || state.PendingOperationID != op {
		t.Fatal("refused cancellation changed ban", state, err)
	}
	if err = db.QueryRowContext(ctx, `SELECT state FROM ledger_trash_quota WHERE operation_id=?`, op).Scan(&quota); err != nil || quota != "reserved" {
		t.Fatal("unknown result released quota", quota, err)
	}
	files.failAfter = false
	completed, err := l.Resume(ctx, op)
	if err != nil || completed.Status != commands.ReceiptSucceeded {
		t.Fatal(completed, err)
	}
	if _, err = l.CancelTrash(ctx, owner, e.key(), in); errcode.CodeOf(err) != errcode.InvalidStateTransition {
		t.Fatal("completed deletion was cancelled", err)
	}
}

type observedIntentSource struct {
	*ledger.Lifecycle
	seen chan struct{}
	once sync.Once
}

func (s *observedIntentSource) AcceptedFileIntent(ctx context.Context, op ids.ID) (storage.FileIntent, error) {
	plan, err := s.Lifecycle.AcceptedFileIntent(ctx, op)
	s.once.Do(func() { close(s.seen) })
	return plan, err
}

func TestM2CancelTrashSerializesWithFileResume(t *testing.T) {
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	e.setRole(e.admin.Context.PrincipalID, identity.RoleOwner, true)
	owner := e.login().Context
	_, agent := e.agent("concurrent-cancel@node", identity.RoleContributor)
	v := e.ingest(agent.Context, "concurrent-cancel", []byte("synthetic concurrent cancellation"), *rightsOwned())
	files := &cancellableTrashFiles{Service: e.storage, failBefore: true, verified: make(chan struct{}), proceed: make(chan struct{})}
	l, err := e.ledger.NewLifecycle(files, lifecycleUsesFixture{e}, e.rights, e.id, e.catalog)
	if err != nil {
		t.Fatal(err)
	}
	request, err := l.PreviewTrash(ctx, agent.Context, ledger.TrashSelector{ProjectID: v.ProjectID, AssetID: v.AssetID, WholeAsset: true, Reason: "race reconciliation and resume"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = l.TrashOwn(ctx, agent.Context, e.key(), request); err == nil {
		t.Fatal("file fault not injected")
	}
	state, err := e.ledger.VersionControl(ctx, v.VersionID)
	if err != nil {
		t.Fatal(err)
	}
	var hash digest.Digest
	if err = e.inst.DB(ownership.Ledger).QueryRowContext(ctx, `SELECT request_hash FROM command_receipts WHERE operation_id=?`, state.PendingOperationID).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	in := ledger.CancelTrashRequest{ProjectID: v.ProjectID, AssetID: v.AssetID, OperationID: state.PendingOperationID, RequestHash: hash, Reason: "verified original bytes"}
	key := e.key()
	cancelled := make(chan error, 1)
	go func() {
		_, err := l.CancelTrash(ctx, owner, key, in)
		cancelled <- err
	}()
	select {
	case <-files.verified:
	case <-ctx.Done():
		t.Fatal("cancellation did not reach locked verification", ctx.Err())
	}
	source := &observedIntentSource{Lifecycle: l, seen: make(chan struct{})}
	resumed := make(chan error, 1)
	go func() { resumed <- e.storage.ApplyFileIntent(ctx, in.OperationID, source) }()
	select {
	case <-source.seen:
	case <-ctx.Done():
		t.Fatal("file resume did not read initial intent", ctx.Err())
	}
	close(files.proceed)
	if err = <-cancelled; err != nil {
		t.Fatal(err)
	}
	if err = <-resumed; errcode.CodeOf(err) != errcode.InvalidStateTransition {
		t.Fatal("file resume retained stale deletion authority", err)
	}
	if err = e.storage.VerifyDeep(ctx, v.OperationID); err != nil {
		t.Fatal("concurrent resume moved or damaged content", err)
	}
}
