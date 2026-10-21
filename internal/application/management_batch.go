package application

import (
	"context"
	"encoding/json"
	"path"
	"strings"
	"time"

	"github.com/oujinhaoai/lantai/internal/catalog"
	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/catalog/pathrule"
	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/operations"
)

// Management records are immutable config versions, not a second approval or
// job ledger. This adapter recomputes a bounded, point-in-time acceptance view
// from current owner ports; a saved checkbox never authorizes an operation.
type batchRef struct {
	AssetID   ids.ID `json:"asset_id"`
	VersionID ids.ID `json:"version_id"`
}
type batchEnvelope struct {
	Schema     string          `json:"schema"`
	Kind       string          `json:"kind"`
	InstanceID ids.ID          `json:"instance_id"`
	ProjectID  ids.ID          `json:"project_id"`
	Payload    json.RawMessage `json:"payload"`
}
type batchAsset struct {
	batchRef
	Key            string        `json:"key"`
	ManifestDigest digest.Digest `json:"manifest_digest"`
	Status         string        `json:"status"`
}
type migrationBatch struct {
	Schema      string    `json:"schema"`
	BatchID     string    `json:"batch_id"`
	Revision    int64     `json:"revision"`
	PreviousRef *batchRef `json:"previous_ref,omitempty"`
	InstanceID  ids.ID    `json:"instance_id"`
	ProjectID   ids.ID    `json:"project_id"`
	OwnerID     ids.ID    `json:"owner_id"`
	LastAction  struct {
		ActorID ids.ID    `json:"actor_id"`
		At      time.Time `json:"at"`
		Action  string    `json:"action"`
	} `json:"last_action"`
	Stage           string        `json:"stage"`
	Acceptance      string        `json:"acceptance"`
	SourceRetained  bool          `json:"source_retained"`
	ReferenceScope  string        `json:"reference_scope"`
	IntendedPurpose authz.Purpose `json:"intended_purpose"`
	ImportSummary   struct {
		Planned            int `json:"planned"`
		Committed          int `json:"committed"`
		DescriptionPending int `json:"description_pending"`
		Failed             int `json:"failed"`
		Unknown            int `json:"unknown"`
		NotAttempted       int `json:"not_attempted"`
	} `json:"import_summary"`
	Evidence struct {
		Assets              []batchAsset `json:"assets"`
		Worksets            []batchRef   `json:"worksets"`
		Verifications       []batchRef   `json:"verifications"`
		Recovery            []batchRef   `json:"recovery"`
		NativeContinuations []batchRef   `json:"native_continuations,omitempty"`
		ReferenceJobs       []struct {
			JobID  ids.ID `json:"job_id"`
			Action string `json:"action"`
		} `json:"reference_jobs"`
	} `json:"evidence"`
	Checklist map[string]bool `json:"checklist"`
}
type batchCheck struct {
	Key    string `json:"key"`
	Status string `json:"status"`
	Code   string `json:"code,omitempty"`
}
type batchCheckView struct {
	Schema          string        `json:"schema"`
	InstanceID      ids.ID        `json:"instance_id"`
	RecordRef       batchRef      `json:"record_ref"`
	BatchID         string        `json:"batch_id"`
	Revision        int64         `json:"revision"`
	CheckedAt       time.Time     `json:"checked_at"`
	Acceptance      string        `json:"acceptance"`
	IntendedPurpose authz.Purpose `json:"intended_purpose"`
	Checks          []batchCheck  `json:"checks"`
	Assets          []batchAsset  `json:"assets"`
	Worksets        []batchRef    `json:"worksets"`
	RecoveryRefs    []batchRef    `json:"recovery_refs"`
}

func batchInvalid() error             { return errcode.New(errcode.SchemaInvalid, "invalid management record") }
func validBatchRef(ref batchRef) bool { return ref.AssetID.Valid() && ref.VersionID.Valid() }

type batchBudgetKey struct{}
type batchReadBudget struct {
	bytes    int64
	files    int
	metadata int
}

func reserveBatchManifest(ctx context.Context, v catalog.VersionInfo) error {
	b, ok := ctx.Value(batchBudgetKey{}).(*batchReadBudget)
	if !ok || len(v.Manifest.Content.Files) > 10000-b.files {
		return errcode.New(errcode.QuotaExceeded, "batch exceeds total acceptance file budget")
	}
	raw, err := json.Marshal(v.Manifest)
	if err != nil || len(raw) > (16<<20)-b.metadata {
		return errcode.New(errcode.QuotaExceeded, "batch exceeds total acceptance manifest budget")
	}
	b.files += len(v.Manifest.Content.Files)
	b.metadata += len(raw)
	return nil
}

func consumeBatchBudget(ctx context.Context, bytes int64) error {
	b, ok := ctx.Value(batchBudgetKey{}).(*batchReadBudget)
	if !ok || bytes < 0 || bytes > (256<<20)-b.bytes {
		return errcode.New(errcode.QuotaExceeded, "batch exceeds total acceptance read budget")
	}
	b.bytes += bytes
	return nil
}

func (a *App) readBatchRecord(ctx context.Context, who authz.Context, ref batchRef, kind string, project ids.ID) (batchEnvelope, catalog.VersionInfo, error) {
	var env batchEnvelope
	if !validBatchRef(ref) {
		return env, catalog.VersionInfo{}, batchInvalid()
	}
	v, err := a.Catalog.ExecutionVersion(ctx, who, ref.AssetID, ref.VersionID)
	if err != nil {
		return env, v, err
	}
	r := v.Manifest.Content.Rights
	if v.Manifest.Content.AssetType != manifest.TypeConfig || len(v.Manifest.Content.Files) != 1 || v.Manifest.Content.Files[0].Path != "record.json" || v.Manifest.Content.Files[0].Role != "record" || v.Manifest.Content.Files[0].Size > 2<<20 || r.Usage != "reference" || r.License != "LicenseRef-Lantai-Private-Record" || r.RedistributeRaw || !r.NoAI || (project != "" && project != v.Version.ProjectID) {
		return env, v, batchInvalid()
	}
	if err := consumeBatchBudget(ctx, v.Manifest.Content.Files[0].Size); err != nil {
		return env, v, err
	}
	if err := reserveBatchManifest(ctx, v); err != nil {
		return env, v, err
	}
	files, err := a.Storage.ReadVersionFiles(ctx, v.Version, 2<<20)
	if err != nil {
		return env, v, err
	}
	b := files["record.json"]
	if canonjson.Validate(b) != nil || json.Unmarshal(b, &env) != nil || env.Schema != "lantai.management-record/v1" || env.Kind != kind || env.InstanceID != a.Instance.InstanceID() || env.ProjectID != v.Version.ProjectID {
		return env, v, batchInvalid()
	}
	return env, v, nil
}

// CheckManagementBatch returns JSON to the transport without making the
// transport own domain state. Current security is held throughout all reads.
func (a *App) CheckManagementBatch(ctx context.Context, who authz.Context, assetID, versionID ids.ID) (json.RawMessage, error) {
	ctx, held, err := a.Instance.Gate().Coordinator().Acquire(ctx, commands.Request{Security: commands.ModeShared})
	if err != nil {
		return nil, err
	}
	defer held.Release()
	ctx = context.WithValue(ctx, batchBudgetKey{}, &batchReadBudget{})
	ref := batchRef{assetID, versionID}
	env, record, err := a.readBatchRecord(ctx, who, ref, "migration", "")
	if err != nil {
		return nil, err
	}
	var b migrationBatch
	if json.Unmarshal(env.Payload, &b) != nil || b.Schema != "lantai.migration-batch/v1" || b.ProjectID != env.ProjectID || b.InstanceID != env.InstanceID || b.BatchID == "" || len(b.BatchID) > 128 || b.Revision < 1 {
		return nil, batchInvalid()
	}
	out := batchCheckView{Schema: "lantai.batch-check/v1", InstanceID: env.InstanceID, RecordRef: ref, BatchID: b.BatchID, Revision: b.Revision, CheckedAt: a.Instance.Clock().Now(), Acceptance: "blocked", IntendedPurpose: b.IntendedPurpose, Checks: []batchCheck{}, Assets: []batchAsset{}, Worksets: []batchRef{}, RecoveryRefs: []batchRef{}}
	check := func(key, status, code string) { out.Checks = append(out.Checks, batchCheck{key, status, code}) }
	// Only the current record version can be accepted. Revisions bind the same
	// asset and previous fixed version; historical records remain readable.
	// The record was authorized above under this security guard. Use the
	// ledger's owner read port instead of reacquiring catalog's security lock.
	latest, e := a.Ledger.LatestVersion(ctx, assetID)
	recordOK := e == nil && latest.VersionID == versionID && b.OwnerID.Valid() && b.LastAction.ActorID == record.Version.CommittedBy && !b.LastAction.At.IsZero() && !b.LastAction.At.After(out.CheckedAt.Add(time.Minute)) && b.LastAction.Action != "" && b.SourceRetained
	recordCode := "record_identity_revision_or_owner_mismatch"
	if e != nil {
		recordCode = "current_record_unavailable"
	} else if latest.VersionID != versionID {
		recordCode = "historical_record_revision"
	} else if b.LastAction.ActorID != record.Version.CommittedBy {
		recordCode = "last_actor_mismatch"
	} else if b.LastAction.At.IsZero() || b.LastAction.At.After(out.CheckedAt.Add(time.Minute)) {
		recordCode = "last_action_time_invalid"
	}
	if b.Revision == 1 {
		recordOK = recordOK && b.PreviousRef == nil
	} else if b.PreviousRef == nil || b.PreviousRef.AssetID != assetID {
		recordOK = false
	} else {
		p, pv, pe := a.readBatchRecord(ctx, who, *b.PreviousRef, "migration", env.ProjectID)
		var previous migrationBatch
		next, ne := a.Ledger.AssetVersions(ctx, assetID, pv.Version.VersionNumber, 2)
		recordOK = recordOK && pe == nil && ne == nil && len(next) == 1 && next[0].VersionID == versionID && json.Unmarshal(p.Payload, &previous) == nil && previous.Schema == b.Schema && previous.BatchID == b.BatchID && previous.ProjectID == b.ProjectID && previous.InstanceID == b.InstanceID && previous.Revision == b.Revision-1 && pv.Version.VersionNumber < record.Version.VersionNumber
	}
	members, _, me := a.Identity.Members(ctx, who, env.ProjectID)
	ownerExists := false
	for _, m := range members {
		if m.PrincipalID == b.OwnerID && m.Kind == authz.Human {
			ownerExists = true
		}
	}
	recordOK = recordOK && me == nil && ownerExists && (b.Acceptance == "requested" || b.Acceptance == "not_requested")
	stages := map[string]bool{"inventory": true, "ingest": true, "verification": true, "workset": true, "references": true, "recovery": true, "acceptance": true, "in_use": true, "rollback": true}
	recordOK = recordOK && stages[b.Stage]
	if recordOK {
		check("record", "passed", "")
	} else {
		check("record", "failed", recordCode)
	}

	// Bound both object count and total bytes before touching content. A batch
	// larger than this explicit acceptance budget must be split, never partial.
	counts := b.ImportSummary
	assetsOK := counts.Planned > 0 && counts.Planned <= 256 && counts.Committed == counts.Planned && counts.DescriptionPending == 0 && counts.Failed == 0 && counts.Unknown == 0 && counts.NotAttempted == 0 && len(b.Evidence.Assets) == counts.Planned
	versions := map[batchRef]catalog.VersionInfo{}
	var total int64
	loadVersion := func(r batchRef) (catalog.VersionInfo, error) {
		if v, ok := versions[r]; ok {
			return v, nil
		}
		if !validBatchRef(r) || len(versions) >= 256 {
			return catalog.VersionInfo{}, batchInvalid()
		}
		v, ve := a.Catalog.ExecutionVersion(ctx, who, r.AssetID, r.VersionID)
		if ve != nil {
			return v, ve
		}
		if v.Version.ProjectID != env.ProjectID {
			return v, batchInvalid()
		}
		if be := reserveBatchManifest(ctx, v); be != nil {
			return v, be
		}
		for _, f := range v.Manifest.Content.Files {
			if f.Size < 0 || f.Size > (256<<20)-total {
				return v, errcode.New(errcode.QuotaExceeded, "batch content exceeds acceptance budget")
			}
			total += f.Size
			if be := consumeBatchBudget(ctx, f.Size); be != nil {
				return v, be
			}
		}
		_, ve = a.Storage.ReadVersionFiles(ctx, v.Version, 256<<20)
		if ve == nil {
			versions[r] = v
		}
		return v, ve
	}
	if len(b.Evidence.Assets) > 256 {
		assetsOK = false
	} else {
		keys := map[string]bool{}
		for _, row := range b.Evidence.Assets {
			_, duplicate := versions[row.batchRef]
			v, ve := loadVersion(row.batchRef)
			if ve != nil || duplicate || row.Status != "committed" || row.Key == "" || keys[row.Key] || row.ManifestDigest != v.Version.ManifestDigest {
				assetsOK = false
				continue
			}
			keys[row.Key] = true
			out.Assets = append(out.Assets, row)
		}
	}
	if assetsOK {
		check("assets", "passed", "")
	} else {
		check("assets", "failed", "incomplete_import_or_current_content_mismatch")
	}
	assetSet := map[batchRef]catalog.VersionInfo{}
	for _, row := range out.Assets {
		assetSet[row.batchRef] = versions[row.batchRef]
	}
	purposeOK := false
	for _, p := range []authz.Purpose{authz.PurposeArchiveReview, authz.PurposeReference, authz.PurposeProduction, authz.PurposeGenerativeInput, authz.PurposeRawExport} {
		if p == b.IntendedPurpose {
			purposeOK = true
		}
	}
	for r := range assetSet {
		decision, pe := a.Rights.EvaluateUse(ctx, who, ids.PermanentRef{InstanceID: env.InstanceID, AssetID: r.AssetID, VersionID: r.VersionID}, b.IntendedPurpose)
		if pe != nil || !decision.Allowed {
			purposeOK = false
		}
	}
	if purposeOK && len(assetSet) > 0 {
		check("purpose", "passed", "")
	} else {
		check("purpose", "failed", "intended_use_not_currently_authorized")
	}

	worksetsOK := len(b.Evidence.Worksets) > 0 && len(b.Evidence.Worksets) <= 64
	nativeRequired := false
	for _, v := range assetSet {
		if v.Manifest.Content.AssetType == manifest.TypeScene || v.Manifest.Content.AssetType == manifest.TypeProduction || v.Manifest.Content.AssetType == manifest.TypeModel || v.Manifest.Content.AssetType == manifest.TypeMotion {
			nativeRequired = true
		}
		for _, file := range v.Manifest.Content.Files {
			switch strings.ToLower(path.Ext(file.Path)) {
			case ".blend", ".c4d", ".psd", ".aep", ".nk", ".spp", ".sbs", ".hip", ".max", ".ma", ".mb", ".uasset", ".unity":
				nativeRequired = true
			case ".json":
				if v.Manifest.Content.AssetType == manifest.TypeConfig {
					nativeRequired = true
				}
			}
		}
	}
	nativeWorksets := map[batchRef]nativeWorkset{}
	coverage := map[batchRef]bool{}
	seenRecords := map[batchRef]bool{}
	graphEdges := 0
	for _, r := range boundedBatchRefs(b.Evidence.Worksets) {
		we, _, weErr := a.readBatchRecord(ctx, who, r, "workset", env.ProjectID)
		var w struct {
			Schema string `json:"schema"`
			Input  struct {
				Schema  string `json:"schema"`
				Entries []struct {
					batchRef
					Destination string `json:"destination"`
				} `json:"entries"`
				Entrypoints  []string `json:"entrypoints"`
				Dependencies []struct {
					From     string   `json:"from"`
					Requires []string `json:"requires"`
				} `json:"dependencies"`
			} `json:"input"`
		}
		if weErr != nil || seenRecords[r] || json.Unmarshal(we.Payload, &w) != nil || w.Schema != "lantai.managed-workset/v1" || w.Input.Schema != "lantai.client-workset/v1" || len(w.Input.Entries) == 0 || len(w.Input.Entries) > 256 {
			worksetsOK = false
			continue
		}
		seenRecords[r] = true
		nativeInputs := nativeWorkset{paths: map[string]batchRef{}, relative: map[string]string{}, entries: map[batchRef]catalog.VersionInfo{}, entrypoints: w.Input.Entrypoints}
		paths := map[string]bool{}
		destinations := []string{}
		for _, entry := range w.Input.Entries {
			v, ok := assetSet[entry.batchRef]
			if !ok || !safeBatchPath(entry.Destination) {
				worksetsOK = false
				continue
			}
			coverage[entry.batchRef] = true
			nativeInputs.entries[entry.batchRef] = v
			destinations = append(destinations, entry.Destination)
			for _, file := range v.Manifest.Content.Files {
				if len(paths) >= 10000 {
					worksetsOK = false
					break
				}
				p := path.Join(entry.Destination, file.Path)
				if paths[p] {
					worksetsOK = false
				}
				paths[p] = true
				nativeInputs.paths[p] = entry.batchRef
				nativeInputs.relative[p] = file.Path
			}
		}
		allPaths := make([]string, 0, len(paths))
		for p := range paths {
			allPaths = append(allPaths, p)
		}
		if len(allPaths) > 10000 || batchPathConflicts(allPaths) || batchPathConflicts(destinations) {
			worksetsOK = false
		}
		for _, entrypoint := range w.Input.Entrypoints {
			if !safeBatchPath(entrypoint) || !paths[entrypoint] {
				worksetsOK = false
			}
			// Opening/editing a production entrypoint requires independent native
			// continuation evidence, which these read-only adapters do not invent.
			nativeRequired = true
		}
		if len(w.Input.Dependencies) > 10000 {
			worksetsOK = false
		} else {
			for _, dependency := range w.Input.Dependencies {
				if !safeBatchPath(dependency.From) || !paths[dependency.From] || len(dependency.Requires) == 0 || len(dependency.Requires) > 10000-graphEdges {
					worksetsOK = false
					continue
				}
				graphEdges += len(dependency.Requires)
				seenDependencies := map[string]bool{}
				for _, required := range dependency.Requires {
					if !safeBatchPath(required) || !paths[required] || seenDependencies[required] {
						worksetsOK = false
					}
					seenDependencies[required] = true
				}
			}
		}
		nativeWorksets[r] = nativeInputs
		out.Worksets = append(out.Worksets, r)
	}
	for r := range assetSet {
		if !coverage[r] {
			worksetsOK = false
		}
	}
	if worksetsOK {
		check("worksets", "passed", "")
	} else {
		check("worksets", "failed", "missing_invalid_or_uncovered_fixed_workset")
	}
	nativeStatus, nativeCode := a.checkBatchNative(ctx, who, b, nativeWorksets, nativeRequired, loadVersion)
	check("native_continuation", nativeStatus, nativeCode)

	verificationOK := len(b.Evidence.Verifications) > 0 && len(b.Evidence.Verifications) <= 64
	verified := map[batchRef]bool{}
	for _, r := range boundedBatchRefs(b.Evidence.Verifications) {
		ve, _, veErr := a.readBatchRecord(ctx, who, r, "migration", env.ProjectID)
		var v struct {
			Schema        string              `json:"schema"`
			BatchID       string              `json:"batch_id"`
			InstanceID    ids.ID              `json:"instance_id"`
			ProjectID     ids.ID              `json:"project_id"`
			Verifications []batchVerification `json:"verifications"`
		}
		if veErr != nil || seenRecords[r] || json.Unmarshal(ve.Payload, &v) != nil || v.Schema != "lantai.migration-verification-record/v1" || v.BatchID != b.BatchID || v.InstanceID != env.InstanceID || v.ProjectID != env.ProjectID || len(v.Verifications) == 0 || len(v.Verifications) > 256 {
			verificationOK = false
			continue
		}
		seenRecords[r] = true
		for _, receipt := range v.Verifications {
			if !validBatchVerification(receipt, env.InstanceID, assetSet, out.CheckedAt) {
				verificationOK = false
				continue
			}
			verified[receipt.batchRef] = true
		}
	}
	for r := range assetSet {
		if !verified[r] {
			verificationOK = false
		}
	}
	if verificationOK {
		check("verifications", "passed", "")
	} else {
		check("verifications", "failed", "missing_or_mismatched_current_file_evidence")
	}

	// A restore record is accepted only against a current core-owned receipt.
	// Permission to inspect that administrative receipt is checked separately.
	recoveryOK := len(b.Evidence.Recovery) > 0 && len(b.Evidence.Recovery) <= 64
	recoveryUnknown := false
	d, de := a.Identity.Authorize(ctx, who, identity.ActBackupRead, authz.Resource{})
	info, ie := a.Instance.ManagementInfo(ctx)
	if de != nil || !d.Allowed || ie != nil || info.State != operations.StateReady || info.RecoveryError != "" || info.RecoveryPartial {
		recoveryOK = false
	}
	for _, r := range boundedBatchRefs(b.Evidence.Recovery) {
		re, _, reErr := a.readBatchRecord(ctx, who, r, "recovery", env.ProjectID)
		var recovery struct {
			Schema        string                             `json:"schema"`
			InstanceID    ids.ID                             `json:"instance_id"`
			Epoch         int64                              `json:"current_recovery_epoch"`
			Restore       operations.CompletedRestoreReceipt `json:"restore"`
			Verifications []batchVerification                `json:"verifications"`
			Unknown       bool                               `json:"unknown_after_capture_remains_unknown"`
		}
		if reErr != nil || seenRecords[r] || json.Unmarshal(re.Payload, &recovery) != nil || recovery.Schema != "lantai.recovery-evidence/v1" || recovery.InstanceID != env.InstanceID || recovery.Epoch != info.RecoveryEpoch {
			recoveryOK = false
			continue
		}
		seenRecords[r] = true
		matched := false
		for _, core := range info.RecoveryReceipts {
			if core == recovery.Restore {
				matched = true
			}
		}
		if !matched {
			recoveryOK = false
		}
		covered := map[batchRef]bool{}
		for _, receipt := range recovery.Verifications {
			if !validBatchVerification(receipt, env.InstanceID, assetSet, out.CheckedAt) {
				recoveryOK = false
				continue
			}
			covered[receipt.batchRef] = true
		}
		for r := range assetSet {
			if !covered[r] {
				recoveryOK = false
			}
		}
		recoveryUnknown = recoveryUnknown || recovery.Unknown || recovery.Restore.UnknownAfterCapture
		out.RecoveryRefs = append(out.RecoveryRefs, r)
	}
	if !recoveryOK {
		check("recovery", "failed", "missing_unavailable_or_unmatched_core_restore_evidence")
	} else if recoveryUnknown {
		check("recovery", "unknown", "after_capture_unknown_requires_reconciliation")
	} else {
		check("recovery", "passed", "")
	}

	// Device execution evidence is checked through the jobs owner (implemented
	// alongside the device protocol). Absent evidence never passes by checkbox.
	referenceStatus, referenceCode := a.checkBatchReferences(ctx, who, b, assetSet)
	check("references", referenceStatus, referenceCode)
	allChecked := b.SourceRetained
	for _, key := range []string{"excluded_backed_up", "working_inputs_backed_up", "entrypoints_opened", "new_results_captured", "fallback_ready", "selected_notes_reviewed"} {
		allChecked = allChecked && b.Checklist[key]
	}
	if allChecked {
		check("checklist", "passed", "responsibility_declarations_only")
	} else {
		check("checklist", "missing", "responsibility_checklist_incomplete")
	}
	if b.Acceptance != "requested" {
		check("request", "pending", "acceptance_not_requested")
	}
	ready := true
	for _, c := range out.Checks {
		ready = ready && c.Status == "passed"
	}
	if ready {
		out.Acceptance = "ready"
	}
	return json.Marshal(out)
}

func boundedBatchRefs(refs []batchRef) []batchRef {
	if len(refs) > 64 {
		return nil
	}
	return refs
}
func safeBatchPath(p string) bool {
	return p != "" && len(p) <= 4096 && pathrule.Check(p) == nil && !strings.ContainsAny(p, "\\\x00:") && !strings.HasPrefix(p, "/") && path.Clean(p) == p && p != "." && p != ".." && !strings.HasPrefix(p, "../")
}
func batchPathConflicts(paths []string) bool {
	if len(pathrule.Conflicts(paths)) > 0 {
		return true
	}
	seen := map[string]string{}
	for _, p := range paths {
		parts := strings.Split(p, "/")
		for i := range parts {
			prefix := strings.Join(parts[:i+1], "/")
			key := pathrule.Key(prefix)
			if old, ok := seen[key]; ok && old != prefix {
				return true
			}
			seen[key] = prefix
		}
	}
	return false
}

type batchVerification struct {
	batchRef
	Schema         string          `json:"schema"`
	InstanceID     ids.ID          `json:"instance_id"`
	ManifestDigest digest.Digest   `json:"manifest_digest"`
	VerifiedAt     time.Time       `json:"verified_at"`
	Files          []manifest.File `json:"files"`
}

func validBatchVerification(v batchVerification, instance ids.ID, assets map[batchRef]catalog.VersionInfo, now time.Time) bool {
	actual, ok := assets[v.batchRef]
	if !ok || v.Schema != "lantai.browser-verification/v1" || v.InstanceID != instance || v.ManifestDigest != actual.Version.ManifestDigest || v.VerifiedAt.IsZero() || v.VerifiedAt.After(now.Add(time.Minute)) || len(v.Files) != len(actual.Manifest.Content.Files) {
		return false
	}
	files := map[string]manifest.File{}
	for _, f := range actual.Manifest.Content.Files {
		files[f.Path] = f
	}
	for _, f := range v.Files {
		expected, exists := files[f.Path]
		if !exists || expected != f {
			return false
		}
		delete(files, f.Path)
	}
	return len(files) == 0
}

type batchLocalResultRef struct {
	Schema       string `json:"schema"`
	JobID        ids.ID `json:"job_id"`
	ResultSHA256 string `json:"result_sha256"`
}

func (a *App) checkBatchReferences(ctx context.Context, who authz.Context, b migrationBatch, assets map[batchRef]catalog.VersionInfo) (string, string) {
	if b.ReferenceScope == "none" && len(b.Evidence.ReferenceJobs) == 0 {
		return "passed", "no_selected_notes_declared"
	}
	if b.ReferenceScope != "selected_notes" || len(b.Evidence.ReferenceJobs) < 2 || len(b.Evidence.ReferenceJobs) > 64 {
		return "missing", "selected_notes_require_original_plan_and_copy_jobs"
	}
	type observed struct {
		ID          ids.ID
		Input       json.RawMessage
		Result      batchLocalResultRef
		PostCutover []string
	}
	observations := map[string]observed{}
	seen := map[ids.ID]bool{}
	var deviceID ids.ID
	var workspaceBinding string
	for _, selected := range b.Evidence.ReferenceJobs {
		if !selected.JobID.Valid() || seen[selected.JobID] || (selected.Action != "references.plan" && selected.Action != "references.copy" && selected.Action != "references.rollback") {
			return "failed", "invalid_or_duplicate_reference_job"
		}
		seen[selected.JobID] = true
		j, err := a.DeviceJob(ctx, who, selected.JobID)
		if err != nil || j.ProjectID != b.ProjectID {
			return "failed", "reference_job_not_currently_authorized"
		}
		if j.State == "needs_reconciliation" || j.State == "cancelling" || j.CancelRequested {
			return "unknown", "reference_job_effects_require_reconciliation"
		}
		if j.State == "failed" || j.State == "cancelled" {
			return "failed", "reference_job_did_not_complete"
		}
		if j.State != "succeeded" {
			return "pending", "reference_job_not_completed"
		}
		if !j.Stopped || j.Fence == nil || j.Fence.RecoveryEpoch != a.Instance.Marker().RecoveryEpoch || j.Inspection == nil || j.Inspection.ApprovalDigest != j.ApprovalDigest || j.Inspection.JobDigest != j.JobDigest || j.Inspection.WorkspaceBinding != j.WorkspaceBinding {
			return "unknown", "reference_job_binding_or_recovery_epoch_stale"
		}
		if deviceID == "" {
			deviceID = j.DeviceID
			workspaceBinding = j.WorkspaceBinding
		}
		if deviceID != j.DeviceID || workspaceBinding != j.WorkspaceBinding {
			return "failed", "reference_jobs_use_different_workspaces"
		}
		var job struct {
			JobID       string          `json:"job_id"`
			Action      string          `json:"action"`
			Input       json.RawMessage `json:"input"`
			PostCutover []string        `json:"post_cutover_refs"`
		}
		var result struct {
			Schema         string              `json:"schema"`
			JobID          string              `json:"job_id"`
			Action         string              `json:"action"`
			JobDigest      string              `json:"job_digest"`
			ApprovalDigest string              `json:"approval_digest"`
			Status         string              `json:"status"`
			ExitCode       int                 `json:"exit_code"`
			Result         batchLocalResultRef `json:"result"`
		}
		if json.Unmarshal(j.Job, &job) != nil || json.Unmarshal(j.Result, &result) != nil || job.Action != selected.Action || job.JobID != "references-"+b.BatchID+"-"+strings.TrimPrefix(selected.Action, "references.") || result.Schema != "lantai.local-assistant-result/v1" || result.JobID != job.JobID || result.Action != job.Action || result.JobDigest != j.JobDigest || result.ApprovalDigest != j.ApprovalDigest || result.Status != "completed" || result.ExitCode != 0 || result.Result.Schema != "lantai.local-assistant-result-ref/v1" || result.Result.JobID != j.ID || !digest.ValidHex(result.Result.ResultSHA256) {
			return "failed", "reference_result_is_not_bound_to_original_job"
		}
		if _, duplicate := observations[job.Action]; duplicate {
			return "failed", "ambiguous_reference_action_history"
		}
		observations[job.Action] = observed{j.ID, job.Input, result.Result, job.PostCutover}
	}
	plan, hasPlan := observations["references.plan"]
	copy, hasCopy := observations["references.copy"]
	if !hasPlan || !hasCopy {
		return "missing", "selected_notes_require_plan_and_copy"
	}
	var input struct {
		Schema   string   `json:"schema"`
		Notes    []string `json:"notes"`
		Mappings []struct {
			FixedRef string `json:"fixed_ref"`
		} `json:"mappings"`
	}
	if json.Unmarshal(plan.Input, &input) != nil || input.Schema != "lantai.reference-input/v1" || len(input.Notes) == 0 || len(input.Mappings) == 0 {
		return "failed", "reference_plan_does_not_fix_selected_notes_and_versions"
	}
	for _, note := range input.Notes {
		if !safeBatchPath(note) {
			return "failed", "reference_note_path_invalid"
		}
	}
	for _, mapping := range input.Mappings {
		ref, err := ids.ParseURI(mapping.FixedRef)
		if err != nil || ref.InstanceID != b.InstanceID {
			return "failed", "reference_mapping_is_not_a_fixed_batch_version"
		}
		if _, ok := assets[batchRef{ref.AssetID, ref.VersionID}]; !ok {
			return "failed", "reference_mapping_is_outside_fixed_batch"
		}
	}
	var copyInput batchLocalResultRef
	if json.Unmarshal(copy.Input, &copyInput) != nil || copyInput != plan.Result {
		return "failed", "reference_copy_does_not_use_original_plan"
	}
	if rollback, exists := observations["references.rollback"]; exists {
		var rollbackInput batchLocalResultRef
		if json.Unmarshal(rollback.Input, &rollbackInput) != nil || rollbackInput != plan.Result || rollback.PostCutover == nil {
			return "failed", "rollback_does_not_preserve_original_plan_and_inventory"
		}
		for _, raw := range rollback.PostCutover {
			ref, err := ids.ParseURI(raw)
			if err != nil || ref.InstanceID != b.InstanceID {
				return "failed", "rollback_inventory_contains_unfixed_reference"
			}
			version, err := a.Catalog.ExecutionVersion(ctx, who, ref.AssetID, ref.VersionID)
			if err != nil || version.Version.ProjectID != b.ProjectID {
				return "failed", "rollback_inventory_not_currently_authorized"
			}
		}
	} else if b.Stage == "rollback" {
		return "missing", "rollback_stage_requires_original_rollback_job"
	}
	return "passed", "original_device_jobs_completed_for_explicit_selected_notes"
}
