package ledger

import (
	"context"
	"errors"

	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

// CollaborationFact is a current ledger fact, not an event payload or a grant.
// Recipients are candidates only; T01 membership and current object permission
// must still be checked before any user-visible delivery.
type CollaborationFact struct {
	Ref                               DiscussionTarget
	Revision                          int64
	Title, State, DueAt, WaitingSince string
	Priority                          int
	Principals                        []ids.ID
	Roles                             []string
}

// CollaborationSource owns the SQL reads for T04. Its trusted enumeration and
// Facts methods return locators/current routing facts only. Current inherits the
// caller's security guard and enforces current visibility before returning data.
type CollaborationSource struct {
	ledger    *Service
	reviews   *Reviews
	lifecycle *Lifecycle
	access    DiscussionAccess
}

func (s *Service) NewCollaborationSource(reviews *Reviews, lifecycle *Lifecycle, access DiscussionAccess) (*CollaborationSource, error) {
	if access == nil {
		return nil, errors.New("ledger: current discussion/object authority required")
	}
	if reviews != nil && reviews.ledger != s || lifecycle != nil && lifecycle.ledger != s {
		return nil, errors.New("ledger: collaboration sources must share authority")
	}
	return &CollaborationSource{s, reviews, lifecycle, access}, nil
}
func (s *CollaborationSource) Projects(ctx context.Context, after ids.ID, limit int) ([]ids.ID, error) {
	projects, err := s.ledger.Projects(ctx, after, limit)
	if err != nil {
		return nil, err
	}
	out := make([]ids.ID, 0, len(projects))
	for _, p := range projects {
		out = append(out, p.ProjectID)
	}
	return out, nil
}

// Refs orders every supported core locator by kind:id. Hidden locators may be
// scanned, but never returned to a user without Current. SQL touches ledger only.
func (s *CollaborationSource) Refs(ctx context.Context, project ids.ID, after string, limit int) ([]DiscussionTarget, error) {
	if !project.Valid() || limit < 1 || limit > 1001 || len(after) > 128 {
		return nil, invalid("invalid collaboration enumeration")
	}
	rows, err := s.ledger.db.QueryContext(ctx, `SELECT kind,id FROM (
 SELECT 'project' kind,project_id id,project_id FROM ledger_projects
 UNION ALL SELECT 'asset',asset_id,project_id FROM ledger_assets WHERE record IS NOT NULL
 UNION ALL SELECT 'version',version_id,project_id FROM ledger_versions
 UNION ALL SELECT 'review',target_id,project_id FROM ledger_review_targets
 UNION ALL SELECT 'comment',message_id,project_id FROM ledger_messages
 UNION ALL SELECT 'trash',trash_id,project_id FROM ledger_trash_entries
 ) WHERE project_id=? AND kind||':'||id>? ORDER BY kind,id LIMIT ?`, project, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DiscussionTarget{}
	for rows.Next() {
		r := DiscussionTarget{ProjectID: project}
		if err = rows.Scan(&r.Kind, &r.ID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
func (s *CollaborationSource) Facts(ctx context.Context, ref DiscussionTarget) (CollaborationFact, error) {
	out := CollaborationFact{Ref: ref, Priority: 100, Revision: 1}
	l := s.ledger
	switch ref.Kind {
	case "project":
		p, err := l.Project(ctx, ref.ID)
		if err != nil {
			return out, err
		}
		if p.ProjectID != ref.ProjectID {
			return out, errcode.New(errcode.NotFound, "")
		}
		out.Title = p.Key
		out.State = string(p.State)
		if err = l.db.QueryRowContext(ctx, `SELECT control_revision FROM ledger_projects WHERE project_id=?`, p.ProjectID).Scan(&out.Revision); err != nil {
			return out, err
		}
	case "asset", "version":
		var v commit.Committed
		var err error
		if ref.Kind == "asset" {
			v, err = l.LatestVersion(ctx, ref.ID)
		} else {
			v, err = l.VersionByID(ctx, ref.ID)
		}
		if err != nil {
			return out, err
		}
		if v.ProjectID != ref.ProjectID {
			return out, errcode.New(errcode.NotFound, "")
		}
		a, err := l.Asset(ctx, v.AssetID)
		if err != nil {
			return out, err
		}
		ac, err := l.AssetControl(ctx, v.AssetID)
		if err != nil {
			return out, err
		}
		vc, err := l.VersionControl(ctx, v.VersionID)
		if err != nil {
			return out, err
		}
		out.Title = a.Slug
		out.Revision = ac.Revision + vc.Revision + v.VersionNumber
		out.State = vc.ReviewState
		if vc.Availability != "enabled" {
			out.State = vc.Availability
		}
		if vc.Lifecycle != "active" {
			out.State = vc.Lifecycle
		}
		if ac.Lifecycle != "active" {
			out.State = ac.Lifecycle
		}
		p, err := l.Project(ctx, v.ProjectID)
		if err != nil {
			return out, err
		}
		var projectRevision int64
		if err = l.db.QueryRowContext(ctx, `SELECT control_revision FROM ledger_projects WHERE project_id=?`, p.ProjectID).Scan(&projectRevision); err != nil {
			return out, err
		}
		out.Revision += projectRevision
		if p.State == commit.ProjectArchived {
			out.State = "archived"
		}
	case "review":
		if s.reviews == nil {
			return out, errcode.New(errcode.InvalidStateTransition, "review authority required")
		}
		target, err := s.reviews.target(ctx, ref.ID)
		if err != nil {
			return out, err
		}
		if target.ProjectID != ref.ProjectID {
			return out, errcode.New(errcode.NotFound, "")
		}
		state, err := l.VersionControl(ctx, target.VersionID)
		if err != nil {
			return out, err
		}
		out.Title = "Review"
		out.Revision = state.Revision
		out.State = state.ReviewState
		if state.ReviewTargetID != target.ID {
			out.State = "superseded"
		}
		if out.State == "submitted" {
			out.Priority = 20
			out.WaitingSince = target.CreatedAt
			out.Roles = []string{"owner", "reviewer"}
		}
		if out.State == "changes_requested" || out.State == "rejected" {
			out.Priority = 30
			out.WaitingSince = target.CreatedAt
			out.Principals = []ids.ID{target.SubmittedBy, target.MakerID}
		}
	case "comment":
		m, err := readJSON[Message](ctx, l.db, `SELECT record FROM ledger_messages WHERE message_id=?`, ref.ID)
		if err != nil {
			return out, err
		}
		if m.Target.ProjectID != ref.ProjectID {
			return out, errcode.New(errcode.NotFound, "")
		}
		out.Title = m.Kind
		out.State = "posted"
		out.WaitingSince = m.CreatedAt
		out.Priority = 50
		for _, mention := range m.Mentions {
			if mention.PrincipalID != "" {
				out.Principals = append(out.Principals, mention.PrincipalID)
			} else {
				out.Roles = append(out.Roles, mention.Role)
			}
		}
		if m.ReplyTo != "" {
			parent, err := readJSON[Message](ctx, l.db, `SELECT record FROM ledger_messages WHERE message_id=?`, m.ReplyTo)
			if err != nil {
				return out, err
			}
			if parent.Target != m.Target {
				return out, errcode.New(errcode.RefMismatch, "")
			}
			out.Principals = append(out.Principals, parent.AuthorID)
		}
	case "trash":
		if s.lifecycle == nil {
			return out, errcode.New(errcode.InvalidStateTransition, "lifecycle authority required")
		}
		e, err := s.lifecycle.entry(ctx, ref.ID)
		if err != nil {
			return out, err
		}
		if e.ProjectID != ref.ProjectID {
			return out, errcode.New(errcode.NotFound, "")
		}
		out.Title = "Trash"
		out.State = e.State
		out.Revision = e.Revision
		out.DueAt = e.DueAt
		due, err := clock.Parse(e.DueAt)
		if err != nil {
			return out, err
		}
		if e.State == "trashed" && !e.Hold && e.PendingOperationID == "" && due.Sub(l.clock.Now()).Hours() <= 72 {
			out.Priority = 10
			out.Principals = []ids.ID{e.CreatedBy}
			out.Roles = []string{"owner"}
		}
	default:
		return out, invalid("unsupported core collaboration kind")
	}
	return out, nil
}
func (s *CollaborationSource) Current(ctx context.Context, who authz.Context, ref DiscussionTarget) (CollaborationFact, error) {
	empty := CollaborationFact{}
	if !ref.ProjectID.Valid() || !ref.ID.Valid() {
		return empty, invalid("invalid object reference")
	}
	if err := s.ledger.authorize(ctx, who, "catalog.read", ref.ProjectID, ref.Kind, ref.ID); err != nil {
		return empty, err
	}
	switch ref.Kind {
	case "project", "asset", "version":
		if err := s.ledger.discussionTarget(ctx, who, ref, false, s.access); err != nil {
			return empty, err
		}
	case "review":
		if s.reviews == nil {
			return empty, errcode.New(errcode.InvalidStateTransition, "review authority required")
		}
		t, err := s.reviews.visibleTarget(ctx, who, ref.ID)
		if err != nil {
			return empty, err
		}
		if t.ProjectID != ref.ProjectID {
			return empty, errcode.New(errcode.NotFound, "")
		}
	case "comment":
		m, err := readJSON[Message](ctx, s.ledger.db, `SELECT record FROM ledger_messages WHERE message_id=?`, ref.ID)
		if err != nil {
			return empty, err
		}
		if m.Target.ProjectID != ref.ProjectID {
			return empty, errcode.New(errcode.NotFound, "")
		}
		if err = s.ledger.discussionTarget(ctx, who, m.Target, false, s.access); err != nil {
			return empty, err
		}
	case "trash":
		if s.lifecycle == nil {
			return empty, errcode.New(errcode.InvalidStateTransition, "lifecycle authority required")
		}
		if _, err := s.lifecycle.visibleEntry(ctx, who, ref.ProjectID, ref.ID); err != nil {
			return empty, err
		}
	default:
		return empty, invalid("unsupported core collaboration kind")
	}
	return s.Facts(ctx, ref)
}
