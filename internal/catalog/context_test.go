package catalog

import (
	"context"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

type contextReviewsStub struct{ current []ContextApproval }

func (r *contextReviewsStub) EffectiveContext(context.Context, authz.Context, ids.ID, manifest.AssetType) ([]ContextApproval, error) {
	return r.current, nil
}
func (r *contextReviewsStub) ContextReview(_ context.Context, _ authz.Context, ref ids.PermanentRef) (ContextApproval, error) {
	for _, a := range r.current {
		if a.Ref == ref {
			return a, nil
		}
	}
	return ContextApproval{}, errcode.New(errcode.NotFound, "")
}

func TestContextDraftEffectiveAndHistoricalVersions(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	spec := ProjectDocument{Contract: ProjectDocumentContract, Kind: "decision", Title: "Naming convention", Scope: DocumentScope{Kind: "project"}, FilePath: "rules.md", Supersedes: []ids.PermanentRef{}}
	commit := func(text string, asset, base ids.ID) VersionResult {
		data := map[string][]byte{"rules.md": []byte(text)}
		u := f.upload(f.agent, f.project.ProjectID, blobs(data)...)
		req := VersionRequest{Who: f.agent, IdempotencyKey: f.key(), UploadID: u.UploadID, Slug: "context/decisions/naming", AssetID: asset, BaseVersionID: base, Content: contentOf(manifest.TypeDoc, data)}
		if asset != "" {
			req.Slug = ""
		}
		res, err := f.svc.CommitProjectDocument(ctx, req, spec)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	first := commit("# Naming\nUse stable IDs.\n", "", "")
	draft, err := f.svc.GetContextDocument(ctx, f.agent, first.Ref, nil)
	if err != nil || draft.Approval != nil {
		t.Fatal(draft, err)
	}
	r := &contextReviewsStub{}
	empty, err := f.svc.EffectiveContext(ctx, f.agent, f.project.ProjectID, "", r)
	if err != nil || len(empty.Documents) != 0 {
		t.Fatal(empty, err)
	}
	approve := func(v VersionResult) ContextApproval {
		return ContextApproval{Ref: v.Ref, ManifestDigest: v.ManifestDigest, ReviewID: ids.New(), ApproverID: f.admin.PrincipalID, EffectiveAt: clock.Format(f.clk.Now()), Revision: 1}
	}
	r.current = []ContextApproval{approve(first)}
	before, err := f.svc.EffectiveContext(ctx, f.agent, f.project.ProjectID, "", r)
	if err != nil {
		t.Fatal(err)
	}
	changing := &changingContextReviews{contextReviewsStub: *r}
	_, err = f.svc.EffectiveContext(ctx, f.agent, f.project.ProjectID, "", changing)
	wantCode(t, err, errcode.PreconditionFailed)
	spec.Supersedes = []ids.PermanentRef{first.Ref}
	second := commit("# Naming\nUse stable IDs and explicit versions.\n", first.AssetID, first.VersionID)
	pending, err := f.svc.EffectiveContext(ctx, f.agent, f.project.ProjectID, "", r)
	if err != nil || pending.Digest != before.Digest {
		t.Fatal("draft changed effective context", pending, err)
	}
	r.current = []ContextApproval{approve(second)}
	after, err := f.svc.EffectiveContext(ctx, f.agent, f.project.ProjectID, "", r)
	if err != nil || after.Digest == before.Digest || after.Documents[0].Ref != second.Ref {
		t.Fatal(after, err)
	}
	old, err := f.svc.GetContextDocument(ctx, f.agent, first.Ref, nil)
	if err != nil || old.Markdown != draft.Markdown {
		t.Fatal("history lost", old, err)
	}
	r.current[0].ManifestDigest = digest.Of([]byte("wrong"))
	_, err = f.svc.EffectiveContext(ctx, f.agent, f.project.ProjectID, "", r)
	wantCode(t, err, errcode.PreconditionFailed)
	otherID := ids.New()
	f.az.AddPrincipal(otherID, authz.Agent)
	other := f.az.OpenSession(otherID, time.Hour)
	_, err = f.svc.GetContextDocument(ctx, other, first.Ref, nil)
	if err == nil {
		t.Fatal("unauthorized context read")
	}
}

func TestContextMetadataCannotClaimApproval(t *testing.T) {
	spec := ProjectDocument{Contract: ProjectDocumentContract, Kind: "context", Title: "Rules", Scope: DocumentScope{Kind: "asset_type"}, FilePath: "rules.md", Supersedes: []ids.PermanentRef{}}
	if spec.Validate() == nil {
		t.Fatal("asset type scope missing its type")
	}
	spec.Scope.AssetType = manifest.TypeImage
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}
	spec.FilePath = "../escape.md"
	if spec.Validate() == nil {
		t.Fatal("unsafe file path")
	}
}

type changingContextReviews struct {
	contextReviewsStub
	calls int
}

func (r *changingContextReviews) EffectiveContext(context.Context, authz.Context, ids.ID, manifest.AssetType) ([]ContextApproval, error) {
	r.calls++
	if r.calls > 1 {
		return []ContextApproval{}, nil
	}
	return r.current, nil
}
