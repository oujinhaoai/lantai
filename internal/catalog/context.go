package catalog

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/catalog/pathrule"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/schema"
	"github.com/oujinhaoai/lantai/internal/storage"
)

const ProjectDocumentContract = "lantai.project-document/v1"
const MaxContextBytes = 1 << 20

type DocumentScope struct {
	Kind      string             `json:"kind"`
	AssetType manifest.AssetType `json:"asset_type,omitempty"`
}

// ProjectDocument is immutable metadata inside a doc asset's manifest. Approval
// is intentionally absent: it is returned only from T03's authoritative receipt.
type ProjectDocument struct {
	Contract   string             `json:"contract"`
	Kind       string             `json:"kind"`
	Title      string             `json:"title"`
	Scope      DocumentScope      `json:"scope"`
	FilePath   string             `json:"file_path"`
	Supersedes []ids.PermanentRef `json:"supersedes"`
}

func (d ProjectDocument) Validate() error {
	raw, err := json.Marshal(d)
	if err != nil {
		return err
	}
	doc, err := canonjson.Decode(raw)
	if err != nil {
		return err
	}
	reg, err := schema.Default()
	if err != nil {
		return err
	}
	if err = reg.Validate(ProjectDocumentContract, doc); err != nil {
		return errcode.Wrap(errcode.SchemaInvalid, "invalid project document", err)
	}
	if !strings.HasSuffix(d.FilePath, ".md") {
		return invalid("context content must be Markdown")
	}
	return pathrule.Check(d.FilePath)
}

// ContextApproval is a T03 review/effectivity receipt, never a claim made by a
// document author. Historical receipts survive replacement; Current is queried.
type ContextApproval struct {
	Ref            ids.PermanentRef `json:"ref"`
	ManifestDigest digest.Digest    `json:"manifest_digest"`
	ReviewID       ids.ID           `json:"review_id"`
	ApproverID     ids.ID           `json:"approver_id"`
	EffectiveAt    string           `json:"effective_at"`
	Revision       int64            `json:"revision"`
}

// ContextReviews must return a consistent current approved set, applying
// supersession and scope rules in T03. It must not derive approvals from index.db.
type ContextReviews interface {
	EffectiveContext(context.Context, authz.Context, ids.ID, manifest.AssetType) ([]ContextApproval, error)
	ContextReview(context.Context, authz.Context, ids.PermanentRef) (ContextApproval, error)
}

type ContextDocument struct {
	Ref            ids.PermanentRef `json:"ref"`
	ManifestDigest digest.Digest    `json:"manifest_digest"`
	Spec           ProjectDocument  `json:"spec"`
	Markdown       string           `json:"markdown"`
	Approval       *ContextApproval `json:"approval,omitempty"`
}

// CommitProjectDocument uses ordinary upload/prepare/install/commit. A new
// committed draft does not update the effective context set.
func (s *Service) CommitProjectDocument(ctx context.Context, req VersionRequest, spec ProjectDocument) (VersionResult, error) {
	if err := spec.Validate(); err != nil {
		return VersionResult{}, err
	}
	if req.Content.AssetType != manifest.TypeDoc {
		return VersionResult{}, invalid("context must be a doc asset")
	}
	found := false
	for _, f := range req.Content.Files {
		if f.Path == spec.FilePath {
			found = true
			if f.Size > MaxContextBytes {
				return VersionResult{}, invalid("context exceeds size limit")
			}
		}
	}
	if !found {
		return VersionResult{}, invalid("context Markdown is not in the frozen files")
	}
	for _, ref := range spec.Supersedes {
		if _, err := s.GetContextDocument(ctx, req.Who, ref, nil); err != nil {
			return VersionResult{}, err
		}
	}
	req.Content.Metadata = maps.Clone(req.Content.Metadata)
	if req.Content.Metadata == nil {
		req.Content.Metadata = map[string]any{}
	}
	raw, _ := json.Marshal(spec)
	var value any
	_ = json.Unmarshal(raw, &value)
	req.Content.Metadata["project_document"] = value
	return s.CommitVersion(ctx, req)
}

// GetContextDocument returns an exact version, including historical/replaced
// documents. Passing no review adapter explicitly returns content without approval.
func (s *Service) GetContextDocument(ctx context.Context, who authz.Context, ref ids.PermanentRef, reviews ContextReviews) (ContextDocument, error) {
	var out ContextDocument
	if ref.InstanceID != s.instance || !ref.AssetID.Valid() || !ref.VersionID.Valid() {
		return out, invalid("local permanent context reference required")
	}
	v, err := s.GetVersion(ctx, who, ref.AssetID, ref.VersionID)
	if err != nil {
		return out, err
	}
	if v.Manifest.Content.AssetType != manifest.TypeDoc {
		return out, invalid("context target is not a document")
	}
	raw, err := json.Marshal(v.Manifest.Content.Metadata["project_document"])
	if err != nil {
		return out, err
	}
	var spec ProjectDocument
	if err = json.Unmarshal(raw, &spec); err != nil {
		return out, invalid("invalid context metadata")
	}
	if err = spec.Validate(); err != nil {
		return out, err
	}
	var file *manifest.File
	for _, f := range v.Manifest.Content.Files {
		if f.Path == spec.FilePath {
			copy := f
			file = &copy
		}
	}
	if file == nil || file.Size > MaxContextBytes {
		return out, invalid("context file absent or too large")
	}
	g, err := s.storage.IssueReadGrant(ctx, storage.ReadRequest{Who: who, AssetID: ref.AssetID, VersionID: ref.VersionID, Path: spec.FilePath, Purpose: authz.PurposeArchiveReview})
	if err != nil {
		return out, err
	}
	location, err := url.Parse(g.URL)
	if err != nil {
		return out, err
	}
	h, err := s.storage.OpenRead(ctx, who, g.GrantID, location.Query().Get("sig"))
	if err != nil {
		return out, err
	}
	defer h.Close()
	body, err := io.ReadAll(io.LimitReader(h.File, MaxContextBytes+1))
	if err != nil {
		return out, err
	}
	if int64(len(body)) != file.Size || len(body) > MaxContextBytes || !utf8.Valid(body) || digest.Of(body).Hex() != file.SHA256 {
		return out, errcode.New(errcode.HashMismatch, "context Markdown content differs")
	}
	out = ContextDocument{Ref: ref, ManifestDigest: v.Version.ManifestDigest, Spec: spec, Markdown: string(body)}
	if reviews != nil {
		a, e := reviews.ContextReview(ctx, who, ref)
		if e != nil {
			return out, e
		}
		if e = checkContextApproval(out, a, s.now().UnixMilli()); e != nil {
			return out, e
		}
		out.Approval = &a
	}
	return out, nil
}
func checkContextApproval(d ContextDocument, a ContextApproval, now int64) error {
	at, err := clock.Parse(a.EffectiveAt)
	if err != nil || at.UnixMilli() > now || a.Ref != d.Ref || a.ManifestDigest != d.ManifestDigest || !a.ReviewID.Valid() || !a.ApproverID.Valid() || a.Revision < 1 {
		return errcode.New(errcode.PreconditionFailed, "context approval does not match this version")
	}
	return nil
}

type ContextBundle struct {
	ProjectID ids.ID            `json:"project_id"`
	Documents []ContextDocument `json:"documents"`
	// Digest includes the exact content and approval identities used by T05/T07.
	Digest digest.Digest `json:"digest"`
}

func (s *Service) EffectiveContext(ctx context.Context, who authz.Context, project ids.ID, assetType manifest.AssetType, reviews ContextReviews) (ContextBundle, error) {
	out := ContextBundle{ProjectID: project, Documents: []ContextDocument{}}
	if err := s.canRead(ctx, who, project, "project", project); err != nil {
		return out, err
	}
	if reviews == nil {
		return out, errcode.New(errcode.InvalidStateTransition, "T03 context review adapter is not configured")
	}
	approvals, err := reviews.EffectiveContext(ctx, who, project, assetType)
	if err != nil {
		return out, err
	}
	if len(approvals) > 100 {
		return out, invalid("context set exceeds limit")
	}
	seen := map[ids.ID]bool{}
	total := 0
	for _, a := range approvals {
		if seen[a.Ref.AssetID] {
			return out, invalid("duplicate effective context asset")
		}
		seen[a.Ref.AssetID] = true
		d, e := s.GetContextDocument(ctx, who, a.Ref, nil)
		if e != nil {
			return out, e
		}
		if e = checkContextApproval(d, a, s.now().UnixMilli()); e != nil {
			return out, e
		}
		v, e := s.ledger.Version(ctx, a.Ref.AssetID, a.Ref.VersionID)
		if e != nil {
			return out, e
		}
		if d.Spec.Scope.Kind != "global" && v.ProjectID != project {
			return out, errcode.New(errcode.RefMismatch, "context belongs to another project")
		}
		if d.Spec.Scope.Kind == "asset_type" && d.Spec.Scope.AssetType != assetType {
			return out, errcode.New(errcode.RefMismatch, "context asset type scope differs")
		}
		total += len(d.Markdown)
		if total > 4*MaxContextBytes {
			return out, invalid("context bundle exceeds byte limit")
		}
		d.Approval = &a
		out.Documents = append(out.Documents, d)
	}
	// Immutable content can be read without retaining locks across I/O. Recheck
	// the complete authoritative set at the return boundary, so a concurrent
	// withdrawal, disable or permission change cannot leave a stale bundle.
	current, err := reviews.EffectiveContext(ctx, who, project, assetType)
	if err != nil {
		return out, err
	}
	before, err := canonjson.CanonicalizeValue(approvals)
	if err != nil {
		return out, err
	}
	after, err := canonjson.CanonicalizeValue(current)
	if err != nil {
		return out, err
	}
	if string(before) != string(after) {
		return out, errcode.New(errcode.PreconditionFailed, "context approvals changed during read")
	}
	raw, err := canonjson.CanonicalizeValue(out.Documents)
	if err != nil {
		return out, err
	}
	out.Digest = digest.Of(raw)
	return out, nil
}
