package integration

import (
	"encoding/json"
	"testing"

	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	"github.com/oujinhaoai/lantai/internal/extensions"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/ledger"
	"github.com/oujinhaoai/lantai/internal/storage"
)

// Only the fixture's base roles and clock are controlled. Checks, approvals,
// enablement retirement and publication use the assembled domain owners.
func TestM2BusinessCompletedCheckRetirementBeforeReview(t *testing.T) {
	for _, kind := range []string{"drain_disabled", "new_config_generation"} {
		t.Run(kind, func(t *testing.T) {
			f := newAppFlow(t)
			ctx := t.Context()
			builtin, err := f.app.Extensions.Producer(ctx, "org.lantai.corecheck.manifest")
			if err != nil {
				t.Fatal(err)
			}
			base := f.approvedProfile("profiles/bootstrap", profileDocument("bootstrap", builtin), ids.PermanentRef{})
			pkg, _ := f.approvedPackage(base, "plugins/business", "0.1.0")
			enabled := f.enablePackage(pkg, `{"mode":"business"}`, 1)
			f.sudo(&identity.SetPolicy{ProjectID: f.project.ProjectID, Key: "plugins.allowed", Value: json.RawMessage(`["org.example.business"]`)})
			producer := storage.Producer{ExtensionID: pkg.Manifest.ID, ExtensionVersion: pkg.Manifest.Version, PackageDigest: pkg.Digest, Source: "package", ContributionID: pkg.Manifest.ID + ".check"}
			profile := f.approvedProfile("profiles/media", businessProfile("media", producer, enabled.Enablement.ConfigDigest), base.Ref)
			v, flow := f.candidate("docs/retired", manifest.TypeDoc, map[string][]byte{"data.json": []byte(`{"valid":true}`)}, map[string]any{"synthetic_valid": true})
			j := f.checkCandidate(v, flow, producer.ContributionID)
			target := f.submitChecked(v, flow, profile.Ref, j, false)
			pubV, pubFlow := f.candidate("docs/publication", manifest.TypeDoc, map[string][]byte{"data.json": []byte(`{"valid":true}`)}, map[string]any{"synthetic_valid": true})
			pubJob := f.checkCandidate(pubV, pubFlow, producer.ContributionID)
			pubTarget := f.submitChecked(pubV, pubFlow, profile.Ref, pubJob, false)
			approved, err := f.recordDecision(f.reviewDecision(pubTarget, []ledger.ReviewWaiver{}))
			if err != nil {
				t.Fatal("current approval control", err)
			}
			request := ledger.PublishRequest{ProjectID: f.project.ProjectID, VersionID: pubV.VersionID, ReviewID: approved.ID, Action: "publish", Reason: "retired check must refuse a new publication"}
			unpublished, err := f.ledger.AssetControl(ctx, pubV.AssetID)
			if err != nil || unpublished.PublishedVersionID != "" {
				t.Fatal("publication target must be unpublished", err)
			}
			var invocation string
			if err = f.inst.DB(ownership.Runtime).QueryRowContext(ctx, `SELECT record FROM extensions_invocations WHERE invocation_id=?`, j.Attempt.ID).Scan(&invocation); err != nil {
				t.Fatal(err)
			}
			t.Log("completed invocation before retirement", invocation)
			before, err := f.source.Evidence(ctx, f.owner, j.Evidence[4].EvidenceID)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "drain_disabled":
				req := extensions.DisableRequest{EnablementID: enabled.Enablement.ID, Mode: "drain", Reason: "completed check retirement"}
				a, err := f.app.ExtensionManager.DisableHumanAction(ctx, req)
				if err != nil {
					t.Fatal(err)
				}
				who, grant, op := f.grant(a)
				if _, err = f.app.ExtensionManager.Disable(ctx, who, req, grant, op, f.id); err != nil {
					t.Fatal(err)
				}
			case "new_config_generation":
				next := f.enablePackage(pkg, `{"mode":"business","sleep_ms":0}`, 2)
				if next.Enablement.Generation == enabled.Enablement.Generation {
					t.Fatal("generation unchanged")
				}
			}
			if _, err = f.recordDecision(f.reviewDecision(target, []ledger.ReviewWaiver{})); errcode.CodeOf(err) != errcode.ExtensionActivationStale {
				t.Fatal("retired check allowed new approval", err)
			}
			if _, err = f.reviews.Publish(ctx, f.owner, f.key(), request); errcode.CodeOf(err) != errcode.ExtensionActivationStale {
				t.Fatal("retired check allowed new publication", err)
			}
			stillUnpublished, err := f.ledger.AssetControl(ctx, pubV.AssetID)
			if err != nil || rawJSON(unpublished) != rawJSON(stillUnpublished) {
				t.Fatal("rejected publication changed asset", err)
			}
			after, err := f.source.Evidence(ctx, f.owner, before.ID)
			if err != nil || rawJSON(before) != rawJSON(after) {
				t.Fatal("historical evidence changed or unreadable", err)
			}
			if _, err = f.reviews.Target(ctx, f.owner, pubTarget.ID); err != nil {
				t.Fatal("approved historical target unreadable", err)
			}
			captureGovernanceEvidence(t, "retirement-observation.json", []byte(rawJSON(map[string]any{"kind": kind, "job": j, "approved_review": approved, "invocation": json.RawMessage(invocation), "controlled_clock": f.clk.Now(), "history_before": before, "history_after": after})))
		})
	}
}
