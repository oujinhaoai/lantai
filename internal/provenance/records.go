package provenance

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

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
	"github.com/oujinhaoai/lantai/internal/storage"
)

const EvidenceContract = "lantai.provenance-evidence/v1"

// ExternalInput 固定未知外部来源。M1 只追加证据，不自动确认许可或解除限制。
type ExternalInput struct {
	SHA256       string `json:"sha256,omitempty"`
	SourceURL    string `json:"source_url,omitempty"`
	RightsStatus string `json:"rights_status"`
}
type Evidence struct {
	Note           string          `json:"note"`
	ExternalInputs []ExternalInput `json:"external_inputs"`
}
type AppendRequest struct {
	Who              authz.Context
	IdempotencyKey   string
	Ref              ids.PermanentRef
	ManifestDigest   digest.Digest
	ExpectedRevision int64
	Evidence         Evidence
	Supersedes       ids.ID
	Producer         *storage.Producer
}
type AcceptedRecord struct {
	storage.RecordRef
	Revision    int64  `json:"revision"`
	RightsEpoch int64  `json:"rights_epoch"`
	OperationID ids.ID `json:"operation_id"`
}

func (s *Service) CurrentRevision(ctx context.Context, version ids.ID) (int64, error) {
	var rev int64
	err := s.DB.QueryRowContext(ctx, `SELECT revision FROM provenance_targets WHERE version_id=?`, version).Scan(&rev)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return rev, err
}

// AppendEvidence 冻结待执行记录 -> T02 排他写文件 -> 最终复验授权/修订 ->
// 本库提交生效引用、回执和 outbox。孤立文件不参与用途计算，旧证据永不覆盖。
func (s *Service) AppendEvidence(ctx context.Context, r AppendRequest) (AcceptedRecord, error) {
	if err := r.Ref.Validate(true); err != nil || r.Ref.InstanceID != s.InstanceID || !r.ManifestDigest.Valid() || r.ExpectedRevision < 0 {
		return AcceptedRecord{}, errcode.New(errcode.SchemaInvalid, "invalid evidence target")
	}
	raw, err := canonjson.CanonicalizeValue(r.Evidence)
	if err != nil {
		return AcceptedRecord{}, err
	}
	reg, err := schema.Default()
	if err != nil {
		return AcceptedRecord{}, err
	}
	tree, err := canonjson.Decode(raw)
	if err != nil {
		return AcceptedRecord{}, err
	}
	if err = reg.Validate(EvidenceContract, tree); err != nil {
		return AcceptedRecord{}, errcode.Wrap(errcode.SchemaInvalid, "invalid provenance evidence", err)
	}
	if r.Producer != nil {
		if s.Producers == nil {
			return AcceptedRecord{}, errcode.New(errcode.SchemaInvalid, "producer has no trusted static registration")
		}
		if err = s.Producers.VerifyProducer(ctx, *r.Producer); err != nil {
			return AcceptedRecord{}, err
		}
	}
	v, err := s.Reader.Version(ctx, r.Ref.AssetID, r.Ref.VersionID)
	if err != nil {
		return AcceptedRecord{}, err
	}
	if v.ManifestDigest != r.ManifestDigest {
		return AcceptedRecord{}, errcode.New(errcode.PreconditionFailed, "evidence manifest digest mismatch")
	}
	body, err := canonjson.CanonicalizeValue(struct {
		Ref        ids.PermanentRef
		Digest     digest.Digest
		Revision   int64
		Evidence   Evidence
		Supersedes ids.ID
		Producer   *storage.Producer
	}{r.Ref, r.ManifestDigest, r.ExpectedRevision, r.Evidence, r.Supersedes, r.Producer})
	if err != nil {
		return AcceptedRecord{}, err
	}
	hash, err := commands.RequestHash(commands.HashInput{CommandType: "provenance.append_evidence", ProjectID: v.ProjectID, Body: body})
	if err != nil {
		return AcceptedRecord{}, err
	}
	op, err := s.IDs.New()
	if err != nil {
		return AcceptedRecord{}, err
	}
	cmd := commands.Context{OperationID: op, CommandType: "provenance.append_evidence", ActorID: r.Who.PrincipalID, SessionID: r.Who.SessionID, ProjectID: v.ProjectID, IdempotencyKey: r.IdempotencyKey, RequestHash: hash, RecoveryEpoch: r.Who.RecoveryEpoch}
	if err = cmd.Validate(); err != nil {
		return AcceptedRecord{}, errcode.Wrap(errcode.SchemaInvalid, "invalid command", err)
	}
	var record storage.Record
	var replay *AcceptedRecord
	err = func() error {
		lctx, h, e := s.Gate.Acquire(ctx, commands.Request{Security: commands.ModeShared, Assets: []string{string(v.AssetID)}})
		if e != nil {
			return e
		}
		defer h.Release()
		if e = s.authorizeAppend(lctx, r.Who, v); e != nil {
			return e
		}
		response, e := s.store.Accept(lctx, s.DB, cmd, commands.StagePrepared, []string{string(v.VersionID)}, func(ctx context.Context, tx *sql.Tx) error {
			var rev int64
			e := tx.QueryRowContext(ctx, `SELECT revision FROM provenance_targets WHERE version_id=?`, v.VersionID).Scan(&rev)
			if e != nil && !errors.Is(e, sql.ErrNoRows) {
				return e
			}
			if rev != r.ExpectedRevision {
				return errcode.New(errcode.PreconditionFailed, "evidence revision changed")
			}
			if r.Supersedes != "" {
				var id string
				if e = tx.QueryRowContext(ctx, `SELECT record_id FROM provenance_records WHERE version_id=? AND record_id=?`, v.VersionID, r.Supersedes).Scan(&id); e != nil {
					return errcode.New(errcode.NotFound, "superseded evidence is not accepted for this version")
				}
			}
			id, e := s.IDs.New()
			if e != nil {
				return e
			}
			record = storage.Record{RecordID: id, ProjectID: v.ProjectID, AssetID: v.AssetID, VersionID: v.VersionID, ManifestDigest: v.ManifestDigest, Kind: "rights_evidence", PayloadSchema: EvidenceContract, Payload: raw, Producer: r.Producer, AuthorID: r.Who.PrincipalID, SessionID: r.Who.SessionID, OperationID: cmd.OperationID, Supersedes: r.Supersedes, CreatedAt: clock.Format(s.Clock.Now())}
			if r.Supersedes != "" {
				record.Kind = "correction"
			}
			b, e := json.Marshal(record)
			if e != nil {
				return e
			}
			_, e = tx.ExecContext(ctx, `INSERT INTO provenance_prepared(operation_id,version_id,expected_revision,record_json) VALUES(?,?,?,?)`, cmd.OperationID, v.VersionID, r.ExpectedRevision, string(b))
			return e
		})
		if e != nil {
			return e
		}
		cmd.OperationID = response.Receipt.OperationID
		if response.Outcome == commands.OutcomeReplay {
			var got AcceptedRecord
			if e = json.Unmarshal(response.Receipt.ResponseSummary, &got); e != nil {
				return e
			}
			replay = &got
			return nil
		}
		var b string
		if e = s.DB.QueryRowContext(lctx, `SELECT record_json FROM provenance_prepared WHERE operation_id=?`, cmd.OperationID).Scan(&b); e != nil {
			return e
		}
		return json.Unmarshal([]byte(b), &record)
	}()
	if err != nil {
		return AcceptedRecord{}, err
	}
	if replay != nil {
		return *replay, nil
	}
	if s.Files == nil {
		return AcceptedRecord{}, errcode.New(errcode.OperationNeedsReconciliation, "evidence file adapter unavailable")
	}
	rr, err := s.Files.AppendRecord(ctx, record)
	if err != nil {
		return AcceptedRecord{}, err
	}
	result := AcceptedRecord{RecordRef: rr, Revision: r.ExpectedRevision + 1, OperationID: cmd.OperationID}
	err = func() error {
		lctx, h, e := s.Gate.Acquire(ctx, commands.Request{Security: commands.ModeExclusive, Assets: []string{string(v.AssetID)}})
		if e != nil {
			return e
		}
		defer h.Release()
		if e = s.authorizeAppend(lctx, r.Who, v); e != nil {
			return e
		}
		if r.Producer != nil {
			if e = s.Producers.VerifyProducer(lctx, *r.Producer); e != nil {
				return e
			}
		}
		b, e := s.Files.ReadRecord(lctx, v.ProjectID, v.AssetID, v.VersionID, record.RecordID)
		if e != nil {
			return e
		}
		if digest.Of(b) != rr.Digest {
			return errcode.New(errcode.HashMismatch, "evidence changed after installation")
		}
		tx, e := s.DB.BeginTx(lctx, nil)
		if e != nil {
			return e
		}
		defer tx.Rollback()
		receipt, e := s.store.ReceiptByOperation(lctx, tx, cmd.OperationID)
		if e != nil {
			return e
		}
		if receipt.Status == commands.ReceiptSucceeded {
			return json.Unmarshal(receipt.ResponseSummary, &result)
		}
		var rev int64
		e = tx.QueryRowContext(lctx, `SELECT revision FROM provenance_targets WHERE version_id=?`, v.VersionID).Scan(&rev)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		if rev != r.ExpectedRevision {
			return errcode.New(errcode.PreconditionFailed, "evidence revision changed; retain installed record for reconciliation")
		}
		_, e = tx.ExecContext(lctx, `INSERT INTO provenance_targets(version_id,revision) VALUES(?,?) ON CONFLICT(version_id) DO UPDATE SET revision=excluded.revision`, v.VersionID, result.Revision)
		if e != nil {
			return e
		}
		_, e = tx.ExecContext(lctx, `INSERT INTO provenance_records(record_id,version_id,digest,revision,operation_id) VALUES(?,?,?,?,?)`, record.RecordID, v.VersionID, rr.Digest, result.Revision, cmd.OperationID)
		if e != nil {
			return e
		}
		if result.RightsEpoch, e = bumpRightsEpoch(lctx, tx); e != nil {
			return e
		}
		ev, e := event.New(s.IDs, s.Clock, event.Params{EventType: "rights.evidence_accepted", SchemaVersion: 1, AggregateType: "version_rights", AggregateID: v.VersionID, AggregateRevision: result.Revision, ActorID: r.Who.PrincipalID, SessionID: r.Who.SessionID, ProjectID: v.ProjectID, OperationID: cmd.OperationID, Payload: map[string]any{"asset_id": v.AssetID, "version_id": v.VersionID, "record_id": record.RecordID, "rights_epoch": result.RightsEpoch}})
		if e != nil {
			return e
		}
		if e = s.store.Complete(lctx, tx, cmd.OperationID, commands.StagePrepared, commands.Result{Status: commands.ReceiptSucceeded, ResponseCode: 201, Summary: result, ResultRefs: []commands.ResultRef{{Kind: "evidence", ID: record.RecordID, Revision: result.Revision}}, Events: []event.Envelope{ev}}); e != nil {
			return e
		}
		if _, e = tx.ExecContext(lctx, `DELETE FROM provenance_prepared WHERE operation_id=?`, cmd.OperationID); e != nil {
			return e
		}
		return tx.Commit()
	}()
	return result, err
}
func (s *Service) authorizeAppend(ctx context.Context, who authz.Context, v commit.Committed) error {
	d, err := s.Authz.Authorize(ctx, who, ActionAppendEvidence, authz.Resource{ProjectID: v.ProjectID, Kind: "asset", ID: v.AssetID})
	if err != nil {
		return err
	}
	if err = d.Err(); err != nil {
		return err
	}
	// 追加证据也须能读取当前目标；不递归计算用途，避免未知来源阻止补充
	// 修复证据，但冻结清单与当前著录的 personal 都必须显式获权。
	d, err = s.Authz.Authorize(ctx, who, "catalog.read", authz.Resource{ProjectID: v.ProjectID, Kind: "asset", ID: v.AssetID})
	if err != nil {
		return err
	}
	if !d.Allowed {
		return hideReadDenial(d)
	}
	_, doc, err := s.read(ctx, v.Ref(s.InstanceID))
	if err != nil {
		return errcode.New(errcode.RightsPending, "target manifest cannot be verified")
	}
	if s.Catalog == nil {
		return errcode.New(errcode.RightsPending, "current asset description is unavailable")
	}
	a, err := s.Catalog.ReadProjection(ctx, v.AssetID)
	if err != nil {
		return errcode.New(errcode.RightsPending, "current asset description cannot be verified")
	}
	if a.Asset.AssetID != v.AssetID || a.Asset.ProjectID != v.ProjectID {
		return errcode.New(errcode.RightsPending, "current asset description target mismatch")
	}
	state, err := s.assertionState(ctx, v, doc)
	if err != nil {
		return errcode.New(errcode.RightsPending, "current rights assertion cannot be verified")
	}
	if state.Rights.Sensitivity == "personal" || a.Description.Sensitivity == "personal" {
		d, err = s.Authz.Authorize(ctx, who, ActionPersonalRead, authz.Resource{ProjectID: v.ProjectID, Kind: "asset", ID: v.AssetID})
		if err != nil {
			return err
		}
		if !d.Allowed {
			return hideReadDenial(d)
		}
	}
	return nil
}
func hideReadDenial(d authz.Decision) error {
	switch d.Code {
	case errcode.AuthRequired, errcode.TokenExpired, errcode.TokenRevoked:
		return d.Err()
	}
	return errcode.New(errcode.NotFound, "")
}
func (s *Service) evidenceState(ctx context.Context, v commit.Committed, confirmed map[ids.ID]bool) (int64, bool, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT record_id,digest,revision FROM provenance_records WHERE version_id=? ORDER BY revision`, v.VersionID)
	if err != nil {
		return 0, false, err
	}
	type item struct {
		id     ids.ID
		digest digest.Digest
		rev    int64
	}
	var items []item
	for rows.Next() {
		var i item
		if err = rows.Scan(&i.id, &i.digest, &i.rev); err != nil {
			rows.Close()
			return 0, false, err
		}
		items = append(items, i)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, false, err
	}
	current, err := s.CurrentRevision(ctx, v.VersionID)
	if err != nil {
		return 0, false, err
	}
	if current != int64(len(items)) {
		return 0, false, errcode.New(errcode.OperationNeedsReconciliation, "accepted evidence revision chain is incomplete")
	}
	for n, i := range items {
		if i.rev != int64(n+1) {
			return 0, false, errcode.New(errcode.OperationNeedsReconciliation, "accepted evidence revision chain has a gap")
		}
	}
	// 全库接受序号单调递增。任意来源的新证据都使 epoch 改变，不能用依赖
	// 树内 max(各对象 revision) 冒充对所有变化敏感的水位。
	var epoch int64
	if err = s.DB.QueryRowContext(ctx, `SELECT epoch FROM provenance_rights_epoch WHERE singleton=1`).Scan(&epoch); err != nil {
		return 0, false, err
	}
	unknown := false
	for _, i := range items {
		raw, e := s.Files.ReadRecord(ctx, v.ProjectID, v.AssetID, v.VersionID, i.id)
		if e != nil {
			return epoch, false, e
		}
		if digest.Of(raw) != i.digest {
			return epoch, false, errcode.New(errcode.HashMismatch, "accepted evidence changed")
		}
		var r storage.Record
		if e = json.Unmarshal(raw, &r); e != nil {
			return epoch, false, e
		}
		var evidence Evidence
		if e = json.Unmarshal(r.Payload, &evidence); e != nil {
			return epoch, false, e
		}
		if r.RecordID != i.id || r.ManifestDigest != v.ManifestDigest || r.VersionID != v.VersionID || r.ProjectID != v.ProjectID || r.AssetID != v.AssetID || r.PayloadSchema != EvidenceContract || (r.Kind != "rights_evidence" && r.Kind != "correction") {
			return epoch, false, errcode.New(errcode.HashMismatch, "accepted evidence target mismatch")
		}
		reg, e := schema.Default()
		if e != nil {
			return epoch, false, e
		}
		tree, e := canonjson.Decode(raw)
		if e != nil {
			return epoch, false, e
		}
		if e = reg.Validate(storage.RecordContract, tree); e != nil {
			return epoch, false, e
		}
		payload, e := canonjson.Decode(r.Payload)
		if e != nil {
			return epoch, false, e
		}
		if e = reg.Validate(EvidenceContract, payload); e != nil {
			return epoch, false, e
		}
		unknown = unknown || len(evidence.ExternalInputs) > 0 && !confirmed[i.id]
	}
	return epoch, unknown, nil
}

// AcceptedEvidenceRecord is a trusted read port for T03 review assembly. It
// resolves only this module's committed evidence references, rechecks current
// access (including personal restrictions), and verifies the immutable bytes.
// Caller holds the shared security guard. An orphan file is never returned.
func (s *Service) AcceptedEvidenceRecord(ctx context.Context, who authz.Context, id ids.ID) (storage.Record, digest.Digest, error) {
	var out storage.Record
	var version ids.ID
	var hash digest.Digest
	err := s.DB.QueryRowContext(ctx, `SELECT version_id,digest FROM provenance_records WHERE record_id=?`, id).Scan(&version, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return out, "", errcode.New(errcode.NotFound, "")
	}
	if err != nil {
		return out, "", err
	}
	reader, ok := s.Reader.(interface {
		VersionByID(context.Context, ids.ID) (commit.Committed, error)
	})
	if !ok {
		return out, "", errcode.New(errcode.RightsPending, "version identity resolver unavailable")
	}
	v, err := reader.VersionByID(ctx, version)
	if err != nil {
		return out, "", err
	}
	d, err := s.Authz.Authorize(ctx, who, "catalog.read", authz.Resource{ProjectID: v.ProjectID, Kind: "version", ID: v.VersionID})
	if err != nil {
		return out, "", err
	}
	if !d.Allowed {
		return out, "", hideReadDenial(d)
	}
	use, err := s.EvaluateUse(ctx, who, v.Ref(s.InstanceID), authz.PurposeArchiveReview)
	if err != nil {
		return out, "", err
	}
	if err = use.Err(); err != nil {
		return out, "", err
	}
	if s.Files == nil {
		return out, "", errcode.New(errcode.RightsPending, "")
	}
	raw, err := s.Files.ReadRecord(ctx, v.ProjectID, v.AssetID, v.VersionID, id)
	if err != nil {
		return out, "", err
	}
	if digest.Of(raw) != hash {
		return out, "", errcode.New(errcode.HashMismatch, "")
	}
	if err = json.Unmarshal(raw, &out); err != nil {
		return out, "", err
	}
	if out.RecordID != id || out.ProjectID != v.ProjectID || out.AssetID != v.AssetID || out.VersionID != v.VersionID || out.ManifestDigest != v.ManifestDigest || out.PayloadSchema != EvidenceContract || (out.Kind != "rights_evidence" && out.Kind != "correction") {
		return storage.Record{}, "", errcode.New(errcode.RefMismatch, "")
	}
	return out, hash, nil
}
