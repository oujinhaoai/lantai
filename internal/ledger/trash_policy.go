package ledger

import (
	"slices"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

// Facts are assembled from current owning-module reads under the security writer
// guard. They are deliberately private: API callers cannot supply these flags.
type trashVersionFacts struct {
	id            ids.ID
	author        ids.ID
	committed     time.Time
	everApproved  bool
	humanReviewed bool
	lifecycle     string
	locked        bool
	published     bool
}
type trashFacts struct {
	actor                authz.Context
	versions             []trashVersionFacts
	latestCommitted      time.Time
	firstCommitted       time.Time
	wholeAsset           bool
	directory            bool
	inUse                bool
	humanAuthorized      bool
	adminForceAuthorized bool
	retentionDays        int
	normalReserved       int
	graceReserved        int
	now                  time.Time
}
type trashDecision struct {
	grace         bool
	retentionDays int
	quotaUnits    int
	releaseName   bool
}

// decideTrash enforces the fixed D18 policy. Quota counts passed here include
// both succeeded deletions and unfinished reservations in the rolling hour;
// the caller must check and reserve them in its ledger transaction.
func decideTrash(f trashFacts) (trashDecision, error) {
	var d trashDecision
	if !f.actor.PrincipalID.Valid() || len(f.versions) == 0 || f.now.IsZero() || f.latestCommitted.IsZero() || f.firstCommitted.IsZero() || f.retentionDays < 7 || f.retentionDays > 365 || f.normalReserved < 0 || f.graceReserved < 0 {
		return d, invalid("invalid authoritative deletion facts")
	}
	if f.latestCommitted.After(f.now) || f.firstCommitted.After(f.latestCommitted) {
		return d, errcode.New(errcode.OperationNeedsReconciliation, "invalid commit chronology")
	}
	allOwn, unreviewed, unapproved := true, true, true
	seen := map[ids.ID]bool{}
	liveVersions := 0
	for _, v := range f.versions {
		if !v.id.Valid() || seen[v.id] || !v.author.Valid() || v.committed.IsZero() || (v.committed.After(f.latestCommitted) || v.committed.Before(f.firstCommitted)) {
			return d, invalid("invalid deletion version set")
		}
		seen[v.id] = true
		live := slices.Contains([]string{"active", "archived"}, v.lifecycle)
		if live {
			liveVersions++
		}
		if !live && !(f.wholeAsset && slices.Contains([]string{"trashed", "purged"}, v.lifecycle)) {
			return d, errcode.New(errcode.InvalidStateTransition, "deletion target is not live")
		}
		if v.locked {
			return d, errcode.New(errcode.AssetLocked, "unlock before deletion")
		}
		if v.published {
			return d, errcode.New(errcode.AssetInUse, "suspend publication before deletion")
		}
		allOwn = allOwn && v.author == f.actor.PrincipalID
		unreviewed = unreviewed && !v.humanReviewed
		unapproved = unapproved && !v.everApproved
	}
	// A fresh latest version permits removal of the same author's old intermediate
	// versions; version numbers and client timestamps never define the grace window.
	d.grace = f.now.Sub(f.latestCommitted) < 3*time.Hour && allOwn && unreviewed && unapproved && !f.inUse
	if f.wholeAsset {
		d.grace = d.grace && f.now.Sub(f.firstCommitted) < 3*time.Hour
	}
	if f.directory && !f.humanAuthorized {
		return d, errcode.New(errcode.HumanProofRequired, "whole directories always require exact human approval")
	}
	if f.inUse && !f.adminForceAuthorized {
		return d, errcode.New(errcode.AssetInUse, "")
	}
	if f.adminForceAuthorized && (!f.humanAuthorized || f.actor.PrincipalKind != authz.Human || f.actor.DelegatedBy != "") {
		return d, errcode.New(errcode.HumanProofRequired, "force deletion requires an exact human admin action")
	}
	if !d.grace && liveVersions > 50 && !f.humanAuthorized {
		return d, errcode.New(errcode.HumanProofRequired, "normal batches over 50 require confirmation")
	}
	isAgent := f.actor.PrincipalKind == authz.Agent
	if isAgent {
		if f.humanAuthorized || f.adminForceAuthorized {
			return d, errcode.New(errcode.Forbidden, "agents cannot use human deletion grants")
		}
		if !allOwn || !unapproved {
			return d, errcode.New(errcode.Forbidden, "agent deletion is limited to own never-approved versions")
		}
		if f.wholeAsset && !d.grace {
			return d, errcode.New(errcode.HumanProofRequired, "whole asset deletion requires owner approval outside its grace window")
		}
		if d.grace {
			if liveVersions > 200-f.graceReserved {
				return d, errcode.New(errcode.QuotaExceeded, "grace deletion rolling-hour quota exhausted")
			}
		} else if liveVersions > 20-f.normalReserved {
			return d, errcode.New(errcode.QuotaExceeded, "normal deletion rolling-hour quota exhausted")
		}
		d.quotaUnits = liveVersions
	} else {
		if f.actor.PrincipalKind != authz.Human || f.actor.DelegatedBy != "" {
			return d, errcode.New(errcode.Forbidden, "only project humans and agents can request deletion")
		}
		if (!d.grace || f.directory || f.inUse) && !f.humanAuthorized {
			return d, errcode.New(errcode.HumanProofRequired, "")
		}
	}
	d.retentionDays = f.retentionDays
	if d.grace {
		d.retentionDays = 7
	}
	d.releaseName = f.wholeAsset && d.grace
	return d, nil
}
