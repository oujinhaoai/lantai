package application

import (
	"context"
	"encoding/json"
	"path"
	"strings"
	"time"

	"github.com/oujinhaoai/lantai/internal/catalog"
	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/schema"
	"github.com/oujinhaoai/lantai/internal/identity"
)

// This is a human attestation, reviewed by T03 with an exact T01 HumanGrant.
// File hashes and current approval are core facts; DCC execution is NOT.
type nativeContinuation struct {
	Schema        string        `json:"schema"`
	InstanceID    ids.ID        `json:"instance_id"`
	ProjectID     ids.ID        `json:"project_id"`
	BatchID       string        `json:"batch_id"`
	RecoveryEpoch int64         `json:"recovery_epoch"`
	Workset       batchRef      `json:"workset_ref"`
	Purpose       authz.Purpose `json:"intended_purpose"`
	Basis         string        `json:"evidence_basis"`
	OperatorID    ids.ID        `json:"operator_id"`
	Application   struct {
		Name     string `json:"name"`
		Version  string `json:"version"`
		Platform string `json:"platform"`
	} `json:"application"`
	StartedAt   time.Time `json:"started_at"`
	FinishedAt  time.Time `json:"finished_at"`
	Entrypoints []string  `json:"entrypoints"`
	Returns     []struct {
		Base           batchRef      `json:"base"`
		Result         batchRef      `json:"result"`
		ManifestDigest digest.Digest `json:"manifest_digest"`
	} `json:"returns"`
	SupportingEvidence []batchAsset `json:"supporting_evidence"`
	Outcome            string       `json:"outcome"`
	Unresolved         []string     `json:"unresolved"`
}

type nativeWorkset struct {
	paths       map[string]batchRef
	relative    map[string]string
	entries     map[batchRef]catalog.VersionInfo
	entrypoints []string
}

func nativeVersion(v catalog.VersionInfo) bool {
	switch v.Manifest.Content.AssetType {
	case manifest.TypeScene, manifest.TypeProduction, manifest.TypeModel, manifest.TypeMotion:
		return true
	}
	for _, f := range v.Manifest.Content.Files {
		switch strings.ToLower(path.Ext(f.Path)) {
		case ".blend", ".c4d", ".psd", ".aep", ".nk", ".spp", ".sbs", ".hip", ".max", ".ma", ".mb", ".uasset", ".unity":
			return true
		case ".json":
			if v.Manifest.Content.AssetType == manifest.TypeConfig {
				return true
			}
		}
	}
	return false
}

// The caller holds the shared security guard and a global bounded read budget.
// An ordinary uploaded JSON/checkbox never supplies authority: the exact file
// must have a current T03 approval, a qualified human reviewer under current T03 policy and native profile.
func (a *App) checkBatchNative(ctx context.Context, who authz.Context, b migrationBatch, worksets map[batchRef]nativeWorkset, required bool, load func(batchRef) (catalog.VersionInfo, error)) (string, string) {
	if !required {
		if len(b.Evidence.NativeContinuations) > 0 {
			return "failed", "unexpected_native_evidence"
		}
		return "passed", "no_declared_native_entrypoint"
	}
	if len(b.Evidence.NativeContinuations) == 0 {
		return "unknown", "native_open_edit_return_not_verified"
	}
	if len(b.Evidence.NativeContinuations) > 64 {
		return "failed", "native_evidence_budget_exceeded"
	}
	epoch, err := a.Instance.RecoveryEpoch(ctx)
	if err != nil {
		return "failed", "native_current_epoch_unavailable"
	}
	members, err := a.Identity.ProjectRecipients(ctx, b.ProjectID)
	if err != nil {
		return "failed", "native_current_members_unavailable"
	}
	humans := map[ids.ID]bool{}
	reviewers := map[ids.ID]bool{}
	for _, m := range members {
		if m.Kind == authz.Human {
			humans[m.PrincipalID] = true
			for _, r := range m.Roles {
				if r == identity.RoleOwner || r == identity.RoleReviewer {
					reviewers[m.PrincipalID] = true
				}
			}
		}
	}
	covered := map[batchRef]bool{}
	seen := map[batchRef]bool{}
	for _, ref := range b.Evidence.NativeContinuations {
		if seen[ref] {
			return "failed", "duplicate_native_evidence"
		}
		seen[ref] = true
		v, e := load(ref)
		if e != nil || v.Manifest.Content.AssetType != manifest.TypeConfig || len(v.Manifest.Content.Files) != 1 || v.Manifest.Content.Files[0].Path != "native-continuation.json" || v.Manifest.Content.Files[0].Role != "record" || v.Manifest.Content.Files[0].Size > 2<<20 || !v.Manifest.Content.Rights.NoAI || v.Manifest.Content.Rights.RedistributeRaw {
			return "failed", "native_evidence_file_invalid"
		}
		if consumeBatchBudget(ctx, v.Manifest.Content.Files[0].Size) != nil {
			return "failed", "native_evidence_read_budget_exceeded"
		}
		files, e := a.Storage.ReadVersionFiles(ctx, v.Version, 2<<20)
		if e != nil {
			return "failed", "native_evidence_bytes_unavailable"
		}
		raw := files["native-continuation.json"]
		registry, e := schema.Default()
		if e != nil {
			return "failed", "native_schema_unavailable"
		}
		doc, e := canonjson.Decode(raw)
		if e != nil || registry.Validate("lantai.native-continuation/v1", doc) != nil {
			return "failed", "native_evidence_schema_invalid"
		}
		var n nativeContinuation
		if json.Unmarshal(raw, &n) != nil {
			return "failed", "native_evidence_schema_invalid"
		}
		w, ok := worksets[n.Workset]
		if !ok || covered[n.Workset] || n.Schema != "lantai.native-continuation/v1" || n.InstanceID != who.InstanceID || n.ProjectID != b.ProjectID || n.BatchID != b.BatchID || n.Purpose != b.IntendedPurpose || n.RecoveryEpoch != epoch || n.Basis != "human_attested" || !humans[n.OperatorID] || v.Version.CommittedBy != n.OperatorID {
			return "failed", "native_evidence_binding_mismatch"
		}
		if n.Outcome != "completed" || len(n.Unresolved) > 0 {
			return "unknown", "native_unresolved_requires_review"
		}
		if n.StartedAt.IsZero() || n.FinishedAt.Before(n.StartedAt) || n.FinishedAt.After(v.Version.CommittedAt) || n.FinishedAt.After(a.Instance.Clock().Now()) {
			return "failed", "native_evidence_time_invalid"
		}
		state, e := a.Ledger.VersionControl(ctx, ref.VersionID)
		if e != nil || state.ReviewState != "approved" || !state.EffectiveReviewID.Valid() {
			return "unknown", "native_human_review_required"
		}
		fact, e := a.Reviews.ReviewByID(ctx, state.EffectiveReviewID)
		if e != nil || fact.Review.Verdict != "approve" || fact.Review.VersionID != ref.VersionID || fact.Review.ManifestDigest != v.Version.ManifestDigest || fact.Target.VersionID != ref.VersionID || fact.Target.AssetID != ref.AssetID || fact.Target.ProjectID != b.ProjectID || !reviewers[fact.Review.ActorID] || !fact.Review.HumanGrantID.Valid() || fact.Target.Profile.Profile.ID != "native-continuation" || fact.Target.Profile.Profile.DistinctActorRule != "maker_checker" || fact.Target.Profile.Profile.Defaults.Publication != "manual" {
			return "failed", "native_current_human_review_invalid"
		}
		// Revalidate the exact profile and its current approval/evidence, including
		// rights/producer changes. No new approval state is stored by this adapter.
		if e = a.Reviews.ValidateEffectiveApproval(ctx, who, fact); e != nil {
			return "failed", "native_review_no_longer_applicable"
		}
		points := map[string]bool{}
		entryCoverage := map[batchRef]bool{}
		for _, p := range n.Entrypoints {
			r, exists := w.paths[p]
			if !exists || points[p] || !safeBatchPath(p) {
				return "failed", "native_entrypoint_mismatch"
			}
			points[p] = true
			entryCoverage[r] = true
		}
		for _, p := range w.entrypoints {
			if !points[p] {
				return "failed", "native_entrypoint_omitted"
			}
		}
		needed := map[batchRef]bool{}
		for r, input := range w.entries {
			if nativeVersion(input) || len(w.entrypoints) > 0 && entryCoverage[r] {
				needed[r] = true
				if !entryCoverage[r] {
					return "failed", "native_asset_entrypoint_omitted"
				}
			}
		}
		if len(points) == 0 || len(needed) == 0 {
			return "failed", "native_entrypoint_missing"
		}
		returned := map[batchRef]bool{}
		for _, ret := range n.Returns {
			base, exists := w.entries[ret.Base]
			result, e := load(ret.Result)
			if !exists || e != nil || base.Version.CommittedAt.After(n.StartedAt) || returned[ret.Base] || ret.Result.AssetID != ret.Base.AssetID || result.Version.ManifestDigest != ret.ManifestDigest || result.Version.VersionNumber <= base.Version.VersionNumber || result.Version.CommittedBy != n.OperatorID || result.Version.CommittedAt.Before(n.StartedAt) || result.Version.CommittedAt.After(n.FinishedAt) {
				return "failed", "native_return_binding_mismatch"
			}
			next, e := a.Ledger.AssetVersions(ctx, ret.Base.AssetID, base.Version.VersionNumber, 1)
			if e != nil || len(next) != 1 || next[0].VersionID != ret.Result.VersionID {
				return "failed", "native_return_not_next_committed_version"
			}
			for _, point := range n.Entrypoints {
				if w.paths[point] != ret.Base {
					continue
				}
				preserved := false
				for _, file := range result.Manifest.Content.Files {
					if file.Path == w.relative[point] {
						preserved = true
					}
				}
				if !preserved {
					return "failed", "native_return_entrypoint_removed"
				}
			}
			if !nativeContentChanged(base.Manifest.Content.Files, result.Manifest.Content.Files) {
				return "failed", "native_return_content_unchanged"
			}
			decision, e := a.Rights.EvaluateUse(ctx, who, result.Version.Ref(who.InstanceID), b.IntendedPurpose)
			if e != nil || !decision.Allowed {
				return "failed", "native_return_use_denied"
			}
			returned[ret.Base] = true
		}
		for r := range needed {
			if !returned[r] {
				return "failed", "native_return_missing"
			}
		}
		if len(n.SupportingEvidence) == 0 || len(n.SupportingEvidence) > 16 {
			return "failed", "native_supporting_evidence_missing"
		}
		supportSeen := map[batchRef]bool{}
		for _, support := range n.SupportingEvidence {
			sv, e := load(support.batchRef)
			if e != nil || supportSeen[support.batchRef] || support.Status != "committed" || support.ManifestDigest != sv.Version.ManifestDigest || support.batchRef == ref {
				return "failed", "native_supporting_evidence_invalid"
			}
			supportSeen[support.batchRef] = true
		}
		covered[n.Workset] = true
	}
	for r, w := range worksets {
		needs := len(w.entrypoints) > 0
		for _, v := range w.entries {
			needs = needs || nativeVersion(v)
		}
		if needs && !covered[r] {
			return "unknown", "native_workset_evidence_missing"
		}
	}
	return "passed", "current_human_attestation"
}

func nativeContentChanged(before, after []manifest.File) bool {
	old := map[string]manifest.File{}
	for _, f := range before {
		old[f.Path] = f
	}
	for _, f := range after {
		p, ok := old[f.Path]
		if ok && (p.SHA256 != f.SHA256 || p.Size != f.Size) {
			return true
		}
	}
	return false
}
