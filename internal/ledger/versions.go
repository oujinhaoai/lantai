package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/oujinhaoai/lantai/internal/catalog/pathrule"
	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/event"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/install"
)

func (s *Service) Prepare(ctx context.Context, cmd commands.Context, req commit.PrepareRequest) (commit.Prepared, error) {
	var zero commit.Prepared
	if err := cmd.Validate(); err != nil {
		return zero, err
	}
	if cmd.CommandType != commit.CommandType || cmd.ProjectID != req.ProjectID || cmd.ActorID != req.Who.PrincipalID || cmd.SessionID != req.Who.SessionID {
		return zero, invalid("command context does not match prepare request")
	}
	ctx, release, err := s.write(ctx, req.ProjectID)
	if err != nil {
		return zero, err
	}
	defer release()
	if err = s.finalCheck(ctx, cmd, req.Who, commit.ActionCommitVersion, req.ProjectID, "project", req.ProjectID); err != nil {
		return zero, err
	}
	// A replay keeps its frozen IDs and payload, including after a response was lost.
	if r, err := s.lookup(ctx, s.db, cmd); err != nil {
		return zero, err
	} else if r != nil {
		return s.LookupPrepared(ctx, cmd)
	}
	if req.AssetID != "" {
		if err := s.CheckAssetWrite(ctx, req.AssetID); err != nil {
			return zero, err
		}
		if err := s.checkout(ctx, req.Who, cmd, req.ProjectID, req.AssetID); err != nil {
			return zero, err
		}
	} else if err := s.checkPathLock(ctx, req.ProjectID, "", req.Slug); err != nil {
		return zero, err
	} else if cmd.TaskID != "" {
		// 新建资产没有签出，但绑定任务的提交仍须来自当前有效轮次。
		if err := s.checkout(ctx, req.Who, cmd, req.ProjectID, ""); err != nil {
			return zero, err
		}
	}
	response, err := s.store.Accept(ctx, s.db, cmd, commands.StagePrepared, []string{string(req.ProjectID)}, func(ctx context.Context, tx *sql.Tx) error {
		if err := s.unusedOperation(ctx, tx, cmd.OperationID); err != nil {
			return err
		}
		project, err := readJSON[commit.Project](ctx, tx, `SELECT record FROM ledger_projects WHERE project_id = ?`, req.ProjectID)
		if err != nil {
			return err
		}
		if project.State != commit.ProjectActive {
			return conflict()
		}
		assetID := req.AssetID
		var number int64 = 1
		var generation int64
		var prev *commit.Claim
		if assetID == "" {
			if err := pathrule.CheckSlug(req.Slug); err != nil {
				return err
			}
			if req.BaseVersionID != "" {
				return errcode.New(errcode.BaseVersionConflict, "")
			}
			cl, err := readJSON[commit.Claim](ctx, tx, `SELECT record FROM ledger_namespace_claims WHERE project_id = ? AND slug_key = ?`, req.ProjectID, pathrule.Key(req.Slug))
			if err != nil && errcode.CodeOf(err) != errcode.NotFound {
				return err
			}
			generation = 1
			if err == nil {
				if cl.State != commit.ClaimReleased {
					return errcode.New(errcode.PathConflict, "")
				}
				generation = cl.Generation + 1
				prev = &cl
			}
			assetID, err = s.ids.New()
			if err != nil {
				return err
			}
		} else {
			var latest, pending ids.ID
			err := tx.QueryRowContext(ctx, `SELECT next_number,latest_version_id,pending_operation_id FROM ledger_assets WHERE asset_id = ? AND project_id = ? AND record IS NOT NULL`, assetID, req.ProjectID).Scan(&number, &latest, &pending)
			if err != nil {
				return missing(err)
			}
			if pending != "" {
				return busy(pending)
			}
			if latest != req.BaseVersionID {
				return errcode.New(errcode.BaseVersionConflict, "")
			}
		}
		versionID, err := s.ids.New()
		if err != nil {
			return err
		}
		p := commit.Prepared{OperationID: cmd.OperationID, ActorID: cmd.ActorID, SessionID: cmd.SessionID, ProjectID: req.ProjectID, AssetID: assetID, VersionID: versionID, VersionNumber: number, ManifestDigest: req.ManifestDigest, Files: req.Files, AliasGeneration: generation}
		if err := p.InstallRequest().Validate(); err != nil {
			return errcode.Wrap(errcode.SchemaInvalid, "invalid frozen installation", err)
		}
		if generation > 0 {
			_, err = tx.ExecContext(ctx, `INSERT INTO ledger_assets(asset_id,project_id,slug,generation,next_number,pending_operation_id) VALUES(?,?,?,?,?,?)`, assetID, req.ProjectID, req.Slug, generation, number+1, cmd.OperationID)
			if err != nil {
				return err
			}
			claim := commit.Claim{ProjectID: req.ProjectID, Slug: req.Slug, State: commit.ClaimReserved, Generation: generation, AssetID: assetID, OperationID: cmd.OperationID, Revision: 1}
			var old any
			if prev != nil {
				old = encoded(prev)
				claim.Revision = prev.Revision + 1
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO ledger_namespace_claims(project_id,slug_key,record,previous_record) VALUES(?,?,?,?) ON CONFLICT(project_id,slug_key) DO UPDATE SET record=excluded.record,previous_record=excluded.previous_record`, req.ProjectID, pathrule.Key(req.Slug), encoded(claim), old)
			if err != nil {
				return err
			}
		} else {
			_, err = tx.ExecContext(ctx, `UPDATE ledger_assets SET next_number=?,pending_operation_id=? WHERE asset_id=?`, number+1, cmd.OperationID, assetID)
			if err != nil {
				return err
			}
		}
		var baseRevision int64
		if req.BaseVersionID != "" {
			state, e := versionControl(ctx, tx, req.BaseVersionID)
			if e != nil {
				return e
			}
			baseRevision = state.Revision
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO ledger_prepared(operation_id,command,prepared,base_version_id,base_control_revision) VALUES(?,?,?,?,?)`, cmd.OperationID, encoded(cmd), encoded(p), req.BaseVersionID, baseRevision)
		return err
	})
	if err != nil {
		return zero, err
	}
	o, err := s.versionOp(ctx, s.db, response.Receipt.OperationID)
	return o.Prepared, err
}

// MarkInstalled durably records verified installed evidence without making a
// version visible. A retry cannot replace the original installation proof.
func (s *Service) MarkInstalled(ctx context.Context, id ids.ID, who authz.Context, proof install.Proof) error {
	if _, err := proof.Digest(); err != nil {
		return errcode.Wrap(errcode.SchemaInvalid, "invalid installation proof", err)
	}
	o, err := s.versionOp(ctx, s.db, id)
	if err != nil {
		return err
	}
	ctx, release, err := s.write(ctx, o.Prepared.ProjectID)
	if err != nil {
		return err
	}
	defer release()
	if err := s.finalCheck(ctx, o.Command, who, commit.ActionCommitVersion, o.Prepared.ProjectID, "project", o.Prepared.ProjectID); err != nil {
		return err
	}
	in, _, _ := s.verifiers()
	if in == nil {
		return errcode.New(errcode.OperationNeedsReconciliation, "no installer configured")
	}
	if err := proof.Matches(o.Prepared.InstallRequest()); err != nil {
		return err
	}
	if err := in.Verify(ctx, proof); err != nil {
		return err
	}
	return s.markInstalled(ctx, o, proof)
}

func (s *Service) markInstalled(ctx context.Context, o versionOp, proof install.Proof) error {
	return inTx(ctx, s.db, func(tx *sql.Tx) error {
		op, err := s.store.GetOperation(ctx, tx, o.Command.OperationID)
		if err != nil {
			return err
		}
		var raw sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT proof FROM ledger_prepared WHERE operation_id=?`, o.Command.OperationID).Scan(&raw); err != nil {
			return err
		}
		if raw.Valid {
			var old install.Proof
			if err := json.Unmarshal([]byte(raw.String), &old); err != nil {
				return err
			}
			a, err := old.Digest()
			if err != nil {
				return err
			}
			b, err := proof.Digest()
			if err != nil {
				return err
			}
			if a != b {
				return errcode.New(errcode.OperationNeedsReconciliation, "installation proof differs from the durable evidence")
			}
		}
		if op.Stage == commands.StageCommitted || op.Stage == commands.StageInstalled {
			return nil
		}
		if op.Stage.Final() || op.Stage == commands.StageQuarantined {
			return conflict()
		}
		if _, err := tx.ExecContext(ctx, `UPDATE ledger_prepared SET proof=? WHERE operation_id=?`, encoded(proof), o.Command.OperationID); err != nil {
			return err
		}
		return s.store.Advance(ctx, tx, o.Command.OperationID, op.Stage, commands.StageInstalled, commands.Update{ManifestDigest: o.Prepared.ManifestDigest})
	})
}

func (s *Service) Commit(ctx context.Context, id ids.ID, who authz.Context, proof install.Proof) (commit.Committed, error) {
	originalContext := ctx
	var zero commit.Committed
	o, err := s.versionOp(ctx, s.db, id)
	if err != nil {
		return zero, err
	}
	stage, err := s.store.GetOperation(ctx, s.db, id)
	if err != nil {
		return zero, missing(err)
	}
	if stage.Stage == commands.StageCommitted {
		// Receipt replay is a read. Maintenance does not require re-installing
		// bytes or re-running final acceptance for an already committed version.
		lctx, held, err := s.gate.Coordinator().Acquire(ctx, commands.Request{Security: commands.ModeShared})
		if err != nil {
			return zero, err
		}
		defer held.Release()
		return s.replayVersion(lctx, o, who, proof)
	}
	ctx, release, err := s.write(ctx, o.Prepared.ProjectID)
	if err != nil {
		return zero, err
	}
	in, _, acceptance := s.verifiers()
	quarantine := false
	defer func() {
		release()
		if quarantine && in != nil {
			_ = in.Quarantine(context.WithoutCancel(originalContext), id, errcode.OperationNeedsReconciliation)
		}
	}()
	op, err := s.store.GetOperation(ctx, s.db, id)
	if err != nil {
		return zero, missing(err)
	}
	if who.PrincipalID != o.Command.ActorID {
		return zero, errcode.New(errcode.Forbidden, "only the accepting actor can complete this operation")
	}
	pd, err := proof.Digest()
	if err != nil {
		return zero, errcode.Wrap(errcode.SchemaInvalid, "invalid installation proof", err)
	}
	if op.Stage == commands.StageCommitted {
		return s.replayVersion(ctx, o, who, proof)
	}
	if op.Stage.Final() || op.Stage == commands.StageQuarantined {
		return zero, conflict()
	}
	block := func(e error) (commit.Committed, error) {
		if err := s.changeStage(ctx, id, commands.StageBlocked, e); err != nil {
			return zero, err
		}
		return zero, e
	}
	if err := s.finalCheck(ctx, o.Command, who, commit.ActionCommitVersion, o.Prepared.ProjectID, "project", o.Prepared.ProjectID); err != nil {
		return block(err)
	}
	if err := s.checkPreparedWrite(ctx, o.Prepared); err != nil {
		return block(err)
	}
	if o.Prepared.AliasGeneration == 0 || o.Command.TaskID != "" {
		// Lease/checkout are rechecked at the visible commit point; a prepared
		// write from an expired or superseded attempt cannot become committed.
		asset := o.Prepared.AssetID
		if o.Prepared.AliasGeneration > 0 {
			asset = ""
		}
		if err := s.checkout(ctx, who, o.Command, o.Prepared.ProjectID, asset); err != nil {
			return block(err)
		}
	}
	if in == nil || acceptance == nil {
		return block(errcode.New(errcode.OperationNeedsReconciliation, "installation and acceptance verifiers are required"))
	}
	bad := func(e error) (commit.Committed, error) {
		if err := s.changeStage(ctx, id, commands.StageQuarantined, e); err != nil {
			return zero, err
		}
		quarantine = true
		return zero, e
	}
	if err := proof.Matches(o.Prepared.InstallRequest()); err != nil {
		return bad(err)
	}
	if err := in.Verify(ctx, proof); err != nil {
		if code := errcode.CodeOf(err); code == errcode.HashMismatch || code == errcode.OperationNeedsReconciliation {
			return bad(err)
		}
		return zero, err
	}
	if err := s.markInstalled(ctx, o, proof); err != nil {
		if errcode.CodeOf(err) == errcode.OperationNeedsReconciliation {
			return bad(err)
		}
		return zero, err
	}
	if err := acceptance.VerifyAcceptance(ctx, who, o.Prepared); err != nil {
		return block(err)
	}
	p := o.Prepared
	result := commit.Committed{OperationID: id, ProjectID: p.ProjectID, AssetID: p.AssetID, VersionID: p.VersionID, VersionNumber: p.VersionNumber, ManifestDigest: p.ManifestDigest, ProofDigest: pd, CommittedAt: s.clock.Now(), CommittedBy: o.Command.ActorID, AliasGeneration: p.AliasGeneration}
	err = inTx(ctx, s.db, func(tx *sql.Tx) error {
		var latest, pending ids.ID
		var slug string
		var generation int64
		if err := tx.QueryRowContext(ctx, `SELECT latest_version_id,pending_operation_id,slug,generation FROM ledger_assets WHERE asset_id=?`, p.AssetID).Scan(&latest, &pending, &slug, &generation); err != nil {
			return err
		}
		if latest != o.Base {
			return errcode.New(errcode.BaseVersionConflict, "")
		}
		if pending != id {
			return conflict()
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO ledger_versions(version_id,asset_id,project_id,version_number,operation_id,record) VALUES(?,?,?,?,?,?)`, p.VersionID, p.AssetID, p.ProjectID, p.VersionNumber, id, encoded(result)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO ledger_version_states(version_id,revision,state) VALUES(?,1,'draft')`, p.VersionID); err != nil {
			return err
		}
		if p.AliasGeneration > 0 {
			asset := commit.Asset{AssetID: p.AssetID, ProjectID: p.ProjectID, Slug: slug, Generation: generation, CreatedBy: o.Command.ActorID, CreatedAt: result.CommittedAt, OperationID: id}
			if _, err := tx.ExecContext(ctx, `UPDATE ledger_assets SET record=? WHERE asset_id=?`, encoded(asset), p.AssetID); err != nil {
				return err
			}
			if err := saveAliasAllocation(ctx, tx, asset, "create"); err != nil {
				return err
			}
			cl, err := readJSON[commit.Claim](ctx, tx, `SELECT record FROM ledger_namespace_claims WHERE project_id=? AND slug_key=?`, p.ProjectID, pathrule.Key(slug))
			if err != nil {
				return err
			}
			if cl.OperationID != id || cl.State != commit.ClaimReserved {
				return conflict()
			}
			cl.State = commit.ClaimActive
			cl.Revision++
			if _, err := tx.ExecContext(ctx, `UPDATE ledger_namespace_claims SET record=?,previous_record=NULL WHERE project_id=? AND slug_key=?`, encoded(cl), p.ProjectID, pathrule.Key(slug)); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE ledger_assets SET latest_version_id=?,pending_operation_id='' WHERE asset_id=?`, p.VersionID, p.AssetID); err != nil {
			return err
		}
		ev, err := s.event(o.Command, EvVersionCommitted, "version", p.VersionID, 1, map[string]any{"project_id": p.ProjectID, "asset_id": p.AssetID, "version_id": p.VersionID, "version_number": p.VersionNumber, "manifest_digest": p.ManifestDigest})
		if err != nil {
			return err
		}
		return s.store.Complete(ctx, tx, id, commands.StageInstalled, commands.Result{Status: commands.ReceiptSucceeded, ResponseCode: 201, Summary: result, ResultRefs: []commands.ResultRef{{Kind: "version", ID: p.VersionID, Revision: 1}}, Events: []event.Envelope{ev}})
	})
	if err != nil {
		return zero, err
	}
	return result, nil
}

func (s *Service) replayVersion(ctx context.Context, o versionOp, who authz.Context, proof install.Proof) (commit.Committed, error) {
	if who.PrincipalID != o.Command.ActorID {
		return commit.Committed{}, errcode.New(errcode.Forbidden, "only the accepting actor can replay this operation")
	}
	// 重放是同一操作者对同一提交命令的重复请求：复核与最终接受相同的已登记
	// 动作（仅 ingest 范围的会话也能取回原结果），撤权后同样拒绝。
	if err := s.authorize(ctx, who, commit.ActionCommitVersion, o.Prepared.ProjectID, "project", o.Prepared.ProjectID); err != nil {
		return commit.Committed{}, err
	}
	pd, err := proof.Digest()
	if err != nil {
		return commit.Committed{}, errcode.Wrap(errcode.SchemaInvalid, "invalid installation proof", err)
	}
	v, err := s.Version(ctx, o.Prepared.AssetID, o.Prepared.VersionID)
	if err == nil && v.ProofDigest != pd {
		return commit.Committed{}, errcode.New(errcode.OperationNeedsReconciliation, "committed proof differs")
	}
	return v, err
}

func (s *Service) release(ctx context.Context, tx *sql.Tx, id ids.ID) error {
	o, err := s.versionOp(ctx, tx, id)
	if errcode.CodeOf(err) == errcode.NotFound {
		_, err = tx.ExecContext(ctx, `UPDATE ledger_metadata_targets SET pending_operation_id='' WHERE pending_operation_id=?`, id)
		return err
	}
	if err != nil {
		return err
	}
	p := o.Prepared
	if _, err := tx.ExecContext(ctx, `UPDATE ledger_assets SET pending_operation_id='' WHERE asset_id=? AND pending_operation_id=?`, p.AssetID, id); err != nil {
		return err
	}
	if p.AliasGeneration == 0 {
		return nil
	}
	var slug string
	if err := tx.QueryRowContext(ctx, `SELECT slug FROM ledger_assets WHERE asset_id=?`, p.AssetID).Scan(&slug); err != nil {
		return err
	}
	var raw string
	var prev sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT record,previous_record FROM ledger_namespace_claims WHERE project_id=? AND slug_key=?`, p.ProjectID, pathrule.Key(slug)).Scan(&raw, &prev)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var cl commit.Claim
	if err := json.Unmarshal([]byte(raw), &cl); err != nil {
		return err
	}
	if cl.OperationID != id || cl.State != commit.ClaimReserved {
		return nil
	}
	if prev.Valid {
		var old commit.Claim
		if err := json.Unmarshal([]byte(prev.String), &old); err != nil {
			return err
		}
		old.Revision = cl.Revision + 1
		_, err = tx.ExecContext(ctx, `UPDATE ledger_namespace_claims SET record=?,previous_record=NULL WHERE project_id=? AND slug_key=?`, encoded(old), p.ProjectID, pathrule.Key(slug))
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM ledger_namespace_claims WHERE project_id=? AND slug_key=?`, p.ProjectID, pathrule.Key(slug))
	return err
}

func (s *Service) Cancel(ctx context.Context, id ids.ID, who authz.Context) error {
	r, err := s.store.ReceiptByOperation(ctx, s.db, id)
	if err != nil {
		return missing(err)
	}
	if r.OwnerModule != Module || r.Key.ActorID != who.PrincipalID {
		return errcode.New(errcode.NotFound, "")
	}
	ctx, release, err := s.write(ctx, r.Key.ProjectID)
	if err != nil {
		return err
	}
	defer release()
	if err := s.authorize(ctx, who, commit.ActionCommitVersion, r.Key.ProjectID, "project", r.Key.ProjectID); err != nil {
		return err
	}
	return inTx(ctx, s.db, func(tx *sql.Tx) error {
		op, err := s.store.GetOperation(ctx, tx, id)
		if err != nil {
			return missing(err)
		}
		if op.Stage.Final() || op.Stage == commands.StageQuarantined {
			return conflict()
		}
		if err := s.release(ctx, tx, id); err != nil {
			return err
		}
		return s.store.Cancel(ctx, tx, id, op.Stage, errcode.InvalidStateTransition)
	})
}

// QuarantineOrphan never manufactures a commit from installed files. A known
// committed operation cannot be quarantined through this recovery entry point.
func (s *Service) QuarantineOrphan(ctx context.Context, id ids.ID) error {
	if !id.Valid() {
		return invalid("operation id is required")
	}
	in, _, _ := s.verifiers()
	if in == nil {
		return errcode.New(errcode.OperationNeedsReconciliation, "no installer configured")
	}
	op, err := s.store.GetOperation(ctx, s.db, id)
	if err != nil && !errors.Is(err, commands.ErrNotFound) {
		return err
	}
	if op != nil {
		if op.OwnerModule != Module {
			return errcode.New(errcode.NotFound, "")
		}
		if op.Stage == commands.StageCommitted {
			return conflict()
		}
		o, err := s.versionOp(ctx, s.db, id)
		if err != nil {
			return err
		}
		lctx, release, err := s.write(ctx, o.Prepared.ProjectID)
		if err != nil {
			return err
		}
		if !op.Stage.Final() && op.Stage != commands.StageQuarantined {
			err = s.changeStage(lctx, id, commands.StageQuarantined, errcode.New(errcode.OperationNeedsReconciliation, "orphan install needs reconciliation"))
		}
		release()
		if err != nil {
			return err
		}
	}
	return in.Quarantine(ctx, id, errcode.OperationNeedsReconciliation)
}
