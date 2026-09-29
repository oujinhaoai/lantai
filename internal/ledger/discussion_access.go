package ledger

import (
	"context"
	"errors"
	"slices"

	"github.com/oujinhaoai/lantai/internal/catalog"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/rights"
	"github.com/oujinhaoai/lantai/internal/identity"
)

type DiscussionCatalog interface {
	ReadProjectionVersion(context.Context, ids.ID, ids.ID) (catalog.VersionInfo, error)
}
type DiscussionMembers interface {
	Members(context.Context, authz.Context, ids.ID) ([]identity.Member, int64, error)
}

// DiscussionTasks stays owned by T05: it checks current visibility and whether
// the exact anchor belongs to this task. Calls inherit the security guard.
type DiscussionTasks interface {
	CheckDiscussion(context.Context, authz.Context, DiscussionTarget, bool) error
	CheckDiscussionAnchor(context.Context, authz.Context, DiscussionTarget, Anchor) error
}

type DiscussionObjects struct {
	ledger  *Service
	catalog DiscussionCatalog
	rights  rights.Evaluator
	members DiscussionMembers
	tasks   DiscussionTasks
}

// NewDiscussionObjects composes real core reads, without introducing a second
// task store. A nil T05 port disables task discussions only, explicitly.
func (s *Service) NewDiscussionObjects(catalog DiscussionCatalog, rights rights.Evaluator, members DiscussionMembers, tasks DiscussionTasks) (*DiscussionObjects, error) {
	if catalog == nil || rights == nil || members == nil {
		return nil, errors.New("ledger: discussion catalog, rights and membership required")
	}
	return &DiscussionObjects{s, catalog, rights, members, tasks}, nil
}
func (a *DiscussionObjects) readable(ctx context.Context, who authz.Context, v commit.Committed) error {
	if err := a.ledger.authorize(ctx, who, "catalog.read", v.ProjectID, "version", v.VersionID); err != nil {
		return err
	}
	if err := a.ledger.CheckVersionRead(ctx, v.AssetID, v.VersionID); err != nil {
		return err
	}
	d, err := a.rights.EvaluateUse(ctx, who, v.Ref(who.InstanceID), authz.PurposeArchiveReview)
	if err != nil {
		return err
	}
	return d.Err()
}
func (a *DiscussionObjects) Object(ctx context.Context, who authz.Context, target DiscussionTarget) error {
	var v commit.Committed
	var err error
	switch target.Kind {
	case "asset":
		v, err = a.ledger.LatestVersion(ctx, target.ID)
	case "version":
		v, err = a.ledger.VersionByID(ctx, target.ID)
	default:
		return errcode.New(errcode.SchemaInvalid, "resource discussion target required")
	}
	if err != nil {
		return err
	}
	if v.ProjectID != target.ProjectID {
		return errcode.New(errcode.NotFound, "")
	}
	return a.readable(ctx, who, v)
}
func (a *DiscussionObjects) Task(ctx context.Context, who authz.Context, target DiscussionTarget, write bool) error {
	if a.tasks == nil {
		return errcode.New(errcode.InvalidStateTransition, "T05 discussion authority is not configured")
	}
	return a.tasks.CheckDiscussion(ctx, who, target, write)
}
func (a *DiscussionObjects) Anchor(ctx context.Context, who authz.Context, target DiscussionTarget, anchor Anchor) error {
	if err := anchor.validate(); err != nil {
		return err
	}
	if anchor.Ref.InstanceID != who.InstanceID {
		return errcode.New(errcode.NotFound, "")
	}
	v, err := a.ledger.Version(ctx, anchor.Ref.AssetID, anchor.Ref.VersionID)
	if err != nil {
		return err
	}
	if v.ProjectID != target.ProjectID {
		return errcode.New(errcode.NotFound, "")
	}
	if target.Kind == "asset" && target.ID != v.AssetID || target.Kind == "version" && target.ID != v.VersionID {
		return errcode.New(errcode.RefMismatch, "anchor is outside discussion object")
	}
	if target.Kind == "task" {
		if err = a.Task(ctx, who, target, false); err != nil {
			return err
		}
		if err = a.tasks.CheckDiscussionAnchor(ctx, who, target, anchor); err != nil {
			return err
		}
	}
	if err = a.readable(ctx, who, v); err != nil {
		return err
	}
	doc, err := a.catalog.ReadProjectionVersion(ctx, v.AssetID, v.VersionID)
	if err != nil {
		return err
	}
	if doc.Version.VersionID != v.VersionID || doc.Version.ManifestDigest != v.ManifestDigest {
		return errcode.New(errcode.RefMismatch, "")
	}
	for _, file := range doc.Manifest.Content.Files {
		if file.Path == anchor.FilePath {
			return nil
		}
	}
	return errcode.New(errcode.NotFound, "")
}
func (a *DiscussionObjects) Mention(ctx context.Context, who authz.Context, target DiscussionTarget, mention Mention) error {
	if (mention.PrincipalID == "") == (mention.Role == "") {
		return errcode.New(errcode.SchemaInvalid, "")
	}
	members, _, err := a.members.Members(ctx, who, target.ProjectID)
	if err != nil {
		return err
	}
	for _, member := range members {
		if mention.PrincipalID != "" && member.PrincipalID == mention.PrincipalID {
			return nil
		}
		if mention.Role != "" && slices.Contains(member.Roles, identity.Role(mention.Role)) {
			return nil
		}
	}
	return errcode.New(errcode.NotFound, "")
}

var _ DiscussionAccess = (*DiscussionObjects)(nil)
