package workflow

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/event"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	tc "github.com/oujinhaoai/lantai/internal/contract/tasks"
	"github.com/oujinhaoai/lantai/internal/events"
	"github.com/oujinhaoai/lantai/internal/ledger"
	"github.com/oujinhaoai/lantai/internal/tasks"
)

const postCommitCheckCommand = "workflow.post_commit_check"

type postCommitCheckLedger interface {
	PostCommitCheckEnrollment(context.Context, commit.Committed) (event.Envelope, bool, error)
}

func enabledEnrollment(rule commit.PostCommitCheckRule, e event.Envelope) (commit.PostCommitCheckEnrollment, bool) {
	var p commit.PostCommitCheckEnrollment
	if !rule.Valid() || e.EventType != ledger.EvPostCommitCheckEnrolled || e.SchemaVersion != 1 ||
		e.AggregateType != "version" || e.AggregateRevision != 1 ||
		e.ProjectID != rule.ProjectID || e.ActorID != rule.ActorID || !e.SessionID.Valid() || !e.OperationID.Valid() ||
		canonjson.Validate(e.Payload) != nil || json.Unmarshal(e.Payload, &p) != nil ||
		p.Rule != rule || p.VersionID != e.AggregateID || !p.VersionID.Valid() || !p.AssetID.Valid() || !p.ManifestDigest.Valid() {
		return p, false
	}
	return p, true
}

func (w *Service) postCommitIntent(e event.Envelope) []events.Command {
	if _, ok := enabledEnrollment(w.d.PostCommitCheck, e); !ok {
		return nil
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return nil
	} // the already-validated stored envelope is JSON
	return []events.Command{{StepKey: "pinned-human-checklist", CommandType: postCommitCheckCommand, Payload: raw}}
}

func (w *Service) postCommitAllowed(who authz.Context) bool {
	return who.InstanceID == w.d.InstanceID && w.d.PostCommitCheck.Allows(who)
}

// PostCommitChecklist is the commit fast path, using that request's verified
// human context. The ledger's enrollment stays durable for later replay if
// the request ends or permission prevents task acceptance.
func (w *Service) PostCommitChecklist(ctx context.Context, who authz.Context, project, version ids.ID) error {
	if !w.postCommitAllowed(who) || project != w.d.PostCommitCheck.ProjectID || !version.Valid() {
		return nil
	}
	v, err := w.d.Ledger.VersionByID(ctx, version)
	if err != nil {
		return err
	}
	source, ok := w.d.Ledger.(postCommitCheckLedger)
	if !ok {
		return fmt.Errorf("workflow: no ledger enrollment authority")
	}
	e, found, err := source.PostCommitCheckEnrollment(ctx, v)
	if err != nil || !found {
		return err
	}
	_, err = w.runPostCommitCheck(ctx, who, e)
	return err
}

func (w *Service) runPostCommitCheck(ctx context.Context, who authz.Context, e event.Envelope) (json.RawMessage, error) {
	skipped := json.RawMessage(`{"status":"not_applicable"}`)
	p, ok := enabledEnrollment(w.d.PostCommitCheck, e)
	if !ok || !w.postCommitAllowed(who) {
		return skipped, nil
	}
	v, err := w.d.Ledger.VersionByID(ctx, p.VersionID)
	if err != nil {
		return nil, err
	}
	if !p.Matches(v) || !v.ProofDigest.Valid() || v.OperationID != e.OperationID {
		return skipped, nil
	}
	// A relayed payload, even one matching current ledger facts, is not proof
	// of commit-time enrollment.
	source, ok := w.d.Ledger.(postCommitCheckLedger)
	if !ok {
		return nil, fmt.Errorf("workflow: no ledger enrollment authority")
	}
	origin, found, err := source.PostCommitCheckEnrollment(ctx, v)
	if err != nil {
		return nil, err
	}
	originPayload, valid := enabledEnrollment(w.d.PostCommitCheck, origin)
	if !found || !valid || originPayload != p || origin.OperationID != v.OperationID {
		return skipped, nil
	}
	if err := w.authorize(ctx, who, "workflow.operate", v.ProjectID, "project", v.ProjectID); err != nil {
		return nil, err
	}
	if err := w.authorize(ctx, who, "catalog.read", v.ProjectID, "version", v.VersionID); err != nil {
		return nil, err
	}
	if err := w.d.Ledger.CheckVersionRead(ctx, v.AssetID, v.VersionID); err != nil {
		return nil, err
	}
	if _, err := w.d.Manifests.ReadManifest(ctx, v); err != nil {
		return nil, err
	}
	// Stable even if this commit is observed through multiple event IDs or
	// after task creation succeeded but the dispatch result was lost. Tasks
	// persist the key and reject expired results rather than redoing the write.
	id, err := ids.DeriveChild(v.VersionID, "post-commit-human-checklist/v1:"+string(w.d.InstanceID))
	if err != nil {
		return nil, err
	}
	out, err := w.d.Tasks.Create(ctx, who, "post-check-"+string(id), tasks.CreateRequest{
		ProjectID: v.ProjectID, Type: "question", Title: "检查已提交的固定版本",
		Description: "人工检查固定版本与清单校验。此任务不会执行文件、审定或发布。清单摘要：" + string(v.ManifestDigest),
		InputRefs:   []ids.PermanentRef{v.Ref(w.d.InstanceID)}, ExpectedOutputs: []tc.OutputRequirement{},
		AcceptanceCriteria: []string{"确认固定版本与冻结清单一致；记录问题。不进行审定或发布。"},
	})
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"status": "created", "task_id": out.TaskID})
}

func (w *Service) dispatchPostCommitChecks(ctx context.Context, who authz.Context, limit int) (int, error) {
	if !w.postCommitAllowed(who) {
		return 0, nil
	}
	return w.consumer.Dispatch(ctx, limit, func(ctx context.Context, p events.Pending) (json.RawMessage, error) {
		var e event.Envelope
		if p.CommandType != postCommitCheckCommand || canonjson.Validate(p.Payload) != nil || json.Unmarshal(p.Payload, &e) != nil {
			return json.RawMessage(`{"status":"not_applicable"}`), nil
		}
		return w.runPostCommitCheck(ctx, who, e)
	})
}
