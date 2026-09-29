package provenance

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/event"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/schema"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/storage"
)

// Validation uses a non-persisted envelope only to exercise the authoritative
// schema; these placeholders never provide an identity or human authorization.
func (s *Service) validateAssertion(in AssertionRequest) error {
	if in.Subject.InstanceID != s.InstanceID {
		return errcode.New(errcode.RefMismatch, "")
	}
	sample := RightsAssertion{AssertionRequest: in, ID: in.Subject.VersionID, ActorID: in.Subject.VersionID, OperationID: in.Subject.VersionID, CreatedAt: clock.Format(s.Clock.Now())}
	if in.Kind == "correct" || in.Kind == "release" {
		sample.HumanGrantID = in.Subject.VersionID
	}
	raw, err := json.Marshal(sample)
	if err != nil {
		return err
	}
	reg, err := schema.Default()
	if err != nil {
		return err
	}
	if err = reg.ValidateJSON(AssertionContract, raw); err != nil {
		return errcode.Wrap(errcode.SchemaInvalid, "invalid rights assertion", err)
	}
	if strings.TrimSpace(in.Reason) == "" {
		return errcode.New(errcode.SchemaInvalid, "assertion reason required")
	}
	return nil
}
func (s *Service) assertionAccess(ctx context.Context, who authz.Context, v commit.Committed) error {
	ok, err := s.allowed(ctx, who, "catalog.read", v)
	if err != nil {
		return err
	}
	if !ok {
		return errcode.New(errcode.NotFound, "")
	}
	if c, ok := s.Reader.(commit.Controls); ok {
		if err = c.CheckEvidenceAppend(ctx, v.AssetID, v.VersionID); err != nil {
			return err
		}
	}
	d, err := s.EvaluateRiskAccess(ctx, who, v.Ref(s.InstanceID))
	if err != nil {
		return err
	}
	return d.Err()
}
func (s *Service) checkAssertion(ctx context.Context, who authz.Context, in AssertionRequest, human bool) (commit.Committed, int64, error) {
	v, doc, err := s.read(ctx, in.Subject)
	if err != nil {
		return v, 0, err
	}
	if v.ManifestDigest != in.ManifestDigest {
		return v, 0, errcode.New(errcode.RefMismatch, "")
	}
	if err = s.assertionAccess(ctx, who, v); err != nil {
		return v, 0, err
	}
	state, err := s.assertionState(ctx, v, doc)
	if err != nil {
		return v, 0, err
	}
	if state.Revision != in.ExpectedRevision {
		return v, 0, errcode.New(errcode.PreconditionFailed, "current assertion revision changed")
	}
	if in.ReplacesAssertionID != "" && in.ReplacesAssertionID != state.LastID {
		return v, 0, errcode.New(errcode.PreconditionFailed, "assertion replacement is no longer current")
	}
	if !human {
		action := authz.Action("provenance.restrict")
		if in.Kind == "evidence" {
			action = "provenance.assert_evidence"
		} else if in.Kind != "restrict" {
			return v, 0, errcode.New(errcode.HumanProofRequired, "")
		}
		d, err := s.Authz.Authorize(ctx, who, action, authz.Resource{ProjectID: v.ProjectID, Kind: "version", ID: v.VersionID})
		if err != nil {
			return v, 0, err
		}
		if err = d.Err(); err != nil {
			return v, 0, err
		}
		if !monotoneAssertion(state, in.Fields) {
			return v, 0, errcode.New(errcode.HumanProofRequired, "loosening requires a human owner and new evidence")
		}
		if in.Kind == "evidence" {
			raw, _ := json.Marshal(in.Fields)
			if string(raw) != "{}" {
				return v, 0, errcode.New(errcode.SchemaInvalid, "evidence assertions cannot change restrictions")
			}
		}
	} else if in.Kind != "release" && in.Kind != "correct" {
		return v, 0, errcode.New(errcode.HumanGrantMismatch, "")
	}
	// Evidence must have been accepted for this exact version, not merely written
	// to a records directory. Every lowering needs evidence newer than the last
	// active assertion's evidence watermark.
	newEvidence := false
	for _, id := range in.EvidenceIDs {
		revision, err := s.assertionEvidence(ctx, v, id)
		if err != nil {
			return v, 0, err
		}
		newEvidence = newEvidence || revision > state.EvidenceRevision
	}
	if human && !newEvidence {
		return v, 0, errcode.New(errcode.ChecksNotSatisfied, "a new accepted evidence record is required")
	}
	if in.Fields.ConfirmedEvidenceIDs != nil {
		for _, id := range *in.Fields.ConfirmedEvidenceIDs {
			if _, err = s.assertionEvidence(ctx, v, id); err != nil {
				return v, 0, err
			}
		}
	}
	if in.Fields.Uses != nil {
		if err = s.checkCorrectedUses(ctx, who, in.Subject, *in.Fields.Uses); err != nil {
			return v, 0, err
		}
	}
	revision, err := s.CurrentRevision(ctx, v.VersionID)
	return v, revision, err
}
func (s *Service) assertionEvidence(ctx context.Context, v commit.Committed, id ids.ID) (int64, error) {
	var version, op ids.ID
	var revision int64
	var hash digest.Digest
	err := s.DB.QueryRowContext(ctx, `SELECT version_id,operation_id,revision,digest FROM provenance_records WHERE record_id=?`, id).Scan(&version, &op, &revision, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, errcode.New(errcode.NotFound, "")
	}
	if err != nil {
		return 0, err
	}
	if version != v.VersionID {
		return 0, errcode.New(errcode.RefMismatch, "assertion evidence belongs to another target")
	}
	raw, err := s.Files.ReadRecord(ctx, v.ProjectID, v.AssetID, v.VersionID, id)
	if err != nil {
		return 0, err
	}
	if digest.Of(raw) != hash {
		return 0, errcode.New(errcode.HashMismatch, "")
	}
	var record storage.Record
	if err = json.Unmarshal(raw, &record); err != nil {
		return 0, err
	}
	if record.RecordID != id || record.OperationID != op || record.VersionID != version || record.AssetID != v.AssetID || record.ProjectID != v.ProjectID || record.ManifestDigest != v.ManifestDigest || record.PayloadSchema != EvidenceContract || !slices.Contains([]string{"rights_evidence", "correction"}, record.Kind) {
		return 0, errcode.New(errcode.RefMismatch, "")
	}
	reg, err := schema.Default()
	if err != nil {
		return 0, err
	}
	if err = reg.ValidateJSON(EvidenceContract, record.Payload); err != nil {
		return 0, err
	}
	return revision, nil
}
func (s *Service) checkCorrectedUses(ctx context.Context, who authz.Context, subject ids.PermanentRef, uses []manifest.Use) error {
	seen := map[ids.PermanentRef]bool{}
	for _, u := range uses {
		ref := ids.PermanentRef{InstanceID: u.InstanceID, AssetID: u.AssetID, VersionID: u.VersionID}
		if seen[ref] {
			return errcode.New(errcode.SchemaInvalid, "duplicate corrected source")
		}
		seen[ref] = true
		d, err := s.EvaluateRiskAccess(ctx, who, ref)
		if err != nil {
			return err
		}
		if err = d.Err(); err != nil {
			return err
		}
	}
	active := map[ids.PermanentRef]bool{}
	visited := map[ids.PermanentRef]bool{}
	var visit func(ids.PermanentRef, int) error
	visit = func(ref ids.PermanentRef, depth int) error {
		if ref == subject || active[ref] {
			return errcode.New(errcode.DependencyCycle, "")
		}
		if visited[ref] {
			return nil
		}
		if depth > s.MaxDepth || len(active)+len(visited) >= s.MaxNodes {
			return errcode.New(errcode.RightsPending, "corrected graph exceeds verification budget")
		}
		active[ref] = true
		defer delete(active, ref)
		v, doc, err := s.read(ctx, ref)
		if err != nil {
			return err
		}
		state, err := s.assertionState(ctx, v, doc)
		if err != nil {
			return err
		}
		for _, use := range state.Uses {
			if err = visit(ids.PermanentRef{InstanceID: use.InstanceID, AssetID: use.AssetID, VersionID: use.VersionID}, depth+1); err != nil {
				return err
			}
		}
		visited[ref] = true
		return nil
	}
	for ref := range seen {
		if err := visit(ref, 0); err != nil {
			return err
		}
	}
	return nil
}

type preparedAssertion struct {
	Command commands.Context
	Request AssertionRequest
	Record  storage.Record
}

func (s *Service) preparedAssertion(ctx context.Context, op ids.ID) (preparedAssertion, error) {
	var out preparedAssertion
	var cmd, in, record string
	err := s.DB.QueryRowContext(ctx, `SELECT command,request,record FROM provenance_assertion_prepared WHERE operation_id=?`, op).Scan(&cmd, &in, &record)
	if errors.Is(err, sql.ErrNoRows) {
		return out, errcode.New(errcode.NotFound, "")
	}
	if err != nil {
		return out, err
	}
	if err = json.Unmarshal([]byte(cmd), &out.Command); err != nil {
		return out, err
	}
	if err = json.Unmarshal([]byte(in), &out.Request); err != nil {
		return out, err
	}
	err = json.Unmarshal([]byte(record), &out.Record)
	return out, err
}
func assertionJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}
func (s *Service) prepareAssertion(ctx context.Context, who authz.Context, cmd commands.Context, in AssertionRequest, human bool) (commands.Receipt, error) {
	if err := s.validateAssertion(in); err != nil {
		return commands.Receipt{}, err
	}
	v, _, err := s.checkAssertion(ctx, who, in, human)
	if err != nil {
		return commands.Receipt{}, err
	}
	result, err := s.store.Accept(ctx, s.DB, cmd, commands.StagePrepared, []string{string(v.VersionID)}, func(ctx context.Context, tx *sql.Tx) error {
		id, err := s.IDs.New()
		if err != nil {
			return err
		}
		a := RightsAssertion{AssertionRequest: in, ID: id, ActorID: who.PrincipalID, HumanGrantID: cmd.HumanGrantID, OperationID: cmd.OperationID, CreatedAt: clock.Format(s.Clock.Now())}
		raw, err := canonjson.CanonicalizeValue(a)
		if err != nil {
			return err
		}
		record := storage.Record{RecordID: id, ProjectID: v.ProjectID, AssetID: v.AssetID, VersionID: v.VersionID, ManifestDigest: v.ManifestDigest, Kind: "rights_assertion", PayloadSchema: AssertionContract, Payload: raw, AuthorID: who.PrincipalID, SessionID: who.SessionID, OperationID: cmd.OperationID, CreatedAt: a.CreatedAt, Supersedes: in.ReplacesAssertionID}
		_, err = tx.ExecContext(ctx, `INSERT INTO provenance_assertion_prepared VALUES(?,?,?,?,?)`, cmd.OperationID, assertionJSON(cmd), assertionJSON(in), assertionJSON(record), in.ExpectedRevision)
		return err
	})
	if err != nil {
		return commands.Receipt{}, err
	}
	return *result.Receipt, nil
}
func (s *Service) finishAssertion(ctx context.Context, who authz.Context, cmd commands.Context, in AssertionRequest, human bool) (commands.Receipt, error) {
	prior, err := s.store.ReceiptByOperation(ctx, s.DB, cmd.OperationID)
	if err != nil {
		return commands.Receipt{}, err
	}
	if prior.RequestHash != cmd.RequestHash || prior.Key != cmd.Key() {
		return commands.Receipt{}, errcode.New(errcode.IdempotencyConflict, "")
	}
	if prior.Status == commands.ReceiptFailed {
		return commands.Receipt{}, errcode.New(prior.FailureCode, "assertion operation is terminal")
	}
	if prior.Status == commands.ReceiptSucceeded {
		v, err := s.Reader.Version(ctx, in.Subject.AssetID, in.Subject.VersionID)
		if err != nil {
			return commands.Receipt{}, err
		}
		if err = s.assertionAccess(ctx, who, v); err != nil {
			return commands.Receipt{}, err
		}
		if !human {
			d, err := s.Authz.Authorize(ctx, who, authz.Action(cmd.CommandType), authz.Resource{ProjectID: v.ProjectID, Kind: "version", ID: v.VersionID})
			if err != nil {
				return commands.Receipt{}, err
			}
			if err = d.Err(); err != nil {
				return commands.Receipt{}, err
			}
		}
		return *prior, nil
	}
	prepared, err := s.preparedAssertion(ctx, cmd.OperationID)
	if err != nil {
		return commands.Receipt{}, err
	}
	if prepared.Command.RequestHash != cmd.RequestHash || prepared.Command.ActorID != who.PrincipalID || prepared.Command.SessionID != who.SessionID || prepared.Command.RecoveryEpoch != who.RecoveryEpoch {
		return commands.Receipt{}, errcode.New(errcode.OperationNeedsReconciliation, "original assertion authority changed")
	}
	expected, err := canonjson.CanonicalizeValue(in)
	if err != nil {
		return commands.Receipt{}, err
	}
	saved, err := canonjson.CanonicalizeValue(prepared.Request)
	if err != nil {
		return commands.Receipt{}, err
	}
	if string(expected) != string(saved) {
		return commands.Receipt{}, errcode.New(errcode.IdempotencyConflict, "")
	}
	v, evidenceRevision, err := s.checkAssertion(ctx, who, in, human)
	if err != nil {
		return commands.Receipt{}, err
	}
	record := prepared.Record
	raw, err := s.Files.ReadRecord(ctx, v.ProjectID, v.AssetID, v.VersionID, record.RecordID)
	if err != nil {
		return commands.Receipt{}, err
	}
	// Compare the entire immutable envelope, including its generated stable IDs.
	var installed storage.Record
	if err = json.Unmarshal(raw, &installed); err != nil {
		return commands.Receipt{}, err
	}
	a, err := canonjson.CanonicalizeValue(installed)
	if err != nil {
		return commands.Receipt{}, err
	}
	b, err := canonjson.CanonicalizeValue(record)
	if err != nil {
		return commands.Receipt{}, err
	}
	if string(a) != string(b) {
		return commands.Receipt{}, errcode.New(errcode.HashMismatch, "prepared assertion record changed")
	}
	hash := digest.Of(raw)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return commands.Receipt{}, err
	}
	defer tx.Rollback()
	prior, err = s.store.ReceiptByOperation(ctx, tx, cmd.OperationID)
	if err != nil {
		return commands.Receipt{}, err
	}
	if prior.Status == commands.ReceiptSucceeded {
		return *prior, nil
	}
	revision, _, err := s.assertionRevision(ctx, tx, v.VersionID)
	if err != nil {
		return commands.Receipt{}, err
	}
	if revision != in.ExpectedRevision {
		return commands.Receipt{}, errcode.New(errcode.PreconditionFailed, "")
	}
	next := revision + 1
	if _, err = tx.ExecContext(ctx, `INSERT INTO provenance_assertions VALUES(?,?,?,?,?,?)`, record.RecordID, v.VersionID, next, hash, cmd.OperationID, evidenceRevision); err != nil {
		return commands.Receipt{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO provenance_assertion_targets VALUES(?,?,?) ON CONFLICT(version_id) DO UPDATE SET revision=excluded.revision,assertion_id=excluded.assertion_id`, v.VersionID, next, record.RecordID); err != nil {
		return commands.Receipt{}, err
	}
	epoch, err := bumpRightsEpoch(ctx, tx)
	if err != nil {
		return commands.Receipt{}, err
	}
	result := AssertionReceipt{RecordRef: storage.RecordRef{RecordID: record.RecordID, Digest: hash}, Revision: next, RightsEpoch: epoch, OperationID: cmd.OperationID}
	ev, err := event.New(s.IDs, s.Clock, event.Params{EventType: "rights.asserted", SchemaVersion: 1, AggregateType: "version_assertions", AggregateID: v.VersionID, AggregateRevision: next, ActorID: who.PrincipalID, SessionID: who.SessionID, ProjectID: v.ProjectID, OperationID: cmd.OperationID, Payload: map[string]any{"asset_id": v.AssetID, "version_id": v.VersionID, "assertion_id": record.RecordID, "rights_epoch": epoch, "kind": in.Kind}})
	if err != nil {
		return commands.Receipt{}, err
	}
	if err = s.store.Complete(ctx, tx, cmd.OperationID, commands.StagePrepared, commands.Result{Status: commands.ReceiptSucceeded, ResponseCode: 201, Summary: result, Events: []event.Envelope{ev}}); err != nil {
		return commands.Receipt{}, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM provenance_assertion_prepared WHERE operation_id=?`, cmd.OperationID); err != nil {
		return commands.Receipt{}, err
	}
	if err = tx.Commit(); err != nil {
		return commands.Receipt{}, err
	}
	receipt, err := s.store.ReceiptByOperation(ctx, s.DB, cmd.OperationID)
	if err != nil {
		return commands.Receipt{}, err
	}
	return *receipt, nil
}

// ApplyAssertion activates only monotone restrictions or evidence indexes.
// Correct/release is deliberately absent from this ordinary permission path.
func (s *Service) ApplyAssertion(ctx context.Context, who authz.Context, key string, in AssertionRequest) (AssertionReceipt, error) {
	var out AssertionReceipt
	if err := s.validateAssertion(in); err != nil {
		return out, err
	}
	action := "provenance.restrict"
	if in.Kind == "evidence" {
		action = "provenance.assert_evidence"
	} else if in.Kind != "restrict" {
		return out, errcode.New(errcode.HumanProofRequired, "")
	}
	v, err := s.Reader.Version(ctx, in.Subject.AssetID, in.Subject.VersionID)
	if err != nil {
		return out, err
	}
	raw, err := canonjson.CanonicalizeValue(in)
	if err != nil {
		return out, err
	}
	hash, err := commands.RequestHash(commands.HashInput{CommandType: action, ProjectID: v.ProjectID, Body: raw})
	if err != nil {
		return out, err
	}
	op, err := s.IDs.New()
	if err != nil {
		return out, err
	}
	cmd := commands.Context{OperationID: op, IdempotencyKey: key, RequestHash: hash, CommandType: action, ActorID: who.PrincipalID, SessionID: who.SessionID, ProjectID: v.ProjectID, RecoveryEpoch: who.RecoveryEpoch, PolicyRevision: who.PolicyRevision}
	if err = cmd.Validate(); err != nil {
		return out, err
	}
	var receipt commands.Receipt
	var prepared preparedAssertion
	err = func() error {
		ctx, h, err := s.Gate.Acquire(ctx, commands.Request{Security: commands.ModeExclusive, Projects: []string{string(v.ProjectID)}, Assets: []string{string(v.AssetID)}})
		if err != nil {
			return err
		}
		defer h.Release()
		if err = s.assertionAccess(ctx, who, v); err != nil {
			return err
		}
		d, err := s.Authz.Authorize(ctx, who, authz.Action(action), authz.Resource{ProjectID: v.ProjectID, Kind: "version", ID: v.VersionID})
		if err != nil {
			return err
		}
		if err = d.Err(); err != nil {
			return err
		}
		prior, err := s.store.LookupReceipt(ctx, s.DB, cmd.Key())
		if err != nil {
			return err
		}
		if prior != nil {
			switch commands.Decide(prior, hash, s.Clock.Now()) {
			case commands.OutcomeConflict:
				return errcode.New(errcode.IdempotencyConflict, "")
			case commands.OutcomeExpired:
				return errcode.New(errcode.IdempotencyResultExpired, "")
			}
			receipt = *prior
			if receipt.Status == commands.ReceiptFailed {
				return errcode.New(receipt.FailureCode, "assertion operation is terminal")
			}
			if receipt.Status != commands.ReceiptSucceeded {
				prepared, err = s.preparedAssertion(ctx, receipt.OperationID)
			}
			return err
		}
		receipt, err = s.prepareAssertion(ctx, who, cmd, in, false)
		if err == nil {
			prepared, err = s.preparedAssertion(ctx, receipt.OperationID)
		}
		return err
	}()
	if err != nil {
		return out, err
	}
	if receipt.Status == commands.ReceiptSucceeded {
		err = json.Unmarshal(receipt.ResponseSummary, &out)
		return out, err
	}
	if prepared.Command.RecoveryEpoch != who.RecoveryEpoch || prepared.Command.SessionID != who.SessionID {
		return out, errcode.New(errcode.OperationNeedsReconciliation, "old assertion intent requires reconciliation")
	}
	if _, err = s.Files.AppendRecord(ctx, prepared.Record); err != nil {
		return out, err
	}
	err = func() error {
		ctx, h, err := s.Gate.Acquire(ctx, commands.Request{Security: commands.ModeExclusive, Projects: []string{string(v.ProjectID)}, Assets: []string{string(v.AssetID)}})
		if err != nil {
			return err
		}
		defer h.Release()
		receipt, err = s.finishAssertion(ctx, who, prepared.Command, in, false)
		return err
	}()
	if err != nil {
		return out, err
	}
	err = json.Unmarshal(receipt.ResponseSummary, &out)
	return out, err
}

type AssertionHumanCommands interface {
	AcceptHumanItem(context.Context, authz.Context, ids.ID, ids.ID, identity.HumanAction, commands.Request, identity.HumanDomain) (commands.Receipt, error)
}

func (s *Service) AssertionHumanAction(ctx context.Context, in AssertionRequest) (identity.HumanAction, error) {
	var out identity.HumanAction
	if in.Kind != "correct" && in.Kind != "release" {
		return out, errcode.New(errcode.SchemaInvalid, "human correction/release required")
	}
	if err := s.validateAssertion(in); err != nil {
		return out, err
	}
	v, err := s.Reader.Version(ctx, in.Subject.AssetID, in.Subject.VersionID)
	if err != nil {
		return out, err
	}
	if v.ManifestDigest != in.ManifestDigest {
		return out, errcode.New(errcode.RefMismatch, "")
	}
	raw, err := canonjson.CanonicalizeValue(in)
	if err != nil {
		return out, err
	}
	return identity.HumanAction{Action: identity.ActReleaseRestriction, ProjectID: v.ProjectID, ResourceID: v.VersionID, ResourceRevision: in.ExpectedRevision, ManifestDigest: v.ManifestDigest, Request: raw}, nil
}
func (s *Service) ValidateHumanTarget(ctx context.Context, who authz.Context, a identity.HumanAction) error {
	if a.Action != identity.ActReleaseRestriction {
		return errcode.New(errcode.SchemaInvalid, "unsupported provenance human action")
	}
	var in AssertionRequest
	if err := json.Unmarshal(a.Request, &in); err != nil {
		return err
	}
	expected, err := s.AssertionHumanAction(ctx, in)
	if err != nil {
		return err
	}
	left, err := canonjson.CanonicalizeValue(a)
	if err != nil {
		return err
	}
	right, err := canonjson.CanonicalizeValue(expected)
	if err != nil {
		return err
	}
	if string(left) != string(right) {
		return errcode.New(errcode.HumanGrantMismatch, "")
	}
	v, err := s.Reader.Version(ctx, in.Subject.AssetID, in.Subject.VersionID)
	if err != nil {
		return err
	}
	return s.assertionAccess(ctx, who, v)
}
func (s *Service) ApplyHumanAssertion(ctx context.Context, who authz.Context, in AssertionRequest, grant, child ids.ID, human AssertionHumanCommands) (AssertionReceipt, error) {
	var out AssertionReceipt
	if human == nil {
		return out, errcode.New(errcode.HumanProofRequired, "")
	}
	action, err := s.AssertionHumanAction(ctx, in)
	if err != nil {
		return out, err
	}
	domain := &humanAssertion{s: s, who: who, in: in}
	locks := commands.Request{Security: commands.ModeExclusive, Projects: []string{string(action.ProjectID)}, Assets: []string{string(in.Subject.AssetID)}}
	first, err := human.AcceptHumanItem(ctx, who, grant, child, action, locks, domain)
	if err != nil {
		return out, err
	}
	if first.Status == commands.ReceiptFailed {
		return out, errcode.New(first.FailureCode, "assertion operation is terminal")
	}
	if first.Status == commands.ReceiptSucceeded {
		err = json.Unmarshal(first.ResponseSummary, &out)
		return out, err
	}
	if _, err = s.Files.AppendRecord(ctx, domain.prepared.Record); err != nil {
		return out, err
	}
	// Authorization, expiry, identity/epoch and complete request are checked again
	// after file IO. An expired grant leaves inert bytes, never a lowered policy.
	domain.ready = true
	receipt, err := human.AcceptHumanItem(ctx, who, grant, child, action, locks, domain)
	if err != nil {
		return out, err
	}
	err = json.Unmarshal(receipt.ResponseSummary, &out)
	return out, err
}

type humanAssertion struct {
	s        *Service
	who      authz.Context
	in       AssertionRequest
	ready    bool
	prepared preparedAssertion
}

func (h *humanAssertion) Receipt(ctx context.Context, op ids.ID) (commands.Receipt, error) {
	v, err := h.s.Reader.Version(ctx, h.in.Subject.AssetID, h.in.Subject.VersionID)
	if err != nil {
		return commands.Receipt{}, err
	}
	if err = h.s.assertionAccess(ctx, h.who, v); err != nil {
		return commands.Receipt{}, err
	}
	r, err := h.s.store.ReceiptByOperation(ctx, h.s.DB, op)
	if err != nil {
		return commands.Receipt{}, err
	}
	return *r, nil
}
func (h *humanAssertion) Commit(ctx context.Context, cmd commands.Context, _ identity.HumanAction) (commands.Receipt, error) {
	if h.ready {
		return h.s.finishAssertion(ctx, h.who, cmd, h.in, true)
	}
	r, err := h.s.prepareAssertion(ctx, h.who, cmd, h.in, true)
	if err == nil && r.Status != commands.ReceiptSucceeded {
		h.prepared, err = h.s.preparedAssertion(ctx, cmd.OperationID)
	}
	return r, err
}
