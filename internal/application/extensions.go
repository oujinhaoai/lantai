package application

import (
	"cmp"
	"context"
	"time"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/operations"
	"github.com/oujinhaoai/lantai/internal/storage"

	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	"github.com/oujinhaoai/lantai/internal/extensions"
	"github.com/oujinhaoai/lantai/internal/ledger"
)

// packageSource bridges T09 to the owners of package facts: catalog authorizes
// the importing administrator, storage returns verified bytes, and ledger
// holds the review. No SQL crosses modules; T09 keeps only references.
type packageSource struct{ a *App }

func (p packageSource) PackageVersion(ctx context.Context, who authz.Context, ref ids.PermanentRef) (extensions.PackageVersion, error) {
	v, err := p.a.Catalog.GetVersion(ctx, who, ref.AssetID, ref.VersionID)
	if err != nil {
		return extensions.PackageVersion{}, err
	}
	out := extensions.PackageVersion{Ref: v.Version.Ref(p.a.Instance.InstanceID()), ProjectID: v.Version.ProjectID, ManifestDigest: v.Version.ManifestDigest, AssetType: string(v.Manifest.Content.AssetType)}
	// lantai.asset-types/v1 "plugin" metadata names the package it carries.
	out.ExtensionID, _ = v.Manifest.Content.Metadata["extension_id"].(string)
	out.ExtensionVersion, _ = v.Manifest.Content.Metadata["extension_version"].(string)
	return out, nil
}

func (p packageSource) PackageFiles(ctx context.Context, ref ids.PermanentRef) (map[string][]byte, error) {
	if ref.InstanceID != p.a.Instance.InstanceID() {
		return nil, errcode.New(errcode.RefMismatch, "")
	}
	v, err := p.a.Ledger.Version(ctx, ref.AssetID, ref.VersionID)
	if err != nil {
		return nil, err
	}
	return p.a.Storage.ReadVersionFiles(ctx, v, extensions.MaxPackageBytes)
}

func (p packageSource) PackageReview(ctx context.Context, ref ids.PermanentRef) (extensions.PackageReview, error) {
	st, err := p.a.Ledger.VersionControl(ctx, ref.VersionID)
	if err != nil {
		return extensions.PackageReview{}, err
	}
	out := extensions.PackageReview{State: st.ReviewState}
	// Only an effective approval of an active, enabled version authorizes use.
	if st.ReviewState == "approved" && st.EffectiveReviewID.Valid() && st.Availability == "enabled" && st.Lifecycle == "active" && st.PendingOperationID == "" {
		out.Approved, out.ReviewID = true, st.EffectiveReviewID
	}
	return out, nil
}

func (a *App) assembleExtensions() error {
	i := a.Instance
	var err error
	a.ExtensionManager, err = extensions.NewManager(extensions.ManagerDeps{Registry: a.Extensions, Main: i.DB(ownership.Main), Runtime: i.DB(ownership.Runtime), Gate: i.Gate(), Authority: authority{a.Identity, i}, Packages: packageSource{a}, Policies: a.Identity, Clock: i.Clock(), IDs: i.IDs(), InstanceID: i.InstanceID()})
	return err
}

// extensionUses keeps enabled package versions out of the trash: reference
// retention is checked by ledger like any other current use.
func (a *App) extensionUses(ctx context.Context, refs []ids.PermanentRef) ([]ledger.LifecycleUse, error) {
	if a.ExtensionManager == nil {
		return nil, nil
	}
	uses, err := a.ExtensionManager.PackageUses(ctx, refs)
	if err != nil {
		return nil, err
	}
	out := []ledger.LifecycleUse{}
	for _, u := range uses {
		ref := u.Ref
		b, err := canonjson.CanonicalizeValue(u)
		if err != nil {
			return nil, err
		}
		out = append(out, ledger.LifecycleUse{Kind: "extension", ID: u.EnablementID, Revision: u.Revision, Ref: &ref, Digest: digest.Of(b)})
	}
	return out, nil
}

// lifecyclePorts adapts T03/T02 owners to the T08 scheduler. Every call goes
// through the owner's public method; nothing here decides deletion.
type lifecyclePorts struct{ a *App }

func (p lifecyclePorts) DueTrash(ctx context.Context, horizon time.Time, limit int) ([]operations.DueTrash, error) {
	rows, err := p.a.Lifecycle.DueTrash(ctx, horizon, limit)
	out := make([]operations.DueTrash, 0, len(rows))
	for _, r := range rows {
		out = append(out, operations.DueTrash{ProjectID: r.ProjectID, TrashID: r.TrashID, Revision: r.Revision, DueAt: r.DueAt, Reminded: r.Reminded})
	}
	return out, err
}

func (p lifecyclePorts) RunDue(ctx context.Context, cmd commands.Context, job operations.DueJob) error {
	_, err := p.a.Lifecycle.RunDueJob(ctx, cmd, ledger.DueTrashRequest{ProjectID: job.ProjectID, TrashID: job.TrashID, ExpectedRevision: job.ExpectedRevision}, p.a.Scheduler)
	return err
}

func (p lifecyclePorts) GCCandidates(ctx context.Context, after string, limit int) ([]operations.GCCandidate, error) {
	rows, err := p.a.Lifecycle.GCCandidates(ctx, after, limit)
	out := make([]operations.GCCandidate, 0, len(rows))
	for _, r := range rows {
		out = append(out, operations.GCCandidate{SHA256: r.SHA256, OperationID: r.OperationID, Since: r.Since})
	}
	return out, err
}

func (p lifecyclePorts) GCState(ctx context.Context, op ids.ID) (string, error) {
	return p.a.Storage.GCState(ctx, op)
}

func (p lifecyclePorts) CollectBlob(ctx context.Context, sha string) (bool, error) {
	return p.a.Storage.CollectBlob(ctx, sha, storage.GCDependencies{Authority: p.a.Lifecycle, CommitPins: p.a.Ledger, BackupPins: p.a.Instance})
}

func (p lifecyclePorts) PendingGC(ctx context.Context) ([]string, error) {
	rows, err := p.a.Storage.PendingGC(ctx)
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.SHA256)
	}
	return out, err
}

// RetryFileFailures replays failed purge/trash file phases through T03's own
// persisted intent; the operation keeps its original ID and plan.
func (p lifecyclePorts) RetryFileFailures(ctx context.Context) (int, error) {
	failures, err := p.a.Lifecycle.Failures(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	var first error
	for _, f := range failures {
		if _, err := p.a.Lifecycle.Resume(ctx, f.OperationID); err != nil {
			first = cmp.Or(first, err)
			continue
		}
		n++
	}
	return n, first
}

func (a *App) assembleScheduler() error {
	i := a.Instance
	var err error
	a.Scheduler, err = operations.NewLifecycleScheduler(operations.SchedulerDeps{Runtime: i.DB(ownership.Runtime), Gate: i.Gate(), Epochs: i, Ports: lifecyclePorts{a}, Clock: i.Clock(), IDs: i.IDs(), Instance: i.InstanceID(), Batch: i.Config().Lifecycle.Batch})
	return err
}

// RunLifecycle runs one scheduler pass (serve loop or local maintenance).
func (a *App) RunLifecycle(ctx context.Context) (operations.LifecycleReport, error) {
	return a.Scheduler.RunOnce(ctx)
}
