package identity

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/event"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/tasks"
)

// TaskProgressReader is implemented by T05; identity never reads its tables.
// The response must contain exactly the requested tasks, from this project.
// This in-process read port inherits the identity security guard; it must not
// reacquire that guard or modify identity data.
type TaskProgressReader interface {
	MilestoneTasks(context.Context, authz.Context, ids.ID, []ids.ID) ([]tasks.Task, error)
}

type Milestone struct {
	ID        ids.ID   `json:"milestone_id"`
	ProjectID ids.ID   `json:"project_id"`
	Revision  int64    `json:"revision"`
	Name      string   `json:"name"`
	DueDate   string   `json:"due_date"`
	OwnerID   ids.ID   `json:"owner_id"`
	TaskIDs   []ids.ID `json:"task_ids"`
}

type MilestoneProgress struct {
	Milestone Milestone `json:"milestone"`
	Completed int       `json:"completed"`
	Total     int       `json:"total"`
}

func readMilestone(ctx context.Context, q commands.DBTX, project, id ids.ID) (Milestone, error) {
	var m Milestone
	var tasksJSON string
	err := q.QueryRowContext(ctx, `SELECT milestone_id,project_id,revision,name,due_date,owner_id,task_ids FROM identity_milestones WHERE milestone_id=? AND project_id=?`, id, project).Scan(&m.ID, &m.ProjectID, &m.Revision, &m.Name, &m.DueDate, &m.OwnerID, &tasksJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return m, notFound("milestone")
	}
	if err != nil {
		return m, err
	}
	m.TaskIDs, err = decodeList[ids.ID](tasksJSON)
	return m, err
}

// PutMilestone creates (expected=0 and empty ID) or conditionally replaces metadata.
// New IDs are allocated by the server and retained in the durable receipt. It does
// not grant roles: the designated owner must already be an active project owner.
func (s *Service) PutMilestone(ctx context.Context, who authz.Context, key string, expected int64, m Milestone, reader TaskProgressReader) (Result, error) {
	if (expected == 0 && m.ID != "") || (expected > 0 && !m.ID.Valid()) || !m.ProjectID.Valid() || !m.OwnerID.Valid() || expected < 0 || len(strings.TrimSpace(m.Name)) == 0 || len(m.Name) > 256 || len(m.TaskIDs) > 1000 {
		return Result{}, fixRequest("invalid milestone")
	}
	if m.DueDate != "" {
		if _, err := time.Parse("2006-01-02", m.DueDate); err != nil {
			return Result{}, fixRequest("due_date must be YYYY-MM-DD")
		}
	}
	m.TaskIDs = slices.Clone(m.TaskIDs)
	slices.Sort(m.TaskIDs)
	for i, id := range m.TaskIDs {
		if !id.Valid() || (i > 0 && id == m.TaskIDs[i-1]) {
			return Result{}, fixRequest("task IDs must be valid and unique")
		}
	}
	m.Revision = expected + 1
	hash, err := commands.RequestHash(commands.HashInput{CommandType: string(ActProjectMilestonesWrite), ProjectID: m.ProjectID, Targets: []string{string(m.ID)}, ExpectedRevisions: map[string]int64{string(m.ID): expected}, Body: []byte(jsonText(m))})
	if err != nil {
		return Result{}, err
	}
	ctx, release, err := s.write(ctx, true)
	if err != nil {
		return Result{}, err
	}
	defer release()
	v, err := s.current(ctx, who)
	if err != nil {
		return Result{}, err
	}
	if err = s.require(ctx, s.main, v, ActProjectMilestonesWrite, authz.Resource{ProjectID: m.ProjectID}); err != nil {
		return Result{}, err
	}
	op, err := s.newID()
	if err != nil {
		return Result{}, err
	}
	epoch, err := s.recoveryEpoch(ctx)
	if err != nil {
		return Result{}, err
	}
	cc := commands.Context{OperationID: op, ActorID: v.principal.ID, SessionID: v.sess.ID, ProjectID: m.ProjectID, IdempotencyKey: key, CommandType: string(ActProjectMilestonesWrite), RequestHash: hash, PolicyRevision: v.policyRev, RecoveryEpoch: epoch}
	if err := cc.Validate(); err != nil {
		return Result{}, fixRequest("%v", err)
	}
	if prior, e := s.mainStore.LookupReceipt(ctx, s.main, cc.Key()); e == nil {
		outcome := commands.Decide(prior, hash, s.now())
		if err := outcome.Err(prior); err != nil {
			return Result{}, err
		}
		if outcome == commands.OutcomeReplay {
			return Result{OperationID: prior.OperationID, Replayed: true, Summary: prior.ResponseSummary}, nil
		}
	} else if !errors.Is(e, commands.ErrNotFound) {
		return Result{}, e
	}
	// Query the owner module before opening our SQL transaction. The identity
	// guard remains held so membership cannot be revoked during acceptance.
	if _, err = milestoneCounts(ctx, who, m, reader); err != nil {
		return Result{}, err
	}
	resp, err := s.mainStore.Execute(ctx, s.main, cc, func(ctx context.Context, tx *sql.Tx) (commands.Result, error) {
		if expected == 0 {
			id, e := s.newID()
			if e != nil {
				return commands.Result{}, e
			}
			m.ID = id
		}
		old, e := readMilestone(ctx, tx, m.ProjectID, m.ID)
		if e != nil && !milestoneNotFound(e) {
			return commands.Result{}, e
		}
		if (e == nil && old.Revision != expected) || (e != nil && expected != 0) {
			return commands.Result{}, preconditionFailed("milestone revision changed")
		}
		p, e := loadPrincipal(ctx, tx, m.OwnerID)
		if e != nil {
			return commands.Result{}, notFound("owner")
		}
		roles, e := projectRolesOf(ctx, tx, m.ProjectID, m.OwnerID)
		if e != nil {
			return commands.Result{}, e
		}
		if p.State != StateActive || !slices.Contains(roles, RoleOwner) {
			return commands.Result{}, fixRequest("designated owner must be an active project owner")
		}
		_, e = tx.ExecContext(ctx, `INSERT INTO identity_milestones VALUES (?,?,?,?,?,?,?,?) ON CONFLICT(milestone_id) DO UPDATE SET revision=excluded.revision,name=excluded.name,due_date=excluded.due_date,owner_id=excluded.owner_id,task_ids=excluded.task_ids,updated_at=excluded.updated_at WHERE identity_milestones.project_id=excluded.project_id`, m.ID, m.ProjectID, m.Revision, m.Name, m.DueDate, m.OwnerID, jsonText(m.TaskIDs), clock.Millis(s.now()))
		if e != nil {
			return commands.Result{}, e
		}
		// A caller cannot take over another project's stable ID.
		saved, e := readMilestone(ctx, tx, m.ProjectID, m.ID)
		if e != nil {
			return commands.Result{}, e
		}
		ev, e := s.event(evParams{typ: "project.milestone_changed", aggType: "milestone", aggID: m.ID, revision: saved.Revision, actor: v.principal.ID, session: v.sess.ID, project: m.ProjectID, operation: op, payload: m})
		if e != nil {
			return commands.Result{}, e
		}
		return commands.Result{Status: commands.ReceiptSucceeded, ResponseCode: 200, Summary: saved, Events: []event.Envelope{ev}}, nil
	})
	if err != nil {
		return Result{}, err
	}
	return Result{OperationID: resp.Receipt.OperationID, Replayed: resp.Outcome == commands.OutcomeReplay, Summary: resp.Receipt.ResponseSummary}, nil
}

func milestoneCounts(ctx context.Context, who authz.Context, m Milestone, r TaskProgressReader) (MilestoneProgress, error) {
	out := MilestoneProgress{Milestone: m, Total: len(m.TaskIDs)}
	if len(m.TaskIDs) == 0 {
		return out, nil
	}
	if r == nil {
		return out, fmt.Errorf("identity: T05 task progress reader is not configured")
	}
	found, err := r.MilestoneTasks(ctx, who, m.ProjectID, slices.Clone(m.TaskIDs))
	if err != nil {
		return out, err
	}
	seen := map[ids.ID]bool{}
	for _, t := range found {
		if t.ProjectID != m.ProjectID || !slices.Contains(m.TaskIDs, t.ID) || seen[t.ID] {
			return out, fixRequest("task query returned a foreign or duplicate task")
		}
		seen[t.ID] = true
		if t.State == "done" {
			out.Completed++
		}
	}
	if len(seen) != len(m.TaskIDs) {
		return out, notFound("milestone task")
	}
	return out, nil
}

func (s *Service) MilestoneProgress(ctx context.Context, who authz.Context, project, id ids.ID, r TaskProgressReader) (MilestoneProgress, error) {
	var out MilestoneProgress
	err := s.BeginRead(ctx, who, ActProjectMilestonesRead, authz.Resource{ProjectID: project}, func(ctx context.Context, _ authz.Decision) error {
		m, e := readMilestone(ctx, s.main, project, id)
		if e != nil {
			return e
		}
		out, e = milestoneCounts(ctx, who, m, r)
		return e
	})
	return out, err
}

func milestoneNotFound(err error) bool {
	e, ok := errcode.As(err)
	return ok && e.Code == errcode.NotFound
}

// ListMilestones returns metadata only. Progress is computed from T05 on demand.
func (s *Service) ListMilestones(ctx context.Context, who authz.Context, project ids.ID) ([]Milestone, error) {
	out := []Milestone{}
	err := s.BeginRead(ctx, who, ActProjectMilestonesRead, authz.Resource{ProjectID: project}, func(ctx context.Context, _ authz.Decision) error {
		rows, e := s.main.QueryContext(ctx, `SELECT milestone_id FROM identity_milestones WHERE project_id=? ORDER BY milestone_id`, project)
		if e != nil {
			return e
		}
		var all []ids.ID
		for rows.Next() {
			var id ids.ID
			if e = rows.Scan(&id); e != nil {
				rows.Close()
				return e
			}
			all = append(all, id)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
		for _, id := range all {
			m, e := readMilestone(ctx, s.main, project, id)
			if e != nil {
				return e
			}
			out = append(out, m)
		}
		return nil
	})
	return out, err
}
