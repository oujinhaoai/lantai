package commit

import (
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

// PostCommitCheckRule is absent by default. A distinct generation is required
// for each newly authorized enablement; restarts keep the same generation.
type PostCommitCheckRule struct {
	Generation ids.ID `json:"generation"`
	ProjectID  ids.ID `json:"project_id"`
	ActorID    ids.ID `json:"actor_id"`
}

func (r PostCommitCheckRule) Valid() bool {
	return r.Generation.Valid() && r.ProjectID.Valid() && r.ActorID.Valid()
}

func (r PostCommitCheckRule) Allows(who authz.Context) bool {
	return r.Valid() && who.PrincipalID == r.ActorID && who.PrincipalKind == authz.Human && who.DelegatedBy == "" && who.InScope(r.ProjectID)
}

type PostCommitCheckEnrollment struct {
	Rule           PostCommitCheckRule `json:"rule"`
	AssetID        ids.ID              `json:"asset_id"`
	VersionID      ids.ID              `json:"version_id"`
	ManifestDigest digest.Digest       `json:"manifest_digest"`
}

func (e PostCommitCheckEnrollment) Matches(v Committed) bool {
	return e.Rule.Valid() && e.AssetID.Valid() && e.VersionID.Valid() && e.ManifestDigest.Valid() &&
		e.Rule.ProjectID == v.ProjectID && e.Rule.ActorID == v.CommittedBy &&
		e.AssetID == v.AssetID && e.VersionID == v.VersionID && e.ManifestDigest == v.ManifestDigest
}
