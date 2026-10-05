package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/oujinhaoai/lantai/internal/catalog"
	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/event"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

type PublishRequest struct {
	ProjectID        ids.ID `json:"project_id"`
	VersionID        ids.ID `json:"version_id"`
	ReviewID         ids.ID `json:"review_id"`
	ExpectedRevision int64  `json:"expected_publication_revision"`
	Action           string `json:"action"`
	Reason           string `json:"reason"`
}

func (r *Reviews) Publish(ctx context.Context, who authz.Context, key string, in PublishRequest) (Publication, error) {
	return r.publish(ctx, who, key, in, "")
}
func (r *Reviews) publish(ctx context.Context, who authz.Context, key string, in PublishRequest, request ids.ID) (Publication, error) {
	var out Publication
	s := r.ledger
	if !in.ProjectID.Valid() || !in.VersionID.Valid() || !in.ReviewID.Valid() || in.ExpectedRevision < 0 || (in.Action != "publish" && in.Action != "rollback") || strings.TrimSpace(in.Reason) == "" || len(in.Reason) > 4096 {
		return out, invalid("exact publication target/revision/reason required")
	}
	ctx, release, err := s.write(ctx, in.ProjectID)
	if err != nil {
		return out, err
	}
	defer release()
	action := authz.Action("ledger.publish")
	if request != "" {
		action = "catalog.read"
	}
	if err = s.authorize(ctx, who, action, in.ProjectID, "version", in.VersionID); err != nil {
		return out, err
	}
	v, err := s.VersionByID(ctx, in.VersionID)
	if err != nil {
		return out, err
	}
	if v.ProjectID != in.ProjectID {
		return out, errcode.New(errcode.RefMismatch, "")
	}
	if err = s.CheckVersionRead(ctx, v.AssetID, v.VersionID); err != nil {
		return out, err
	}
	cmd, err := s.commandContext(ctx, who, key, "ledger.publish", in.ProjectID, struct {
		PublishRequest
		RequestID ids.ID `json:"request_id,omitempty"`
	}{in, request})
	if err != nil {
		return out, err
	}
	receipt, err := s.lookup(ctx, s.db, cmd)
	if err != nil {
		return out, err
	}
	if receipt != nil {
		if commands.Decide(receipt, cmd.RequestHash, s.clock.Now()) == commands.OutcomeExpired {
			return out, errcode.New(errcode.IdempotencyResultExpired, "")
		}
		err = json.Unmarshal(receipt.ResponseSummary, &out)
		return out, err
	}
	if err = s.CheckAssetWrite(ctx, v.AssetID); err != nil {
		return out, err
	}
	state, err := s.VersionControl(ctx, v.VersionID)
	if err != nil {
		return out, err
	}
	if state.ReviewState != "approved" || state.EffectiveReviewID != in.ReviewID || state.Lifecycle != "active" {
		return out, errcode.New(errcode.NotPublishable, "")
	}
	a, err := s.AssetControl(ctx, v.AssetID)
	if err != nil {
		return out, err
	}
	if a.PublicationRevision != in.ExpectedRevision {
		return out, errcode.New(errcode.PublicationConflict, "")
	}
	if a.PublicationState == "published" && a.PublishedVersionID == v.VersionID {
		return out, errcode.New(errcode.PublicationConflict, "already published")
	}
	review, err := readJSON[Review](ctx, s.db, `SELECT record FROM ledger_reviews WHERE review_id=?`, in.ReviewID)
	if err != nil {
		return out, err
	}
	t, err := r.target(ctx, review.TargetID)
	if err != nil {
		return out, err
	}
	if err = validateReviewWaivers(review, t); err != nil {
		return out, err
	}
	if request != "" {
		var status string
		var storedReview ids.ID
		if err = s.db.QueryRowContext(ctx, `SELECT status,review_id FROM ledger_publication_requests WHERE request_id=?`, request).Scan(&status, &storedReview); err != nil {
			return out, missing(err)
		}
		if storedReview != in.ReviewID || !review.PublicationPending || (status != "pending" && status != "failed") {
			return out, errcode.New(errcode.ReviewTargetStale, "")
		}
		if err = r.sources.Task(ctx, who, v, t.Flow, "auto_publish"); err != nil {
			return out, err
		}
	}
	policies, err := r.policies.ResolvePolicies(ctx, t.ProjectID)
	if err != nil {
		return out, err
	}
	if err = r.satisfied(ctx, who, v, t, review.Waivers, policyBool(policies, "review.qa_required"), review.ActorID); err != nil {
		return out, err
	}
	if in.Action == "rollback" {
		var n int
		if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM ledger_publications WHERE asset_id=? AND json_extract(record,'$.to_version_id')=?`, v.AssetID, v.VersionID).Scan(&n); err != nil {
			return out, err
		}
		if n == 0 {
			return out, errcode.New(errcode.NotPublishable, "rollback target was never published")
		}
	}
	id, err := s.ids.New()
	if err != nil {
		return out, err
	}
	out = Publication{ID: id, AssetID: v.AssetID, FromVersionID: a.PublishedVersionID, ToVersionID: v.VersionID, Action: in.Action, ActorID: who.PrincipalID, Reason: in.Reason, OperationID: cmd.OperationID, PreviousRevision: a.PublicationRevision, Revision: a.PublicationRevision + 1, CreatedAt: clock.Format(s.clock.Now())}
	result, err := s.store.Execute(ctx, s.db, cmd, func(ctx context.Context, tx *sql.Tx) (commands.Result, error) {
		a.PublishedVersionID = v.VersionID
		a.PublicationState = "published"
		a.PublicationRevision++
		a.Revision++
		if err := saveAssetControl(ctx, tx, a); err != nil {
			return commands.Result{}, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO ledger_publications VALUES(?,?,?,?,?)`, id, v.AssetID, out.Revision, cmd.OperationID, encoded(out)); err != nil {
			return commands.Result{}, err
		}
		if request != "" {
			if _, err := tx.ExecContext(ctx, `UPDATE ledger_publication_requests SET status='succeeded',failure_code='' WHERE request_id=?`, request); err != nil {
				return commands.Result{}, err
			}
		}
		e, err := s.event(cmd, "publication.changed", "asset", v.AssetID, a.PublicationRevision, out)
		if err != nil {
			return commands.Result{}, err
		}
		return commands.Result{Status: commands.ReceiptSucceeded, ResponseCode: 201, Summary: out, Events: []event.Envelope{e}}, nil
	})
	if err != nil {
		return Publication{}, err
	}
	err = json.Unmarshal(result.Receipt.ResponseSummary, &out)
	return out, err
}

type PublicationRequest struct {
	ID               ids.ID `json:"request_id"`
	ReviewID         ids.ID `json:"review_id"`
	AssetID          ids.ID `json:"asset_id"`
	VersionID        ids.ID `json:"version_id"`
	ExpectedRevision int64  `json:"expected_publication_revision"`
	Status           string `json:"status"`
	FailureCode      string `json:"failure_code,omitempty"`
}

// PublicationRequests is an authorized bounded queue view; the immutable Review
// reports that it enqueued publication, while this record reports current status.
func (r *Reviews) PublicationRequests(ctx context.Context, who authz.Context, asset ids.ID) ([]PublicationRequest, error) {
	s := r.ledger
	a, err := s.Asset(ctx, asset)
	if err != nil {
		return nil, err
	}
	if err = s.authorize(ctx, who, "ledger.publish", a.ProjectID, "asset", asset); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT request_id,review_id,asset_id,version_id,expected_revision,status,failure_code FROM ledger_publication_requests WHERE asset_id=? ORDER BY rowid`, asset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PublicationRequest{}
	for rows.Next() {
		var p PublicationRequest
		if err = rows.Scan(&p.ID, &p.ReviewID, &p.AssetID, &p.VersionID, &p.ExpectedRevision, &p.Status, &p.FailureCode); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// RunPublication is a trusted dispatcher entry, never an automatic scheduler.
// T05 authorizes the persistent flow operation on every retry. It cannot be
// inferred from a backend success or an arbitrary caller's role.
func (r *Reviews) RunPublication(ctx context.Context, who authz.Context, id ids.ID) (Publication, error) {
	s := r.ledger
	var p PublicationRequest
	err := s.db.QueryRowContext(ctx, `SELECT request_id,review_id,asset_id,version_id,expected_revision,status,failure_code FROM ledger_publication_requests WHERE request_id=?`, id).Scan(&p.ID, &p.ReviewID, &p.AssetID, &p.VersionID, &p.ExpectedRevision, &p.Status, &p.FailureCode)
	if err != nil {
		return Publication{}, missing(err)
	}
	v, err := s.VersionByID(ctx, p.VersionID)
	if err != nil {
		return Publication{}, err
	}
	if err = s.authorize(ctx, who, "catalog.read", v.ProjectID, "version", v.VersionID); err != nil {
		return Publication{}, err
	}
	// Do not permit arbitrary project readers to write queue failure diagnostics.
	review, err := readJSON[Review](ctx, s.db, `SELECT record FROM ledger_reviews WHERE review_id=?`, p.ReviewID)
	if err != nil {
		return Publication{}, err
	}
	t, err := r.target(ctx, review.TargetID)
	if err != nil {
		return Publication{}, err
	}
	if err = r.sources.Task(ctx, who, v, t.Flow, "auto_publish"); err != nil {
		return Publication{}, err
	}
	out, err := r.publish(ctx, who, "publication-request-"+string(id), PublishRequest{ProjectID: v.ProjectID, VersionID: v.VersionID, ReviewID: p.ReviewID, ExpectedRevision: p.ExpectedRevision, Action: "publish", Reason: "approved flow publication request"}, id)
	if err != nil {
		code := errcode.CodeOf(err)
		if _, ok := errcode.Lookup(code); !ok {
			code = errcode.OperationNeedsReconciliation
		}
		// Conditional update cannot resurrect a concurrently revoked/cancelled request.
		_, writeErr := s.db.ExecContext(ctx, `UPDATE ledger_publication_requests SET status='failed',failure_code=? WHERE request_id=? AND status IN ('pending','failed')`, code, id)
		if writeErr != nil {
			return out, writeErr
		}
	}
	return out, err
}

var _ catalog.Publications = (*Service)(nil)

// Published 实现 catalog 的 @published：只读台账当前发布指针，未发布或已暂停时
// NOT_PUBLISHED。新的批准不移动指针，发布与回退由 Publish 追加记录。
func (s *Service) Published(ctx context.Context, asset ids.ID) (ids.ID, error) {
	a, err := assetControl(ctx, s.db, asset)
	if err != nil {
		return "", err
	}
	if a.PublicationState != "published" || a.PublishedVersionID == "" {
		return "", errcode.New(errcode.NotPublished, "")
	}
	return a.PublishedVersionID, nil
}

// Approved 实现 catalog 的 @approved：号码最大且当前仍为 approved、enabled、
// active、没有在途操作的版本，与发布的接受条件一致；没有时 NOT_FOUND。
func (s *Service) Approved(ctx context.Context, asset ids.ID) (ids.ID, error) {
	var id ids.ID
	err := s.db.QueryRowContext(ctx, `SELECT v.version_id FROM ledger_versions v JOIN ledger_version_states s ON s.version_id=v.version_id WHERE v.asset_id=? AND s.state='approved' AND s.availability='enabled' AND s.lifecycle='active' AND s.pending_operation_id='' ORDER BY v.version_number DESC LIMIT 1`, asset).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		const msg = "the asset has no approved version"
		return "", errcode.New(errcode.NotFound, msg).WithDetails(errcode.Detail{Reason: "no_approved_version", Message: msg})
	}
	return id, err
}
