package provenance

import (
	"context"
	"slices"
	"strings"

	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

// IncomingUse is an immutable source edge identity. It contains no guessed
// license conclusion. Only a trusted lifecycle caller may enumerate all sources;
// an end-user view must separately authorize each source before disclosing it.
type IncomingUse struct {
	Source               ids.PermanentRef `json:"source"`
	SourceProjectID      ids.ID           `json:"source_project_id"`
	SourceManifestDigest digest.Digest    `json:"source_manifest_digest"`
	SourceRightsRevision int64            `json:"source_rights_revision"`
	Target               ids.PermanentRef `json:"target"`
	Relation             string           `json:"relation"`
}

// IncomingUses checks authority rather than an eventually consistent reverse
// index. A missing/corrupt live manifest is pending, never an empty reference set.
// The caller holds the security writer guard through accepting a deletion ban.
func (s *Service) IncomingUses(ctx context.Context, targets []ids.PermanentRef) ([]IncomingUse, error) {
	selected := map[ids.PermanentRef]bool{}
	for _, ref := range targets {
		if ref.Validate(true) != nil || ref.InstanceID != s.InstanceID || selected[ref] {
			return nil, errcode.New(errcode.SchemaInvalid, "unique local permanent targets required")
		}
		selected[ref] = true
	}
	if len(selected) == 0 {
		return nil, errcode.New(errcode.SchemaInvalid, "incoming reference target set required")
	}
	out := []IncomingUse{}
	var after ids.ID
	for {
		versions, err := s.Reader.Versions(ctx, after, 500)
		if err != nil {
			return nil, err
		}
		if len(versions) == 0 {
			break
		}
		for _, v := range versions {
			if v.VersionID <= after {
				return nil, errcode.New(errcode.OperationNeedsReconciliation, "version enumeration did not advance")
			}
			after = v.VersionID
			if roots, ok := s.Reader.(commit.ReferenceRoots); ok {
				live, err := roots.RetainsReferences(ctx, v.AssetID, v.VersionID)
				if err != nil {
					return nil, err
				}
				if !live {
					continue
				}
			}
			// Sources removed in the same exact selection do not leave a live dangling edge.
			if selected[v.Ref(s.InstanceID)] {
				continue
			}
			_, doc, err := s.read(ctx, v.Ref(s.InstanceID))
			if err != nil {
				return nil, errcode.New(errcode.RightsPending, "live dependency source cannot be verified")
			}
			state, err := s.assertionState(ctx, v, doc)
			if err != nil {
				return nil, errcode.New(errcode.RightsPending, "live effective dependency source cannot be verified")
			}
			for _, use := range state.Uses {
				if use.Relation != "uses" && use.Relation != "derived_from" {
					continue
				}
				target := ids.PermanentRef{InstanceID: use.InstanceID, AssetID: use.AssetID, VersionID: use.VersionID}
				if selected[target] {
					out = append(out, IncomingUse{Source: v.Ref(s.InstanceID), SourceProjectID: v.ProjectID, SourceManifestDigest: v.ManifestDigest, SourceRightsRevision: state.Revision, Target: target, Relation: use.Relation})
				}
			}
		}
	}
	slices.SortFunc(out, func(a, b IncomingUse) int {
		if a.Source.VersionID != b.Source.VersionID {
			return strings.Compare(string(a.Source.VersionID), string(b.Source.VersionID))
		}
		if a.Target.VersionID != b.Target.VersionID {
			return strings.Compare(string(a.Target.VersionID), string(b.Target.VersionID))
		}
		return strings.Compare(a.Relation, b.Relation)
	})
	return out, nil
}
