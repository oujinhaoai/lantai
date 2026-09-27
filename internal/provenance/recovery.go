package provenance

import (
	"context"
	"encoding/json"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/storage"
)

type RecoveryOperation struct {
	Operation        commands.Operation
	Key              commands.ReceiptKey
	Record           storage.Record
	ExpectedRevision int64
}
type RecoveryInventory struct {
	Open     []RecoveryOperation
	Records  []storage.RecordEntry
	Findings []storage.Finding
}

func (s *Service) recoveryOperation(ctx context.Context, id ids.ID) (RecoveryOperation, error) {
	var out RecoveryOperation
	op, err := s.store.GetOperation(ctx, s.DB, id)
	if err != nil {
		return out, err
	}
	if op.OwnerModule != "provenance" {
		return out, errcode.New(errcode.NotFound, "")
	}
	r, err := s.store.ReceiptByOperation(ctx, s.DB, id)
	if err != nil {
		return out, err
	}
	out.Operation, out.Key = *op, r.Key
	var raw string
	if err = s.DB.QueryRowContext(ctx, `SELECT expected_revision,record_json FROM provenance_prepared WHERE operation_id=?`, id).Scan(&out.ExpectedRevision, &raw); err != nil {
		return out, err
	}
	err = json.Unmarshal([]byte(raw), &out.Record)
	return out, err
}

// RecoveryInventory reports durable pending intents and accepted evidence only.
// Installed but unaccepted records remain storage residuals and never affect rights.
func (s *Service) RecoveryInventory(ctx context.Context) (RecoveryInventory, error) {
	var out RecoveryInventory
	all, err := s.store.OpenOperations(ctx, s.DB)
	if err != nil {
		return out, err
	}
	for _, id := range all {
		r, err := s.recoveryOperation(ctx, id)
		if err != nil {
			return out, err
		}
		out.Open = append(out.Open, r)
	}
	versions := map[ids.ID]commit.Committed{}
	for after := ids.ID(""); ; {
		batch, err := s.Reader.Versions(ctx, after, 1000)
		if err != nil {
			return out, err
		}
		for _, v := range batch {
			versions[v.VersionID] = v
		}
		if len(batch) < 1000 {
			break
		}
		after = batch[len(batch)-1].VersionID
	}
	type accepted struct {
		id, version, op ids.ID
		digest          digest.Digest
		revision        int64
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT record_id,version_id,operation_id,digest,revision FROM provenance_records ORDER BY version_id,revision`)
	if err != nil {
		return out, err
	}
	var records []accepted
	for rows.Next() {
		var r accepted
		if err = rows.Scan(&r.id, &r.version, &r.op, &r.digest, &r.revision); err != nil {
			rows.Close()
			return out, err
		}
		records = append(records, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	counts := map[ids.ID]int64{}
	for _, r := range records {
		v, ok := versions[r.version]
		entry := storage.RecordEntry{RecordID: r.id, ProjectID: v.ProjectID, AssetID: v.AssetID, VersionID: r.version, OperationID: r.op, Digest: r.digest}
		out.Records = append(out.Records, entry)
		counts[r.version]++
		reason := ""
		if !ok {
			reason = "evidence_target_missing"
		} else if counts[r.version] != r.revision {
			reason = "evidence_revision_gap"
		} else if s.Files == nil {
			reason = "evidence_adapter_missing"
		} else {
			b, e := s.Files.ReadRecord(ctx, v.ProjectID, v.AssetID, v.VersionID, r.id)
			var record storage.Record
			if e != nil {
				reason = "evidence_file_missing"
			} else if digest.Of(b) != r.digest {
				reason = "evidence_digest"
			} else if json.Unmarshal(b, &record) != nil || record.RecordID != r.id || record.VersionID != r.version || record.AssetID != v.AssetID || record.ProjectID != v.ProjectID || record.ManifestDigest != v.ManifestDigest || record.OperationID != r.op {
				reason = "evidence_identity"
			}
		}
		if reason != "" {
			out.Findings = append(out.Findings, storage.Finding{Code: errcode.OperationNeedsReconciliation, Reason: reason, OperationID: r.op})
		}
	}
	rows, err = s.DB.QueryContext(ctx, `SELECT version_id,revision FROM provenance_targets ORDER BY version_id`)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	seenTargets := map[ids.ID]bool{}
	for rows.Next() {
		var id ids.ID
		var revision int64
		if err = rows.Scan(&id, &revision); err != nil {
			return out, err
		}
		seenTargets[id] = true
		if counts[id] != revision {
			out.Findings = append(out.Findings, storage.Finding{Code: errcode.OperationNeedsReconciliation, Reason: "evidence_current_revision_mismatch"})
		}
	}
	for id := range counts {
		if !seenTargets[id] {
			out.Findings = append(out.Findings, storage.Finding{Code: errcode.OperationNeedsReconciliation, Reason: "evidence_current_revision_missing"})
		}
	}
	return out, rows.Err()
}

// RecoverOperation retries the same frozen record through normal acceptance.
// The dispatcher supplies the still-valid original authenticated session. A new
// epoch, different actor/session or missing current authority is never substituted.
func (s *Service) RecoverOperation(ctx context.Context, who authz.Context, id ids.ID) (AcceptedRecord, error) {
	if err := s.Gate.RequireMaintenance(ctx); err != nil {
		return AcceptedRecord{}, err
	}
	r, err := s.recoveryOperation(ctx, id)
	if err != nil {
		return AcceptedRecord{}, err
	}
	if r.Operation.Stage != commands.StagePrepared {
		return AcceptedRecord{}, errcode.New(errcode.OperationNeedsReconciliation, "evidence intent cannot be automatically resumed")
	}
	if who.PrincipalID != r.Record.AuthorID || who.PrincipalID != r.Key.ActorID || who.SessionID != r.Record.SessionID || who.RecoveryEpoch != r.Operation.RecoveryEpoch {
		return AcceptedRecord{}, errcode.New(errcode.OperationNeedsReconciliation, "original evidence authority is no longer available")
	}
	var e Evidence
	if err = json.Unmarshal(r.Record.Payload, &e); err != nil {
		return AcceptedRecord{}, err
	}
	return s.AppendEvidence(ctx, AppendRequest{Who: who, IdempotencyKey: r.Key.IdempotencyKey, Ref: ids.PermanentRef{InstanceID: s.InstanceID, AssetID: r.Record.AssetID, VersionID: r.Record.VersionID}, ManifestDigest: r.Record.ManifestDigest, ExpectedRevision: r.ExpectedRevision, Evidence: e, Supersedes: r.Record.Supersedes, Producer: r.Record.Producer})
}
