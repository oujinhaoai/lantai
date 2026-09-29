package ledger

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/oujinhaoai/lantai/internal/catalog/pathrule"
	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/event"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

func (s *Service) RegisterProject(ctx context.Context, cmd commands.Context, req commit.ProjectRequest) (commit.Project, error) {
	var zero commit.Project
	if err := cmd.Validate(); err != nil {
		return zero, err
	}
	if cmd.CommandType != commit.CommandRegisterProject || cmd.ProjectID != "" || cmd.ActorID != req.Who.PrincipalID || cmd.SessionID != req.Who.SessionID {
		return zero, invalid("command does not match project registration")
	}
	if err := pathrule.CheckProjectKey(req.Key); err != nil {
		return zero, err
	}
	ctx, release, err := s.write(ctx, "")
	if err != nil {
		return zero, err
	}
	defer release()
	if err := s.finalCheck(ctx, cmd, req.Who, commit.ActionCreateProject, "", "project", ""); err != nil {
		return zero, err
	}
	response, err := s.store.Execute(ctx, s.db, cmd, func(ctx context.Context, tx *sql.Tx) (commands.Result, error) {
		if err := s.unusedOperation(ctx, tx, cmd.OperationID); err != nil {
			return commands.Result{}, err
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM ledger_projects WHERE project_key=?`, req.Key).Scan(&n); err != nil {
			return commands.Result{}, err
		}
		if n != 0 {
			return commands.Result{}, errcode.New(errcode.PathConflict, "project key already exists")
		}
		id, err := s.ids.New()
		if err != nil {
			return commands.Result{}, err
		}
		p := commit.Project{ProjectID: id, Key: req.Key, State: commit.ProjectActive, CreatedAt: s.clock.Now(), CreatedBy: cmd.ActorID, OperationID: cmd.OperationID}
		if _, err := tx.ExecContext(ctx, `INSERT INTO ledger_projects(project_id,project_key,record) VALUES(?,?,?)`, id, req.Key, encoded(p)); err != nil {
			return commands.Result{}, err
		}
		ev, err := s.event(cmd, EvProjectRegistered, "project", id, 1, map[string]any{"project_id": id, "key": req.Key})
		if err != nil {
			return commands.Result{}, err
		}
		ev.ProjectID = id
		return commands.Result{Status: commands.ReceiptSucceeded, ResponseCode: 201, Summary: p, ResultRefs: []commands.ResultRef{{Kind: "project", ID: id, Revision: 1}}, Events: []event.Envelope{ev}}, nil
	})
	if err != nil {
		return zero, err
	}
	var p commit.Project
	err = json.Unmarshal(response.Receipt.ResponseSummary, &p)
	return p, err
}

type metadataOp struct {
	AssetControlRevision int64
	Command              commands.Context
	Request              commit.MetadataRequest
	Prepared             commit.PreparedMetadata
	ProofDigest          digest.Digest
}

func (s *Service) metadataOp(ctx context.Context, q commands.DBTX, id ids.ID) (metadataOp, error) {
	var o metadataOp
	var cmd, req, p string
	var proof sql.NullString
	if err := q.QueryRowContext(ctx, `SELECT command,request,prepared,proof_digest,asset_control_revision FROM ledger_metadata_prepared WHERE operation_id=?`, id).Scan(&cmd, &req, &p, &proof, &o.AssetControlRevision); err != nil {
		return o, missing(err)
	}
	if err := json.Unmarshal([]byte(cmd), &o.Command); err != nil {
		return o, err
	}
	if err := json.Unmarshal([]byte(req), &o.Request); err != nil {
		return o, err
	}
	if err := json.Unmarshal([]byte(p), &o.Prepared); err != nil {
		return o, err
	}
	o.ProofDigest = digest.Digest(proof.String)
	return o, nil
}

func (s *Service) checkTarget(ctx context.Context, q commands.DBTX, t commit.MetadataTarget, who authz.Context, action authz.Action) error {
	if t.Kind != commit.TargetProject && t.Kind != commit.TargetAsset || !t.ID.Valid() || !t.ProjectID.Valid() {
		return invalid("invalid metadata target")
	}
	var owner ids.ID
	switch t.Kind {
	case commit.TargetProject:
		if t.ID != t.ProjectID {
			return errcode.New(errcode.NotFound, "")
		}
		p, err := readJSON[commit.Project](ctx, q, `SELECT record FROM ledger_projects WHERE project_id=?`, t.ID)
		if err != nil {
			return err
		}
		if p.State != commit.ProjectActive {
			return conflict()
		}
		owner = p.CreatedBy
	case commit.TargetAsset:
		a, err := readJSON[commit.Asset](ctx, q, `SELECT record FROM ledger_assets WHERE asset_id=? AND project_id=? AND record IS NOT NULL`, t.ID, t.ProjectID)
		if err != nil {
			return err
		}
		owner = a.CreatedBy
	}
	if action == "catalog.patch_own_metadata" && owner != who.PrincipalID {
		return errcode.New(errcode.Forbidden, "own metadata permission requires ownership")
	}
	return nil
}

func (s *Service) PrepareMetadata(ctx context.Context, cmd commands.Context, req commit.MetadataRequest) (commit.PreparedMetadata, error) {
	var zero commit.PreparedMetadata
	t := req.Target
	if err := cmd.Validate(); err != nil {
		return zero, err
	}
	if cmd.CommandType != commit.CommandCommitMetadata || cmd.ProjectID != t.ProjectID || cmd.ActorID != req.Who.PrincipalID || cmd.SessionID != req.Who.SessionID {
		return zero, invalid("command does not match metadata request")
	}
	validAction := t.Kind == commit.TargetProject && req.Action == "catalog.patch_project" || t.Kind == commit.TargetAsset && (req.Action == "catalog.patch_metadata" || req.Action == "catalog.patch_own_metadata")
	if !req.ContentDigest.Valid() || req.ExpectedRevision < 0 || !validAction {
		return zero, invalid("invalid metadata request")
	}
	ctx, release, err := s.write(ctx, t.ProjectID)
	if err != nil {
		return zero, err
	}
	defer release()
	if err := s.finalCheck(ctx, cmd, req.Who, req.Action, t.ProjectID, string(t.Kind), t.ID); err != nil {
		return zero, err
	}
	if r, err := s.lookup(ctx, s.db, cmd); err != nil {
		return zero, err
	} else if r != nil {
		o, err := s.metadataOp(ctx, s.db, r.OperationID)
		return o.Prepared, err
	}
	if t.Kind == commit.TargetAsset {
		if err := s.CheckAssetWrite(ctx, t.ID); err != nil {
			return zero, err
		}
	}
	response, err := s.store.Accept(ctx, s.db, cmd, commands.StagePrepared, []string{string(t.ID)}, func(ctx context.Context, tx *sql.Tx) error {
		if err := s.unusedOperation(ctx, tx, cmd.OperationID); err != nil {
			return err
		}
		if err := s.checkTarget(ctx, tx, t, req.Who, req.Action); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO ledger_metadata_targets(kind,target_id,project_id) VALUES(?,?,?)`, t.Kind, t.ID, t.ProjectID); err != nil {
			return err
		}
		var cur int64
		var pending ids.ID
		if err := tx.QueryRowContext(ctx, `SELECT revision,pending_operation_id FROM ledger_metadata_targets WHERE kind=? AND target_id=?`, t.Kind, t.ID).Scan(&cur, &pending); err != nil {
			return err
		}
		if pending != "" {
			return busy(pending)
		}
		if cur != req.ExpectedRevision {
			return errcode.New(errcode.PreconditionFailed, "metadata revision changed")
		}
		p := commit.PreparedMetadata{OperationID: cmd.OperationID, Target: t, Revision: cur + 1, ContentDigest: req.ContentDigest}
		// The durable payload needs the authorized action, not the authentication
		// context (which may contain ephemeral credential information).
		saved := req
		saved.Who = authz.Context{}
		var controlRevision int64
		if t.Kind == commit.TargetAsset {
			control, err := assetControl(ctx, tx, t.ID)
			if err != nil {
				return err
			}
			controlRevision = control.Revision
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO ledger_metadata_prepared(operation_id,command,request,prepared,asset_control_revision) VALUES(?,?,?,?,?)`, cmd.OperationID, encoded(cmd), encoded(saved), encoded(p), controlRevision); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE ledger_metadata_targets SET pending_operation_id=? WHERE kind=? AND target_id=?`, cmd.OperationID, t.Kind, t.ID)
		return err
	})
	if err != nil {
		return zero, err
	}
	o, err := s.metadataOp(ctx, s.db, response.Receipt.OperationID)
	return o.Prepared, err
}

func (s *Service) CommitMetadata(ctx context.Context, id ids.ID, who authz.Context, proof commit.RevisionProof) (commit.CommittedMetadata, error) {
	var zero commit.CommittedMetadata
	o, err := s.metadataOp(ctx, s.db, id)
	if err != nil {
		return zero, err
	}
	t := o.Prepared.Target
	stage, err := s.store.GetOperation(ctx, s.db, id)
	if err != nil {
		return zero, missing(err)
	}
	if stage.Stage == commands.StageCommitted {
		lctx, held, err := s.gate.Coordinator().Acquire(ctx, commands.Request{Security: commands.ModeShared})
		if err != nil {
			return zero, err
		}
		defer held.Release()
		return s.replayMetadata(lctx, o, who, proof)
	}
	ctx, release, err := s.write(ctx, t.ProjectID)
	if err != nil {
		return zero, err
	}
	defer release()
	if who.PrincipalID != o.Command.ActorID {
		return zero, errcode.New(errcode.Forbidden, "only the accepting actor can complete this operation")
	}
	op, err := s.store.GetOperation(ctx, s.db, id)
	if err != nil {
		return zero, missing(err)
	}
	pd, err := metadataProofDigest(proof)
	if err != nil {
		return zero, err
	}
	if op.Stage == commands.StageCommitted {
		return s.replayMetadata(ctx, o, who, proof)
	}
	if op.Stage.Final() || op.Stage == commands.StageQuarantined {
		return zero, conflict()
	}
	reject := func(stage commands.Stage, e error) (commit.CommittedMetadata, error) {
		if err := s.changeStage(ctx, id, stage, e); err != nil {
			return zero, err
		}
		return zero, e
	}
	if err := s.finalCheck(ctx, o.Command, who, o.Request.Action, t.ProjectID, string(t.Kind), t.ID); err != nil {
		return reject(commands.StageBlocked, err)
	}
	if t.Kind == commit.TargetAsset {
		if err := s.CheckAssetWrite(ctx, t.ID); err != nil {
			return reject(commands.StageBlocked, err)
		}
		control, err := s.AssetControl(ctx, t.ID)
		if err != nil {
			return reject(commands.StageBlocked, err)
		}
		expected := o.AssetControlRevision
		if expected == 0 {
			expected = 1
		}
		if control.Revision != expected {
			return reject(commands.StageBlocked, errcode.New(errcode.PreconditionFailed, "prepared asset state changed"))
		}
	}
	if err := s.checkTarget(ctx, s.db, t, who, o.Request.Action); err != nil {
		return reject(commands.StageBlocked, err)
	}
	p := o.Prepared
	if proof.OperationID != id || proof.Target != t || proof.Revision != p.Revision || proof.ContentDigest != p.ContentDigest {
		return reject(commands.StageQuarantined, errcode.New(errcode.OperationNeedsReconciliation, "revision proof does not match prepared revision"))
	}
	_, verifier, _ := s.verifiers()
	if verifier == nil {
		return reject(commands.StageBlocked, errcode.New(errcode.OperationNeedsReconciliation, "no revision verifier configured"))
	}
	if err := verifier.VerifyRevision(ctx, proof); err != nil {
		if code := errcode.CodeOf(err); code == errcode.HashMismatch || code == errcode.OperationNeedsReconciliation {
			return reject(commands.StageQuarantined, err)
		}
		return zero, err
	}
	result := commit.CommittedMetadata{OperationID: id, Target: t, Revision: p.Revision, ContentDigest: p.ContentDigest, CommittedAt: s.clock.Now(), CommittedBy: o.Command.ActorID}
	err = inTx(ctx, s.db, func(tx *sql.Tx) error {
		var current int64
		var pending ids.ID
		if err := tx.QueryRowContext(ctx, `SELECT revision,pending_operation_id FROM ledger_metadata_targets WHERE kind=? AND target_id=?`, t.Kind, t.ID).Scan(&current, &pending); err != nil {
			return err
		}
		if current != o.Request.ExpectedRevision || pending != id {
			return errcode.New(errcode.PreconditionFailed, "metadata revision changed")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO ledger_metadata_revisions(operation_id,kind,target_id,revision,record) VALUES(?,?,?,?,?)`, id, t.Kind, t.ID, p.Revision, encoded(result)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE ledger_metadata_targets SET revision=?,pending_operation_id='',record=? WHERE kind=? AND target_id=?`, p.Revision, encoded(result), t.Kind, t.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE ledger_metadata_prepared SET proof_digest=? WHERE operation_id=?`, pd, id); err != nil {
			return err
		}
		payload := map[string]any{"target_kind": t.Kind, "project_id": t.ProjectID, "target_id": t.ID, "revision": p.Revision, "content_digest": p.ContentDigest}
		if t.Kind == commit.TargetAsset {
			payload["asset_id"] = t.ID
		}
		ev, err := s.event(o.Command, EvMetadataCommitted, string(t.Kind), t.ID, p.Revision, payload)
		if err != nil {
			return err
		}
		return s.store.Complete(ctx, tx, id, op.Stage, commands.Result{Status: commands.ReceiptSucceeded, ResponseCode: 200, Summary: result, ResultRefs: []commands.ResultRef{{Kind: string(t.Kind), ID: t.ID, Revision: p.Revision}}, Events: []event.Envelope{ev}})
	})
	if err != nil {
		return zero, err
	}
	return result, nil
}

func metadataProofDigest(proof commit.RevisionProof) (digest.Digest, error) {
	// Reconstructing an immutable file proof may give it a new WrittenAt.
	// Replay equality therefore covers only stable identity and content.
	b, err := canonjson.CanonicalizeValue(struct {
		OperationID   ids.ID
		Target        commit.MetadataTarget
		Revision      int64
		ContentDigest digest.Digest
		FileRef       string
	}{proof.OperationID, proof.Target, proof.Revision, proof.ContentDigest, proof.FileRef})
	if err != nil {
		return "", errcode.Wrap(errcode.SchemaInvalid, "invalid metadata proof", err)
	}
	return digest.Of(b), nil
}

func (s *Service) replayMetadata(ctx context.Context, o metadataOp, who authz.Context, proof commit.RevisionProof) (commit.CommittedMetadata, error) {
	var zero commit.CommittedMetadata
	if who.PrincipalID != o.Command.ActorID {
		return zero, errcode.New(errcode.Forbidden, "only the accepting actor can replay this operation")
	}
	t := o.Prepared.Target
	if err := s.authorize(ctx, who, "catalog.read", t.ProjectID, string(t.Kind), t.ID); err != nil {
		return zero, err
	}
	pd, err := metadataProofDigest(proof)
	if err != nil {
		return zero, err
	}
	// A concurrent completion may have populated the proof while this caller
	// waited for the project lock; refresh that durable completion evidence.
	current, err := s.metadataOp(ctx, s.db, o.Command.OperationID)
	if err != nil {
		return zero, err
	}
	if current.ProofDigest != pd {
		return zero, errcode.New(errcode.OperationNeedsReconciliation, "metadata proof differs from committed proof")
	}
	return readJSON[commit.CommittedMetadata](ctx, s.db, `SELECT record FROM ledger_metadata_revisions WHERE operation_id=?`, o.Command.OperationID)
}
