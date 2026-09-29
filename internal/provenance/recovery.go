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
	Assertion        *AssertionRequest
}
type RecoveryInventory struct {
	Open      []RecoveryOperation
	Records   []storage.RecordEntry
	Findings  []storage.Finding
	Residuals []storage.Finding
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
	if op.CommandType != string(ActionAppendEvidence) {
		prepared, err := s.preparedAssertion(ctx, id)
		if err != nil {
			return out, err
		}
		if prepared.Command.OperationID != id || prepared.Command.Key() != r.Key || prepared.Command.RequestHash != r.RequestHash || prepared.Command.RecoveryEpoch != op.RecoveryEpoch {
			return out, errcode.New(errcode.OperationNeedsReconciliation, "assertion intent differs from operation")
		}
		out.Record, out.ExpectedRevision, out.Assertion = prepared.Record, prepared.Request.ExpectedRevision, &prepared.Request
		return out, nil
	}
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
		assertion       bool
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
	rows, err = s.DB.QueryContext(ctx, `SELECT assertion_id,version_id,operation_id,digest,revision FROM provenance_assertions ORDER BY version_id,revision`)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		r := accepted{assertion: true}
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
	assertionCounts := map[ids.ID]int64{}
	assertionLast := map[ids.ID]ids.ID{}
	counts := map[ids.ID]int64{}
	for _, r := range records {
		v, ok := versions[r.version]
		entry := storage.RecordEntry{RecordID: r.id, ProjectID: v.ProjectID, AssetID: v.AssetID, VersionID: r.version, OperationID: r.op, Digest: r.digest}
		out.Records = append(out.Records, entry)
		expected := counts[r.version] + 1
		if r.assertion {
			assertionCounts[r.version]++
			expected = assertionCounts[r.version] + 1
			assertionLast[r.version] = r.id
		} else {
			counts[r.version]++
		}
		reason := ""
		if !ok {
			reason = "evidence_target_missing"
		} else if expected != r.revision {
			reason = "evidence_revision_gap"
		} else if s.Files == nil {
			reason = "evidence_adapter_missing"
		} else {
			if source, ok := s.Reader.(commit.FileLocations); ok {
				location, e := source.VersionFileLocation(ctx, v.AssetID, v.VersionID)
				if e != nil {
					return out, e
				}
				if location.Purged {
					continue
				} // Evidence receipt/history remains authoritative.
				if location.PendingOperationID != "" {
					out.Residuals = append(out.Residuals, storage.Finding{Code: errcode.OperationNeedsReconciliation, Reason: "evidence_lifecycle_pending", OperationID: r.op})
					continue
				}
			}
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
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	rows, err = s.DB.QueryContext(ctx, `SELECT version_id,revision,assertion_id FROM provenance_assertion_targets ORDER BY version_id`)
	if err != nil {
		return out, err
	}
	seen := map[ids.ID]bool{}
	for rows.Next() {
		var version, last ids.ID
		var revision int64
		if err = rows.Scan(&version, &revision, &last); err != nil {
			rows.Close()
			return out, err
		}
		seen[version] = true
		if assertionCounts[version]+1 != revision || assertionLast[version] != last {
			out.Findings = append(out.Findings, storage.Finding{Code: errcode.OperationNeedsReconciliation, Reason: "assertion_current_revision_mismatch"})
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	for version := range assertionCounts {
		if !seen[version] {
			out.Findings = append(out.Findings, storage.Finding{Code: errcode.OperationNeedsReconciliation, Reason: "assertion_current_revision_missing"})
		}
		v, ok := versions[version]
		if !ok {
			continue
		}
		if locations, ok := s.Reader.(commit.FileLocations); ok {
			location, err := locations.VersionFileLocation(ctx, v.AssetID, v.VersionID)
			if err != nil {
				return out, err
			}
			if location.Purged || location.PendingOperationID != "" {
				continue
			}
		}
		_, doc, err := s.read(ctx, v.Ref(s.InstanceID))
		if err == nil {
			_, err = s.assertionState(ctx, v, doc)
		}
		if err != nil {
			out.Findings = append(out.Findings, storage.Finding{Code: errcode.OperationNeedsReconciliation, Reason: "assertion_history_invalid"})
		}
	}
	return out, nil
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
	if r.Assertion != nil {
		if r.Assertion.Kind == "correct" || r.Assertion.Kind == "release" {
			return AcceptedRecord{}, errcode.New(errcode.HumanProofRequired, "human assertion requires current exact grant acceptance")
		}
		result, err := s.ApplyAssertion(ctx, who, r.Key.IdempotencyKey, *r.Assertion)
		return AcceptedRecord(result), err
	}
	var e Evidence
	if err = json.Unmarshal(r.Record.Payload, &e); err != nil {
		return AcceptedRecord{}, err
	}
	return s.AppendEvidence(ctx, AppendRequest{Who: who, IdempotencyKey: r.Key.IdempotencyKey, Ref: ids.PermanentRef{InstanceID: s.InstanceID, AssetID: r.Record.AssetID, VersionID: r.Record.VersionID}, ManifestDigest: r.Record.ManifestDigest, ExpectedRevision: r.ExpectedRevision, Evidence: e, Supersedes: r.Record.Supersedes, Producer: r.Record.Producer})
}
