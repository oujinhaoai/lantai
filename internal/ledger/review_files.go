package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
	"github.com/oujinhaoai/lantai/internal/contract/rights"
	"github.com/oujinhaoai/lantai/internal/contract/schema"
	"github.com/oujinhaoai/lantai/internal/extensions"
	"github.com/oujinhaoai/lantai/internal/storage"
)

type ReviewFiles interface {
	ReadManifest(context.Context, commit.Committed) ([]byte, error)
	AppendRecord(context.Context, storage.Record) (storage.RecordRef, error)
	ReadRecord(context.Context, ids.ID, ids.ID, ids.ID, ids.ID) ([]byte, error)
}

// ReviewExecution is the T05/T06 acceptance bridge, not a task implementation.
// VerifyEvidence must resolve the principal, active attempt/fence, exact processor
// activation/job completion and bound result from authority. A caller's claim of
// completion or a matching package digest is insufficient. Historical verifies
// accepted completed evidence without requiring its job still to be running.
type ReviewExecution interface {
	Task(context.Context, authz.Context, commit.Committed, ReviewFlow, string) error
	VerifyEvidence(context.Context, authz.Context, commit.Committed, ids.ID, ReviewEvidenceInput, bool) error
	CheckApplicable(context.Context, authz.Context, commit.Committed, ids.ID, ReviewEvidenceInput, ids.ID) error
}
type ReviewProducerVerifier interface {
	VerifyProducer(context.Context, storage.Producer) error
}
type AdditionalReviewEvidence interface {
	AcceptedEvidenceRecord(context.Context, authz.Context, ids.ID) (storage.Record, digest.Digest, error)
}
type FileReviewSources struct {
	additional AdditionalReviewEvidence
	ledger     *Service
	files      ReviewFiles
	rights     rights.Evaluator
	execution  ReviewExecution
	producers  ReviewProducerVerifier
}

func (s *Service) NewFileReviewSources(files ReviewFiles, rights rights.Evaluator, execution ReviewExecution, producers ReviewProducerVerifier, additional ...AdditionalReviewEvidence) (*FileReviewSources, error) {
	if files == nil || rights == nil || execution == nil || producers == nil {
		return nil, errors.New("ledger: review files, rights, task/execution and producer authorities required")
	}
	if len(additional) > 1 {
		return nil, errors.New("ledger: at most one provenance evidence authority")
	}
	var extra AdditionalReviewEvidence
	if len(additional) == 1 {
		extra = additional[0]
	}
	return &FileReviewSources{ledger: s, files: files, rights: rights, execution: execution, producers: producers, additional: extra}, nil
}
func (s *FileReviewSources) read(ctx context.Context, v commit.Committed) (manifest.Document, error) {
	raw, err := s.files.ReadManifest(ctx, v)
	if err != nil {
		return manifest.Document{}, err
	}
	doc, err := manifest.Parse(raw)
	if err != nil {
		return doc, err
	}
	if doc.AssetID != v.AssetID || doc.VersionID != v.VersionID || doc.ProjectID != v.ProjectID || doc.ManifestDigest != v.ManifestDigest {
		return doc, errcode.New(errcode.RefMismatch, "")
	}
	return doc, nil
}
func (s *FileReviewSources) AssetType(ctx context.Context, v commit.Committed) (manifest.AssetType, error) {
	doc, err := s.read(ctx, v)
	return doc.Content.AssetType, err
}
func (s *FileReviewSources) Task(ctx context.Context, who authz.Context, v commit.Committed, f ReviewFlow, action string) error {
	return s.execution.Task(ctx, who, v, f, action)
}
func (s *FileReviewSources) Use(ctx context.Context, who authz.Context, v commit.Committed, purpose authz.Purpose) error {
	if err := s.ledger.authorize(ctx, who, "catalog.read", v.ProjectID, "version", v.VersionID); err != nil {
		return err
	}
	result, err := s.rights.EvaluateUse(ctx, who, v.Ref(who.InstanceID), purpose)
	if err != nil {
		return err
	}
	return result.Err()
}
func (s *FileReviewSources) Profile(ctx context.Context, who authz.Context, ref ids.PermanentRef) (ProfileSnapshot, error) {
	var out ProfileSnapshot
	if ref.InstanceID != who.InstanceID {
		return out, errcode.New(errcode.RefMismatch, "")
	}
	v, err := s.ledger.Version(ctx, ref.AssetID, ref.VersionID)
	if err != nil {
		return out, err
	}
	if err = s.Use(ctx, who, v, authz.PurposeArchiveReview); err != nil {
		return out, err
	}
	doc, err := s.read(ctx, v)
	if err != nil {
		return out, err
	}
	if doc.Content.AssetType != manifest.TypeConfig {
		return out, invalid("acceptance profile requires a config asset")
	}
	raw, err := json.Marshal(doc.Content.Metadata["acceptance_profile"])
	if err != nil {
		return out, err
	}
	var p AcceptanceProfile
	if err = json.Unmarshal(raw, &p); err != nil {
		return out, err
	}
	if err = validateProfile(p); err != nil {
		return out, err
	}
	return ProfileSnapshot{Ref: ref, ManifestDigest: v.ManifestDigest, Profile: p}, nil
}

type QAReport struct {
	Tool         string `json:"tool"`
	ToolVersion  string `json:"tool_version"`
	Observations string `json:"observations"`
	Verdict      string `json:"verdict"`
}

// ReviewEvidenceInput is the core record wrapper; Check preserves the existing
// extension check-result wire format. A QA report is evidence, never approval.
type ReviewEvidenceInput struct {
	CheckRunID     ids.ID                  `json:"check_run_id,omitempty"`
	ProjectID      ids.ID                  `json:"project_id"`
	Ref            ids.PermanentRef        `json:"ref"`
	ManifestDigest digest.Digest           `json:"manifest_digest"`
	Flow           ReviewFlow              `json:"flow"`
	Kind           string                  `json:"kind"`
	CheckKey       string                  `json:"check_key,omitempty"`
	SchemaVersion  int                     `json:"schema_version,omitempty"`
	ConfigDigest   digest.Digest           `json:"config_digest,omitempty"`
	Check          *extensions.CheckResult `json:"check,omitempty"`
	QA             *QAReport               `json:"qa,omitempty"`
}

func (in ReviewEvidenceInput) validate() error {
	if !in.ProjectID.Valid() || in.Ref.Validate(true) != nil || !in.ManifestDigest.Valid() || !in.Flow.TaskID.Valid() || !in.Flow.AttemptID.Valid() || in.Flow.Round < 1 || in.Flow.Fence < 1 || in.Flow.CandidateGroup != "" && !in.Flow.CandidateGroup.Valid() {
		return invalid("fixed evidence target and task attempt required")
	}
	switch in.Kind {
	case "check_result":
		if in.CheckRunID != "" && !in.CheckRunID.Valid() || in.QA != nil || in.Check == nil || in.SchemaVersion != 1 || strings.TrimSpace(in.CheckKey) == "" || !in.ConfigDigest.Valid() || in.Check.Ref != in.Ref || in.Check.ManifestDigest != in.ManifestDigest || in.Check.CheckKey != "" && in.Check.CheckKey != in.CheckKey {
			return invalid("invalid check evidence binding")
		}
		raw, err := canonjson.CanonicalizeValue(in.Check)
		if err != nil {
			return err
		}
		doc, err := canonjson.Decode(raw)
		if err != nil {
			return err
		}
		reg, err := schema.Default()
		if err != nil {
			return err
		}
		if err = reg.Validate("lantai.check-result/v1", doc); err != nil {
			return errcode.Wrap(errcode.SchemaInvalid, "invalid check result", err)
		}
	case "qa_report":
		if in.CheckRunID != "" || in.Check != nil || in.QA == nil || in.CheckKey != "" || in.ConfigDigest != "" || in.SchemaVersion != 0 || strings.TrimSpace(in.QA.Tool) == "" || strings.TrimSpace(in.QA.ToolVersion) == "" || strings.TrimSpace(in.QA.Observations) == "" || len(in.QA.Observations) > 16<<10 || (in.QA.Verdict != "pass" && in.QA.Verdict != "fail" && in.QA.Verdict != "unknown") {
			return invalid("QA requires bounded observations, tool/version and a verdict")
		}
	default:
		return invalid("unsupported review evidence kind")
	}
	return nil
}
func (s *FileReviewSources) verify(ctx context.Context, who authz.Context, v commit.Committed, in ReviewEvidenceInput, historical bool, actor ids.ID) error {
	if err := in.validate(); err != nil {
		return err
	}
	if in.ProjectID != v.ProjectID || in.Ref != v.Ref(who.InstanceID) || in.ManifestDigest != v.ManifestDigest {
		return errcode.New(errcode.RefMismatch, "")
	}
	if err := s.Use(ctx, who, v, authz.PurposeArchiveReview); err != nil {
		return err
	}
	if err := s.ledger.CheckVersionRead(ctx, v.AssetID, v.VersionID); err != nil {
		return err
	}
	if !historical {
		if err := s.ledger.authorize(ctx, who, "ledger.append_check", v.ProjectID, "version", v.VersionID); err != nil {
			return err
		}
		if err := s.ledger.CheckAssetWrite(ctx, v.AssetID); err != nil {
			return err
		}
		state, err := s.ledger.VersionControl(ctx, v.VersionID)
		if err != nil {
			return err
		}
		if state.ReviewState != "draft" && state.ReviewState != "withdrawn" {
			return errcode.New(errcode.ReviewTargetStale, "submitted evidence is immutable; submit a new target")
		}
	}
	if in.Check != nil && !historical {
		if err := s.producers.VerifyProducer(ctx, in.Check.Producer); err != nil {
			return err
		}
	}
	if in.QA != nil && who.PrincipalID == v.CommittedBy && !historical {
		return errcode.New(errcode.SelfReviewForbidden, "")
	}
	return s.execution.VerifyEvidence(ctx, who, v, actor, in, historical)
}

// AppendEvidence records a stable intent before writing files and then accepts
// the hash in ledger's own transaction with receipt/outbox. A failed acceptance
// leaves a recoverable intent; an orphan record can never satisfy a profile.
func (s *FileReviewSources) AppendEvidence(ctx context.Context, who authz.Context, key string, in ReviewEvidenceInput) (AcceptedEvidence, error) {
	var out AcceptedEvidence
	if err := in.validate(); err != nil {
		return out, err
	}
	l := s.ledger
	v, err := l.Version(ctx, in.Ref.AssetID, in.Ref.VersionID)
	if err != nil {
		return out, err
	}
	cmd, err := l.commandContext(ctx, who, key, "ledger.append_check", v.ProjectID, in)
	if err != nil {
		return out, err
	}
	var record storage.Record
	err = func() error {
		ctx, release, err := l.write(ctx, v.ProjectID)
		if err != nil {
			return err
		}
		defer release()
		if err = l.authorize(ctx, who, "ledger.append_check", v.ProjectID, "version", v.VersionID); err != nil {
			return err
		}
		if err = s.Use(ctx, who, v, authz.PurposeArchiveReview); err != nil {
			return err
		}
		receipt, err := l.lookup(ctx, l.db, cmd)
		if err != nil {
			return err
		}
		if receipt != nil && commands.Decide(receipt, cmd.RequestHash, l.clock.Now()) == commands.OutcomeExpired {
			return errcode.New(errcode.IdempotencyResultExpired, "")
		}
		if receipt != nil && receipt.Status == commands.ReceiptSucceeded {
			return json.Unmarshal(receipt.ResponseSummary, &out)
		}
		if err = s.verify(ctx, who, v, in, false, who.PrincipalID); err != nil {
			return err
		}
		result, err := l.store.Accept(ctx, l.db, cmd, commands.StagePrepared, []string{string(v.VersionID)}, func(ctx context.Context, tx *sql.Tx) error {
			id, err := l.ids.New()
			if err != nil {
				return err
			}
			payload, err := canonjson.CanonicalizeValue(in)
			if err != nil {
				return err
			}
			record = storage.Record{RecordID: id, ProjectID: v.ProjectID, AssetID: v.AssetID, VersionID: v.VersionID, ManifestDigest: v.ManifestDigest, Kind: in.Kind, PayloadSchema: "lantai.review-evidence/v1", Payload: payload, AuthorID: who.PrincipalID, SessionID: who.SessionID, OperationID: cmd.OperationID, CreatedAt: clock.Format(l.clock.Now())}
			if in.Check != nil {
				record.Producer = &in.Check.Producer
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO ledger_check_prepared VALUES(?,?)`, cmd.OperationID, encoded(record))
			return err
		})
		if err != nil {
			return err
		}
		cmd.OperationID = result.Receipt.OperationID
		if result.Outcome == commands.OutcomeReplay {
			return json.Unmarshal(result.Receipt.ResponseSummary, &out)
		}
		record, err = readJSON[storage.Record](ctx, l.db, `SELECT record_json FROM ledger_check_prepared WHERE operation_id=?`, cmd.OperationID)
		if err != nil {
			return err
		}
		op, err := l.store.GetOperation(ctx, l.db, cmd.OperationID)
		if err != nil {
			return err
		}
		cmd.RecoveryEpoch, cmd.SessionID = op.RecoveryEpoch, record.SessionID
		return l.finalCheck(ctx, cmd, who, "ledger.append_check", v.ProjectID, "version", v.VersionID)
	}()
	if err != nil || out.ID != "" {
		return out, err
	}
	installed, err := s.files.AppendRecord(ctx, record)
	if err != nil {
		return out, err
	}
	ctx, release, err := l.write(ctx, v.ProjectID)
	if err != nil {
		return out, err
	}
	defer release()
	if err = l.finalCheck(ctx, cmd, who, "ledger.append_check", v.ProjectID, "version", v.VersionID); err != nil {
		return out, err
	}
	receipt, err := l.store.ReceiptByOperation(ctx, l.db, cmd.OperationID)
	if err != nil {
		return out, err
	}
	if receipt.Status == commands.ReceiptSucceeded {
		err = json.Unmarshal(receipt.ResponseSummary, &out)
		return out, err
	}
	if err = s.verify(ctx, who, v, in, false, who.PrincipalID); err != nil {
		return out, err
	}
	raw, err := s.files.ReadRecord(ctx, v.ProjectID, v.AssetID, v.VersionID, record.RecordID)
	if err != nil {
		return out, err
	}
	if digest.Of(raw) != installed.Digest {
		return out, errcode.New(errcode.HashMismatch, "")
	}
	out = acceptedEvidence(record, in, installed.Digest)
	err = inTx(ctx, l.db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO ledger_check_records VALUES(?,?,?,?,?,?)`, record.RecordID, v.ProjectID, v.AssetID, v.VersionID, installed.Digest, cmd.OperationID); err != nil {
			return err
		}
		e, err := l.event(cmd, "check.accepted", "check", record.RecordID, 1, map[string]any{"evidence_id": record.RecordID})
		if err != nil {
			return err
		}
		if err = l.store.Complete(ctx, tx, cmd.OperationID, commands.StagePrepared, commands.Result{Status: commands.ReceiptSucceeded, ResponseCode: 201, Summary: out, Events: []event.Envelope{e}}); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM ledger_check_prepared WHERE operation_id=?`, cmd.OperationID)
		return err
	})
	return out, err
}
func acceptedEvidence(record storage.Record, in ReviewEvidenceInput, hash digest.Digest) AcceptedEvidence {
	e := AcceptedEvidence{CheckRunID: in.CheckRunID, ID: record.RecordID, Digest: hash, Ref: in.Ref, ManifestDigest: in.ManifestDigest, Kind: in.Kind, ActorID: record.AuthorID, Flow: in.Flow, CheckKey: in.CheckKey, SchemaVersion: in.SchemaVersion, ConfigDigest: in.ConfigDigest, Check: in.Check, CompletedAt: record.CreatedAt}
	if in.QA != nil {
		e.QAVerdict = in.QA.Verdict
	}
	return e
}
func (s *FileReviewSources) Evidence(ctx context.Context, who authz.Context, id ids.ID) (AcceptedEvidence, error) {
	var project, asset, version ids.ID
	var hash digest.Digest
	err := s.ledger.db.QueryRowContext(ctx, `SELECT project_id,asset_id,version_id,digest FROM ledger_check_records WHERE evidence_id=?`, id).Scan(&project, &asset, &version, &hash)
	if errors.Is(err, sql.ErrNoRows) && s.additional != nil {
		record, hash, e := s.additional.AcceptedEvidenceRecord(ctx, who, id)
		if e != nil {
			return AcceptedEvidence{}, e
		}
		if record.RecordID != id || !hash.Valid() || (record.Kind != "rights_evidence" && record.Kind != "correction") {
			return AcceptedEvidence{}, errcode.New(errcode.RefMismatch, "")
		}
		return AcceptedEvidence{ID: id, Digest: hash, Ref: ids.PermanentRef{InstanceID: who.InstanceID, AssetID: record.AssetID, VersionID: record.VersionID}, ManifestDigest: record.ManifestDigest, Kind: "license_evidence", ActorID: record.AuthorID, CompletedAt: record.CreatedAt}, nil
	}
	if err != nil {
		return AcceptedEvidence{}, missing(err)
	}
	v, err := s.ledger.Version(ctx, asset, version)
	if err != nil {
		return AcceptedEvidence{}, err
	}
	if err = s.Use(ctx, who, v, authz.PurposeArchiveReview); err != nil {
		return AcceptedEvidence{}, err
	}
	raw, err := s.files.ReadRecord(ctx, project, asset, version, id)
	if err != nil {
		return AcceptedEvidence{}, err
	}
	if digest.Of(raw) != hash {
		return AcceptedEvidence{}, errcode.New(errcode.HashMismatch, "")
	}
	var record storage.Record
	if err = json.Unmarshal(raw, &record); err != nil {
		return AcceptedEvidence{}, err
	}
	if record.RecordID != id || record.ProjectID != project || record.AssetID != asset || record.VersionID != version || record.ManifestDigest != v.ManifestDigest {
		return AcceptedEvidence{}, errcode.New(errcode.RefMismatch, "")
	}
	var in ReviewEvidenceInput
	if err = json.Unmarshal(record.Payload, &in); err != nil {
		return AcceptedEvidence{}, err
	}
	if record.Kind != in.Kind || record.PayloadSchema != "lantai.review-evidence/v1" {
		return AcceptedEvidence{}, invalid("evidence envelope differs")
	}
	if err = s.verify(ctx, who, v, in, true, record.AuthorID); err != nil {
		return AcceptedEvidence{}, err
	}
	return acceptedEvidence(record, in, hash), nil
}

// CheckApplicable is separate from Evidence: immutable bytes remain readable
// after revocation, while a new human acceptance must use current authority.
func (s *FileReviewSources) CheckApplicable(ctx context.Context, who authz.Context, v commit.Committed, e AcceptedEvidence) error {
	in := ReviewEvidenceInput{CheckRunID: e.CheckRunID, ProjectID: v.ProjectID, Ref: e.Ref, ManifestDigest: e.ManifestDigest, Flow: e.Flow, Kind: e.Kind, CheckKey: e.CheckKey, SchemaVersion: e.SchemaVersion, ConfigDigest: e.ConfigDigest, Check: e.Check}
	return s.execution.CheckApplicable(ctx, who, v, e.ActorID, in, e.ID)
}
