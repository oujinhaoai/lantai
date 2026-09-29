package query

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/event"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/ledger"
)

type CollaborationMembers interface {
	ProjectRecipients(context.Context, ids.ID) ([]identity.Member, error)
}

// CollaborationTasks is T05's current visibility and assignment authority. Refs
// enumerates task IDs in ascending order after the supplied ID; Recipients reads
// current assignment, never the event's old assignee. No task state is stored here.
// All reads inherit their caller's guard. A nil port explicitly disables tasks.
type CollaborationTasks interface {
	Current(context.Context, authz.Context, ObjectRef) (ObjectState, error)
	Refs(context.Context, ids.ID, ids.ID, int) ([]ids.ID, error)
	Recipients(context.Context, ObjectRef) ([]ids.ID, error)
}
type CoreCollaborationObjects struct {
	source  *ledger.CollaborationSource
	members CollaborationMembers
	tasks   CollaborationTasks
}

func NewCoreCollaborationObjects(source *ledger.CollaborationSource, members CollaborationMembers, tasks CollaborationTasks) (*CoreCollaborationObjects, error) {
	if source == nil || members == nil {
		return nil, errors.New("query: ledger and identity collaboration sources required")
	}
	return &CoreCollaborationObjects{source, members, tasks}, nil
}
func coreRef(r ObjectRef) ledger.DiscussionTarget {
	return ledger.DiscussionTarget{ProjectID: r.ProjectID, Kind: r.Kind, ID: r.ID}
}
func queryRef(r ledger.DiscussionTarget) ObjectRef {
	return ObjectRef{ProjectID: r.ProjectID, Kind: r.Kind, ID: r.ID}
}
func objectKey(r ObjectRef) string { return r.Kind + ":" + string(r.ID) }
func (a *CoreCollaborationObjects) ResolveEvent(_ context.Context, e event.Envelope) (ObjectRef, bool, error) {
	r := ObjectRef{ProjectID: e.ProjectID, ID: e.AggregateID}
	switch e.EventType {
	case "review.submitted", "review.withdrawn", "review.recorded":
		var p struct {
			Target ids.ID `json:"target_id"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return r, false, err
		}
		r.Kind = "review"
		r.ID = p.Target
	case "task_run.changed", "job.changed":
		var payload struct {
			TaskID ids.ID `json:"task_id"`
		}
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			return r, false, err
		}
		r.Kind = "task"
		r.ID = payload.TaskID
	case "comment.posted":
		r.Kind = "comment"
	case "trash.created", "trash.restored", "trash.purged", "trash.held", "trash.unheld", "trash.purge_due":
		r.Kind = "trash"
	case "version.committed", "rights.asserted", "rights.evidence_accepted":
		r.Kind = "version"
	case "publication.changed":
		r.Kind = "asset"
	case "ledger.project_registered":
		r.Kind = "project"
	case "ledger.metadata_committed", "ledger.control_changed":
		if !slices.Contains([]string{"project", "asset", "version"}, e.AggregateType) {
			return r, false, nil
		}
		r.Kind = e.AggregateType
	default:
		if !strings.HasPrefix(e.EventType, "task.") || e.AggregateType != "task" {
			return r, false, nil
		}
		if a.tasks == nil {
			return r, false, errcode.New(errcode.InvalidStateTransition, "T05 collaboration authority is not configured")
		}
		r.Kind = "task"
	}
	if e.SchemaVersion != 1 {
		return r, false, errcode.New(errcode.SchemaInvalid, "unsupported collaboration event version")
	}
	if !r.valid() {
		return r, false, errcode.New(errcode.RefMismatch, "invalid collaboration event locator")
	}
	return r, true, nil
}
func (a *CoreCollaborationObjects) candidates(ctx context.Context, ref ObjectRef) ([]ids.ID, error) {
	var idsIn []ids.ID
	var roles []string
	if ref.Kind == "task" {
		if a.tasks == nil {
			return nil, errcode.New(errcode.InvalidStateTransition, "T05 collaboration authority is not configured")
		}
		var err error
		idsIn, err = a.tasks.Recipients(ctx, ref)
		if err != nil {
			return nil, err
		}
	} else {
		fact, err := a.source.Facts(ctx, coreRef(ref))
		if err != nil {
			return nil, err
		}
		idsIn, roles = fact.Principals, fact.Roles
	}
	members, err := a.members.ProjectRecipients(ctx, ref.ProjectID)
	if err != nil {
		return nil, err
	}
	out := []ids.ID{}
	for _, member := range members {
		if slices.Contains(idsIn, member.PrincipalID) || slices.ContainsFunc(member.Roles, func(r identity.Role) bool { return slices.Contains(roles, string(r)) }) {
			out = append(out, member.PrincipalID)
		}
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}
func (a *CoreCollaborationObjects) Recipients(ctx context.Context, _ event.Envelope, ref ObjectRef) ([]ids.ID, error) {
	out, err := a.candidates(ctx, ref)
	if hidden(err) {
		return []ids.ID{}, nil
	}
	return out, err
}
func (a *CoreCollaborationObjects) Current(ctx context.Context, who authz.Context, ref ObjectRef) (ObjectState, error) {
	var out ObjectState
	if !ref.valid() {
		return out, errcode.New(errcode.SchemaInvalid, "")
	}
	if ref.Kind == "task" {
		if a.tasks == nil {
			return out, errcode.New(errcode.InvalidStateTransition, "T05 collaboration authority is not configured")
		}
		var err error
		out, err = a.tasks.Current(ctx, who, ref)
		if err != nil {
			return out, err
		}
	} else {
		f, err := a.source.Current(ctx, who, coreRef(ref))
		if err != nil {
			return out, err
		}
		out = ObjectState{Ref: queryRef(f.Ref), Revision: f.Revision, Title: f.Title, State: f.State, Priority: f.Priority, DueAt: f.DueAt, WaitingSince: f.WaitingSince}
	}
	recipients, err := a.candidates(ctx, ref)
	if err != nil {
		return ObjectState{}, err
	}
	out.Relevant = slices.Contains(recipients, who.PrincipalID)
	return out, nil
}

// refs merges two bounded, ordered authority pages. A cursor names the last
// scanned locator, including hidden objects, so filtering cannot stall resync.
func (a *CoreCollaborationObjects) refs(ctx context.Context, project ids.ID, after string, limit int) ([]ObjectRef, string, error) {
	if !validCollaborationCursor(after) || limit < 1 || limit > 1000 {
		return nil, "", errcode.New(errcode.SchemaInvalid, "invalid collaboration cursor")
	}
	refs, err := a.source.Refs(ctx, project, after, limit+1)
	if err != nil {
		return nil, "", err
	}
	out := make([]ObjectRef, 0, len(refs))
	for _, ref := range refs {
		out = append(out, queryRef(ref))
	}
	if a.tasks != nil && (after == "" || strings.SplitN(after, ":", 2)[0] <= "task") {
		var taskAfter ids.ID
		if strings.HasPrefix(after, "task:") {
			taskAfter = ids.ID(strings.TrimPrefix(after, "task:"))
		}
		tasks, err := a.tasks.Refs(ctx, project, taskAfter, limit+1)
		if err != nil {
			return nil, "", err
		}
		if len(tasks) > limit+1 {
			return nil, "", errors.New("query: task enumeration exceeds limit")
		}
		prior := taskAfter
		for _, id := range tasks {
			if !id.Valid() || id <= prior {
				return nil, "", errors.New("query: unordered task authority page")
			}
			prior = id
			ref := ObjectRef{ProjectID: project, Kind: "task", ID: id}
			if objectKey(ref) > after {
				out = append(out, ref)
			}
		}
	}
	slices.SortFunc(out, func(x, y ObjectRef) int { return strings.Compare(objectKey(x), objectKey(y)) })
	next := ""
	if len(out) > limit {
		out = out[:limit]
		next = objectKey(out[len(out)-1])
	}
	return out, next, nil
}
func (a *CoreCollaborationObjects) Snapshot(ctx context.Context, who authz.Context, project ids.ID, after string, limit int) ([]ObjectState, string, error) {
	refs, next, err := a.refs(ctx, project, after, limit)
	if err != nil {
		return nil, "", err
	}
	out := []ObjectState{}
	for _, ref := range refs {
		v, err := a.Current(ctx, who, ref)
		if hidden(err) {
			continue
		}
		if err != nil {
			return nil, "", err
		}
		out = append(out, v)
	}
	return out, next, nil
}
func (a *CoreCollaborationObjects) InboxSnapshot(ctx context.Context) ([]InboxSeed, error) {
	out := []InboxSeed{}
	var afterProject ids.ID
	scanned := 0
	for {
		projects, err := a.source.Projects(ctx, afterProject, 200)
		if err != nil {
			return nil, err
		}
		for _, project := range projects {
			after := ""
			for {
				refs, next, err := a.refs(ctx, project, after, 200)
				if err != nil {
					return nil, err
				}
				scanned += len(refs)
				if scanned > 100000 {
					return nil, errcode.New(errcode.QuotaExceeded, "inbox rebuild exceeds bounded authority scan")
				}
				for _, ref := range refs {
					recipients, err := a.Recipients(ctx, event.Envelope{}, ref)
					if err != nil {
						return nil, err
					}
					for _, id := range recipients {
						out = append(out, InboxSeed{PrincipalID: id, Ref: ref})
						if len(out) > 100000 {
							return nil, errcode.New(errcode.QuotaExceeded, "inbox rebuild exceeds bounded recipient expansion")
						}
					}
				}
				if next == "" {
					break
				}
				after = next
			}
		}
		if len(projects) < 200 {
			break
		}
		afterProject = projects[len(projects)-1]
	}
	return out, nil
}

var _ CollaborationObjects = (*CoreCollaborationObjects)(nil)
var _ InboxRebuilder = (*CoreCollaborationObjects)(nil)

func validCollaborationCursor(cursor string) bool {
	if cursor == "" {
		return true
	}
	kind, id, ok := strings.Cut(cursor, ":")
	return ok && ids.ID(id).Valid() && slices.Contains([]string{"asset", "comment", "project", "review", "task", "trash", "version"}, kind)
}
