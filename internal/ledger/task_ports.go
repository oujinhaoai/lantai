package ledger

import (
	"context"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

// CheckoutGuard is T05's current checkout and lease authority for appending a
// version to an existing asset (asset set) or for a task-bound write creating a
// new asset (asset empty). Ledger calls it under the shared security guard
// and project lock, outside its SQL transactions, at Prepare and again at final
// Commit. It must not reacquire those locks or write another module's data.
// A nil guard keeps the M1 behaviour (only locks, permissions and base version).
type CheckoutGuard interface {
	CheckVersionWrite(context.Context, authz.Context, commands.Context, ids.ID, ids.ID) error
}

// SetCheckoutGuard connects T05 after assembly. Nil disconnects it.
func (s *Service) SetCheckoutGuard(g CheckoutGuard) {
	s.wiring.Lock()
	defer s.wiring.Unlock()
	s.guard = g
}

func (s *Service) checkout(ctx context.Context, who authz.Context, cmd commands.Context, project, asset ids.ID) error {
	s.wiring.RLock()
	g := s.guard
	s.wiring.RUnlock()
	if g == nil {
		return nil
	}
	return g.CheckVersionWrite(ctx, who, cmd, project, asset)
}

// DiscussionMessage is a trusted read for T05, which binds block/answer/handoff
// records to an immutable message. It performs no authorization: the caller must
// already have authorized the task (the message's own target).
func (s *Service) DiscussionMessage(ctx context.Context, id ids.ID) (Message, error) {
	if !id.Valid() {
		return Message{}, invalid("message id required")
	}
	return readJSON[Message](ctx, s.db, `SELECT record FROM ledger_messages WHERE message_id=?`, id)
}

// ReviewFact is the authoritative review record with its frozen target.
type ReviewFact struct {
	Review Review       `json:"review"`
	Target ReviewTarget `json:"target"`
}

// ReviewByOperation and ReviewByID are trusted T05 reads: task completion and
// flow advancement use the recorded decision, never an event payload alone.
// They perform no authorization; callers authorize the owning task or flow.
func (r *Reviews) ReviewByOperation(ctx context.Context, operation ids.ID) (ReviewFact, error) {
	if !operation.Valid() {
		return ReviewFact{}, invalid("review operation required")
	}
	review, err := readJSON[Review](ctx, r.ledger.db, `SELECT record FROM ledger_reviews WHERE operation_id=?`, operation)
	if err != nil {
		return ReviewFact{}, err
	}
	return r.fact(ctx, review)
}
func (r *Reviews) ReviewByID(ctx context.Context, id ids.ID) (ReviewFact, error) {
	if !id.Valid() {
		return ReviewFact{}, invalid("review id required")
	}
	review, err := readJSON[Review](ctx, r.ledger.db, `SELECT record FROM ledger_reviews WHERE review_id=?`, id)
	if err != nil {
		return ReviewFact{}, err
	}
	return r.fact(ctx, review)
}
func (r *Reviews) fact(ctx context.Context, review Review) (ReviewFact, error) {
	t, err := r.target(ctx, review.TargetID)
	if err != nil {
		return ReviewFact{}, err
	}
	if t.VersionID != review.VersionID {
		return ReviewFact{}, errcode.New(errcode.RefMismatch, "")
	}
	return ReviewFact{Review: review, Target: t}, nil
}

// TargetFact is a trusted read of a frozen review target for T05 flow advancement.
func (r *Reviews) TargetFact(ctx context.Context, id ids.ID) (ReviewTarget, error) {
	if !id.Valid() {
		return ReviewTarget{}, invalid("review target required")
	}
	return r.target(ctx, id)
}

// EvidenceByOperation resolves the accepted Check/QA record created by one
// evidence operation, with the same current-rights check as Evidence.
func (s *FileReviewSources) EvidenceByOperation(ctx context.Context, who authz.Context, operation ids.ID) (AcceptedEvidence, error) {
	if !operation.Valid() {
		return AcceptedEvidence{}, invalid("evidence operation required")
	}
	var id ids.ID
	if err := s.ledger.db.QueryRowContext(ctx, `SELECT evidence_id FROM ledger_check_records WHERE operation_id=?`, operation).Scan(&id); err != nil {
		return AcceptedEvidence{}, missing(err)
	}
	return s.Evidence(ctx, who, id)
}

// EvidenceIDByOperation is a trusted T05 read resolving the accepted evidence
// record of one operation, used to fix a flow's review submission payload.
func (s *Service) EvidenceIDByOperation(ctx context.Context, operation ids.ID) (ids.ID, error) {
	var id ids.ID
	if err := s.db.QueryRowContext(ctx, `SELECT evidence_id FROM ledger_check_records WHERE operation_id=?`, operation).Scan(&id); err != nil {
		return "", missing(err)
	}
	return id, nil
}
