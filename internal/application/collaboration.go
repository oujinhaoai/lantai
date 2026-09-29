package application

import (
	"context"
	ax "github.com/oujinhaoai/lantai/internal/agent_execution"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/jobs"
	"github.com/oujinhaoai/lantai/internal/ledger"
	"github.com/oujinhaoai/lantai/internal/node"
	"github.com/oujinhaoai/lantai/internal/query"
	"github.com/oujinhaoai/lantai/internal/tasks"
	"github.com/oujinhaoai/lantai/internal/workflow"
)

func (a *App) assembleCollaboration() error {
	i := a.Instance
	auth := authority{a.Identity, i}
	db := i.DB(ownership.Runtime)
	var e error
	a.Tasks, e = tasks.New(tasks.Deps{DB: db, Gate: i.Gate(), Authority: auth, Policies: a.Identity, Members: a.Identity, Resources: a.Ledger, Discussions: a.Ledger, Clock: i.Clock(), IDs: i.IDs(), InstanceID: i.InstanceID()})
	if e != nil {
		return e
	}
	a.Nodes, e = node.New(node.Deps{DB: db, Gate: i.Gate(), Authority: auth, Clock: i.Clock(), IDs: i.IDs()})
	if e != nil {
		return e
	}
	a.Jobs, e = jobs.New(jobs.Deps{DB: db, Gate: i.Gate(), Authority: auth, Tasks: a.Tasks, Catalog: a.Catalog, Ledger: a.Ledger, Files: a.Storage, Rights: a.Rights, Host: a.Extensions, Nodes: a.Nodes, Clock: i.Clock(), IDs: i.IDs(), InstanceID: i.InstanceID()})
	if e != nil {
		return e
	}
	a.Flows, e = workflow.New(workflow.Deps{DB: db, Gate: i.Gate(), Authority: auth, Tasks: a.Tasks, Ledger: a.Ledger, Manifests: a.Storage, Events: a.Events, Jobs: a.Jobs, Checks: a.Jobs, Clock: i.Clock(), IDs: i.IDs(), InstanceID: i.InstanceID()})
	if e != nil {
		return e
	}
	a.Evidence, e = a.Ledger.NewFileReviewSources(a.Storage, a.Rights, a.Flows.ReviewExecution(), a.Extensions, a.Rights)
	if e != nil {
		return e
	}
	a.Reviews, e = a.Ledger.NewReviews(a.Evidence, a.Identity)
	if e != nil {
		return e
	}
	a.Flows.SetReviews(a.Reviews)
	a.Jobs.SetEvidence(a.Evidence)
	a.Jobs.SetExecution(a.Flows.ReviewExecution())
	a.Tasks.SetAuthorities(tasks.LedgerAuthorities{Reviews: a.Reviews, Evidence: a.Evidence})
	a.Ledger.SetCheckoutGuard(a.Tasks)
	a.Execution, e = ax.New(ax.Deps{DB: db, Gate: i.Gate(), Authority: auth, Tasks: a.Tasks, Assets: ax.CatalogAssets{Files: a.Storage, Evidence: a.Rights, Catalog: a.Catalog, InstanceID: i.InstanceID()}, Clock: i.Clock(), IDs: i.IDs()})
	if e != nil {
		return e
	}
	a.Tasks.SetExecutions(a.Execution)
	a.Lifecycle, e = a.Ledger.NewLifecycle(a.Storage, lifecycleUses{a}, a.Rights, a.Identity, a.Catalog)
	if e != nil {
		return e
	}
	a.Discussions, e = a.Ledger.NewDiscussionObjects(a.Catalog, a.Rights, a.Identity, a.Tasks)
	if e != nil {
		return e
	}
	source, e := a.Ledger.NewCollaborationSource(a.Reviews, a.Lifecycle, a.Discussions)
	if e != nil {
		return e
	}
	objects, e := query.NewCoreCollaborationObjects(source, a.Identity, a.Tasks)
	if e != nil {
		return e
	}
	a.Collaboration, e = a.Query.NewCollaboration(db, objects)
	return e
}

type lifecycleUses struct{ a *App }

func (x lifecycleUses) CurrentUses(ctx context.Context, w authz.Context, project ids.ID, refs []ids.PermanentRef) ([]ledger.LifecycleUse, error) {
	a := x.a
	incoming, e := a.Rights.IncomingUses(ctx, refs)
	if e != nil {
		return nil, e
	}
	out := []ledger.LifecycleUse{}
	seen := map[string]bool{}
	for _, u := range incoming {
		key := string(u.Source.VersionID) + u.Relation
		if seen[key] {
			continue
		}
		seen[key] = true
		allowed, e := a.Rights.EvaluateRiskAccess(ctx, w, u.Source)
		if e != nil {
			return nil, e
		}
		if e = allowed.Err(); e != nil {
			return nil, e
		}
		ref := u.Source
		out = append(out, ledger.LifecycleUse{Kind: u.Relation, ID: ref.VersionID, Revision: u.SourceRightsRevision, Ref: &ref, Digest: u.SourceManifestDigest})
	}
	assets := []ids.ID{}
	for _, ref := range refs {
		assets = append(assets, ref.AssetID)
	}
	uses, e := a.Tasks.OpenUses(ctx, assets)
	if e != nil {
		return nil, e
	}
	for _, u := range uses {
		b, e := canonjson.CanonicalizeValue(u)
		if e != nil {
			return nil, e
		}
		out = append(out, ledger.LifecycleUse{Kind: "task", ID: u.TaskID, Revision: u.Revision, Digest: digest.Of(b)})
	}
	// Flow bindings can reserve an asset between tasks; fail closed while such
	// work remains active, even if the current task list is momentarily empty.
	if len(uses) == 0 {
		if e = a.Flows.RequireIdle(ctx, project, assets); e != nil {
			return nil, e
		}
	}
	return out, nil
}
func (a *App) ValidateHumanTarget(ctx context.Context, w authz.Context, in identity.HumanAction) error {
	switch in.Action {
	case identity.ActReleaseRestriction:
		return a.Rights.ValidateHumanTarget(ctx, w, in)
	case identity.ActRecordReview, identity.ActRevokeReview:
		return a.Reviews.ValidateHumanTarget(ctx, w, in)
	case "ledger.trash", "ledger.force_trash", identity.ActHold, identity.ActUnhold, identity.ActPurge, identity.ActReleaseName:
		return a.Lifecycle.ValidateHumanTarget(ctx, w, in)
	default:
		return a.Ledger.ValidateHumanTarget(ctx, w, in)
	}
}
