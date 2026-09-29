package agent_execution

import (
	"context"
	"github.com/oujinhaoai/lantai/internal/catalog"
	ae "github.com/oujinhaoai/lantai/internal/contract/agentexec"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/storage"
	"slices"
)

// CatalogAssets verifies candidates against already committed immutable content.
// Workspaces and caller-supplied server paths are never accepted here.
type CandidateEvidence interface {
	AcceptedEvidenceRecord(context.Context, authz.Context, ids.ID) (storage.Record, digest.Digest, error)
}
type CandidateFiles interface {
	VerifyDeep(context.Context, ids.ID) error
}
type CatalogAssets struct {
	Files      CandidateFiles
	Evidence   CandidateEvidence
	Catalog    *catalog.Service
	InstanceID ids.ID
}

func (a CatalogAssets) CheckExecutionRef(ctx context.Context, who authz.Context, ref ids.PermanentRef) error {
	if ref.Validate(true) != nil || ref.InstanceID != a.InstanceID {
		return errcode.New(errcode.RefMismatch, "")
	}
	_, e := a.Catalog.ExecutionVersion(ctx, who, ref.AssetID, ref.VersionID)
	return e
}
func (a CatalogAssets) CheckCandidate(ctx context.Context, who authz.Context, ref ids.PermanentRef, c ae.ArtifactCandidate) error {
	if e := a.CheckExecutionRef(ctx, who, ref); e != nil {
		return e
	}
	v, e := a.Catalog.ExecutionVersion(ctx, who, ref.AssetID, ref.VersionID)
	if e != nil {
		return e
	}
	if v.Version.CommittedBy != who.PrincipalID || v.Version.ManifestDigest != c.ManifestDigest || len(v.Manifest.Content.Files) != len(c.Files) {
		return errcode.New(errcode.RefMismatch, "candidate differs from committed content")
	}
	if v.Manifest.Content.Producer == nil {
		if c.Producer != (ae.Producer{}) {
			return errcode.New(errcode.RefMismatch, "candidate invents a producer")
		}
	} else if encoded(c.Producer) != encoded(v.Manifest.Content.Producer) {
		return errcode.New(errcode.RefMismatch, "candidate producer differs")
	}
	provenance := []ids.ID{}
	for _, u := range v.Manifest.Content.Uses {
		provenance = append(provenance, u.VersionID)
	}
	for _, id := range c.ProvenanceRefs {
		if !slices.Contains(provenance, id) {
			return errcode.New(errcode.RefMismatch, "unknown provenance reference")
		}
	}
	for _, id := range c.LicenseEvidenceRefs {
		if a.Evidence == nil {
			return errcode.New(errcode.UnsupportedCapability, "license evidence owner unavailable")
		}
		record, _, e := a.Evidence.AcceptedEvidenceRecord(ctx, who, id)
		if e != nil {
			return e
		}
		if record.AssetID != ref.AssetID || record.VersionID != ref.VersionID || record.ManifestDigest != c.ManifestDigest {
			return errcode.New(errcode.RefMismatch, "license evidence belongs to another candidate")
		}
	}
	// Producer and rights remain the accepted manifest's authority. A candidate
	// can only describe content, never add an approval or invented provenance.
	if c.ValidationState != "pending" {
		return errcode.New(errcode.Forbidden, "candidate validation is not caller authority")
	}
	for _, f := range c.Files {
		found := false
		for _, m := range v.Manifest.Content.Files {
			if f.Path == m.Path && f.Size == m.Size && f.SHA256 == m.SHA256 {
				found = true
				break
			}
		}
		if !found {
			return errcode.New(errcode.HashMismatch, "")
		}
	}
	if a.Files == nil {
		return errcode.New(errcode.UnsupportedCapability, "candidate file verifier unavailable")
	}
	return a.Files.VerifyDeep(ctx, v.Version.OperationID)
}
