package application

import (
	"context"
	"slices"
	"strings"

	"github.com/oujinhaoai/lantai/internal/catalog"
	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/pin"
	"github.com/oujinhaoai/lantai/internal/ledger"
	"github.com/oujinhaoai/lantai/internal/provenance"
	"github.com/oujinhaoai/lantai/internal/storage"
)

// FSCKReport separates authoritative integrity failures from preserved residual
// work. Only Findings reject startup/backup; Residuals require reconciliation but
// are safe to capture without inventing committed business facts.
type FSCKReport struct {
	Storage    storage.Inventory            `json:"storage"`
	Catalog    catalog.FileReport           `json:"catalog"`
	Ledger     ledger.RecoveryInventory     `json:"ledger"`
	Provenance provenance.RecoveryInventory `json:"provenance"`
	Findings   []storage.Finding            `json:"findings"`
	Residuals  []storage.Finding            `json:"residuals"`
}

func (r FSCKReport) Err() error {
	if len(r.Findings) == 0 {
		return nil
	}
	return errcode.New(errcode.OperationNeedsReconciliation, "authoritative files failed integrity verification")
}

// FSCK only uses owner APIs. Without the maintenance barrier it is a diagnostic
// view; backup/startup callers hold the common barrier for a consistent point.
func (a *App) FSCK(ctx context.Context, deep bool) (FSCKReport, error) {
	var out FSCKReport
	var err error
	out.Ledger, err = a.Ledger.RecoveryInventory(ctx)
	if err != nil {
		return out, err
	}
	out.Storage, err = a.Storage.Inventory(ctx, deep)
	if err != nil {
		return out, err
	}
	out.Catalog, err = a.Catalog.CheckFiles(ctx, out.Ledger.Metadata)
	if err != nil {
		return out, err
	}
	out.Provenance, err = a.Rights.RecoveryInventory(ctx)
	if err != nil {
		return out, err
	}
	committed := map[ids.ID]commit.Committed{}
	for _, v := range out.Ledger.Versions {
		committed[v.OperationID] = v
	}
	accepted := map[ids.ID]bool{}
	acceptedRecords := map[ids.ID]storage.RecordEntry{}
	for _, r := range out.Provenance.Records {
		accepted[r.OperationID] = true
		acceptedRecords[r.RecordID] = r
	}
	for _, r := range out.Ledger.Records {
		accepted[r.OperationID] = true
		acceptedRecords[r.RecordID] = r
	}
	checks, residuals, err := a.Ledger.CheckEvidenceFiles(ctx, a.Storage, out.Ledger.Records)
	if err != nil {
		return out, err
	}
	out.Findings = append(out.Findings, checks...)
	out.Residuals = append(out.Residuals, residuals...)
	for _, f := range out.Storage.Findings {
		_, final := committed[f.OperationID]
		soft := (f.Reason == "install_verification" || f.Reason == "referenced_blob_missing_or_size") && !final
		soft = soft || (slices.Contains([]string{"record_unreadable", "record_invalid", "record_location_mismatch"}, f.Reason) && !accepted[f.OperationID])
		soft = soft || strings.HasPrefix(f.Reason, "upload_")
		soft = soft || f.Reason == "lifecycle_pending" || f.Reason == "record_lifecycle_pending"
		if soft {
			out.Residuals = append(out.Residuals, f)
		} else {
			out.Findings = append(out.Findings, f)
		}
	}
	for _, f := range out.Catalog.Findings {
		if slices.Contains([]string{"snapshot_stale", "alias_missing_or_unsafe", "frozen_digest_or_identity"}, f.Reason) {
			out.Residuals = append(out.Residuals, f)
		} else {
			out.Findings = append(out.Findings, f)
		}
	}
	out.Findings = append(out.Findings, out.Provenance.Findings...)
	out.Residuals = append(out.Residuals, out.Provenance.Residuals...)
	installs := map[ids.ID]storage.InstallEntry{}
	for _, i := range out.Storage.Installs {
		installs[i.OperationID] = i
	}
	add := func(reason string, id ids.ID) {
		out.Findings = append(out.Findings, storage.Finding{Code: errcode.OperationNeedsReconciliation, Reason: reason, OperationID: id})
	}
	for _, v := range out.Ledger.Versions {
		i, ok := installs[v.OperationID]
		if !ok || i.State != "installed" || i.Proof == nil {
			add("committed_install_missing", v.OperationID)
			continue
		}
		p := i.Proof
		pd, e := p.Digest()
		if e != nil || p.AssetID != v.AssetID || p.ProjectID != v.ProjectID || p.VersionID != v.VersionID || p.VersionNumber != v.VersionNumber || p.ManifestDigest != v.ManifestDigest || pd != v.ProofDigest {
			add("committed_install_mismatch", v.OperationID)
			continue
		}
		// Catalog's authority files carry identity as well as the content digest.
		location, e := a.Ledger.VersionFileLocation(ctx, v.AssetID, v.VersionID)
		if e != nil {
			add("committed_location_invalid", v.OperationID)
			continue
		}
		if location.Purged {
			continue
		}
		if location.PendingOperationID != "" {
			out.Residuals = append(out.Residuals, storage.Finding{Code: errcode.OperationNeedsReconciliation, Reason: "committed_lifecycle_pending", OperationID: location.PendingOperationID})
			continue
		}
		b, e := a.Storage.ReadManifest(ctx, v)
		if e != nil {
			add("committed_manifest_unreadable", v.OperationID)
			continue
		}
		d, e := manifest.Parse(b)
		if e != nil || d.InstanceID != a.Instance.InstanceID() || d.ProjectID != v.ProjectID || d.AssetID != v.AssetID || d.VersionID != v.VersionID || d.OperationID != v.OperationID || d.VersionNumber != v.VersionNumber || d.ManifestDigest != v.ManifestDigest || d.CreatedBy != v.CommittedBy || !slices.Equal(d.Content.InstallFiles(), p.Files) {
			add("committed_manifest_mismatch", v.OperationID)
			continue
		}
		for _, use := range d.Content.Uses {
			_, e = a.Ledger.Version(ctx, use.AssetID, use.VersionID)
			if use.InstanceID != a.Instance.InstanceID() || e != nil {
				add("committed_source_missing", v.OperationID)
			}
		}
	}
	for _, o := range out.Storage.Orphans {
		out.Residuals = append(out.Residuals, storage.Finding{Code: errcode.OperationNeedsReconciliation, Reason: o.Reason, Ref: o.Ref, OperationID: o.OperationID})
	}
	for _, r := range out.Storage.Records {
		if expected, ok := acceptedRecords[r.RecordID]; !ok || expected.OperationID != r.OperationID || expected.Digest != r.Digest {
			out.Residuals = append(out.Residuals, storage.Finding{Code: errcode.OperationNeedsReconciliation, Reason: "unaccepted_record", Ref: r.Ref, OperationID: r.OperationID})
		}
	}
	return out, nil
}

type RecoveryIssue struct {
	Module      string       `json:"module"`
	OperationID ids.ID       `json:"operation_id"`
	Code        errcode.Code `json:"code"`
}
type RecoveryReport struct {
	Findings            []storage.Finding    `json:"findings"`
	Resumed             []ids.ID             `json:"resumed"`
	NeedsReconciliation []RecoveryIssue      `json:"needs_reconciliation"`
	Pins                storage.PinReport    `json:"pins"`
	Snapshots           catalog.RepairReport `json:"snapshots"`
	Aliases             catalog.RepairReport `json:"aliases"`
}

// Recover is a maintenance-only dispatcher. A durable command's original
// session must still verify against identity, and its actor and epoch must agree.
// Expired/old authority stays pending with its commit pins, without new IDs or
// epoch substitution. A final deep FSCK rejects damaged committed authority.
func (a *App) Recover(ctx context.Context) (RecoveryReport, error) {
	var out RecoveryReport
	if err := a.Instance.Gate().RequireMaintenance(ctx); err != nil {
		return out, err
	}
	inv, err := a.Ledger.RecoveryInventory(ctx)
	if err != nil {
		return out, err
	}
	issue := func(module string, id ids.ID, err error) {
		out.NeedsReconciliation = append(out.NeedsReconciliation, RecoveryIssue{Module: module, OperationID: id, Code: errcode.CodeOf(err)})
	}
	for _, r := range inv.Open {
		if err = ctx.Err(); err != nil {
			return out, err
		}
		if r.Operation.Stage == commands.StageQuarantined {
			issue("ledger", r.Operation.OperationID, errcode.New(errcode.OperationNeedsReconciliation, ""))
			continue
		}
		if r.LifecyclePlan != nil {
			if _, e := a.Ledger.ResumeLifecycle(ctx, r.Operation.OperationID, a.Storage, a.Catalog); e != nil {
				issue("ledger", r.Operation.OperationID, e)
			} else {
				out.Resumed = append(out.Resumed, r.Operation.OperationID)
			}
			continue
		}
		who, e := a.originalSession(ctx, r.Command.SessionID, r.Command.ActorID, r.Command.RecoveryEpoch)
		if e != nil {
			issue("ledger", r.Operation.OperationID, e)
			continue
		}
		switch {
		case r.PreparedVersion != nil:
			_, e = a.Catalog.RecoverVersion(ctx, who, *r.PreparedVersion)
		case r.PreparedMetadata != nil:
			_, e = a.Catalog.RecoverMetadata(ctx, who, *r.PreparedMetadata)
		default:
			e = errcode.New(errcode.OperationNeedsReconciliation, "unknown durable intent")
		}
		if e != nil {
			issue("ledger", r.Operation.OperationID, e)
		} else {
			out.Resumed = append(out.Resumed, r.Operation.OperationID)
		}
	}
	pv, err := a.Rights.RecoveryInventory(ctx)
	if err != nil {
		return out, err
	}
	for _, r := range pv.Open {
		if err = ctx.Err(); err != nil {
			return out, err
		}
		who, e := a.originalSession(ctx, r.Record.SessionID, r.Record.AuthorID, r.Operation.RecoveryEpoch)
		if e == nil {
			_, e = a.Rights.RecoverOperation(ctx, who, r.Operation.OperationID)
		}
		if e != nil {
			issue("provenance", r.Operation.OperationID, e)
		} else {
			out.Resumed = append(out.Resumed, r.Operation.OperationID)
		}
	}
	out.Pins, err = a.Storage.ReconcilePins(ctx)
	if err != nil {
		return out, err
	}
	inv, err = a.Ledger.RecoveryInventory(ctx)
	if err != nil {
		return out, err
	}
	out.Snapshots, err = a.Catalog.RepairSnapshots(ctx, inv.Metadata)
	if err != nil {
		return out, err
	}
	out.Aliases, err = a.Catalog.RepairAliasHistory(ctx)
	if err != nil {
		return out, err
	}
	check, err := a.FSCK(ctx, true)
	if err != nil {
		return out, err
	}
	out.Findings = check.Findings
	return out, check.Err()
}

func (a *App) originalSession(ctx context.Context, session, actor ids.ID, epoch int64) (authz.Context, error) {
	who, err := a.Identity.VerifySession(ctx, session)
	if err != nil {
		return who, err
	}
	if who.PrincipalID != actor || who.SessionID != session || who.RecoveryEpoch != epoch {
		return authz.Context{}, errcode.New(errcode.OperationNeedsReconciliation, "original accepting authority no longer matches")
	}
	return who, nil
}

// BlobRetention queries each retention owner; any failed source is an error,
// never evidence that the bytes are unreferenced. M1 does not delete CAS data.
func (a *App) BlobRetention(ctx context.Context, sha string) ([]pin.Pin, error) {
	var out []pin.Pin
	for _, source := range []pin.Source{a.Storage, a.Ledger, a.Instance} {
		p, err := source.PinsFor(ctx, sha)
		if err != nil {
			return nil, err
		}
		out = append(out, p...)
	}
	return out, nil
}
