package storage

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"os"

	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
)

// VerifyUnmovedTrash is a read-only core reconciliation port. The caller keeps
// the instance security and every target asset lock until its ledger decision
// commits. It never authorizes an inverse move or makes missing bytes safe.
func (s *Service) VerifyUnmovedTrash(ctx context.Context, plan FileIntent) error {
	if err := plan.validate(); err != nil {
		return err
	}
	if plan.Action != "trash" {
		return invalid("only an uncompleted trash intent can be reconciled as unmoved")
	}
	want, err := intentDigest(plan)
	if err != nil {
		return err
	}
	var saved digest.Digest
	var state string
	err = s.db.QueryRowContext(ctx, `SELECT intent_digest,state FROM storage_file_actions WHERE operation_id=?`, plan.OperationID).Scan(&saved, &state)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil && (saved != want || state != "ready") {
		return errcode.New(errcode.OperationNeedsReconciliation, "file action has completed or its plan differs")
	}
	for _, v := range plan.Versions {
		if err = ctx.Err(); err != nil {
			return err
		}
		if err = s.verifyLifecycleTree(ctx, s.layout.VersionDir(v.ProjectID, v.AssetID, v.VersionNumber), v); err != nil {
			return errcode.Wrap(errcode.OperationNeedsReconciliation, "live version is not intact", err)
		}
		if err = s.verifyLifecycleRecords(ctx, s.layout.recordsDir(v.ProjectID, v.AssetID, v.VersionID), v.VersionID, plan.Records); err != nil {
			return errcode.Wrap(errcode.OperationNeedsReconciliation, "live record inventory differs", err)
		}
		for _, path := range []string{s.trashVersion(plan.TrashID, v.VersionID), s.trashRecords(plan.TrashID, v.VersionID)} {
			if err = s.regularTree(path, true); err != nil {
				return err
			}
			if _, err = os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
				return errcode.New(errcode.OperationNeedsReconciliation, "trash destination exists or cannot be verified absent")
			}
		}
	}
	return nil
}
