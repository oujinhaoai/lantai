package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"strings"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/event"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity"
)

// HumanCommands is implemented by identity.Service; caller-supplied grant IDs
// are never accepted without that service's current proof and replay checks.
type HumanCommands interface {
	AcceptHumanItem(context.Context, authz.Context, ids.ID, ids.ID, identity.HumanAction, commands.Request, identity.HumanDomain) (commands.Receipt, error)
}

type HumanBatchItemResult struct {
	OperationID ids.ID            `json:"operation_id"`
	ResourceID  ids.ID            `json:"resource_id"`
	Receipt     *commands.Receipt `json:"receipt,omitempty"`
	ErrorCode   errcode.Code      `json:"error_code,omitempty"`
}

// Activity is the T05 authority for current flow/checkout/attempt use. It is read
// under the security guard. Missing implementations fail closed for archival.
type Activity interface {
	RequireIdle(context.Context, ids.ID, []ids.ID) error
}
type ControlMutation struct {
	Action           authz.Action `json:"action"`
	ProjectID        ids.ID       `json:"project_id"`
	Kind             string       `json:"kind"`
	ID               ids.ID       `json:"id"`
	ExpectedRevision int64        `json:"expected_revision"`
	LatestVersionID  ids.ID       `json:"latest_version_id,omitempty"`
	Reason           string       `json:"reason"`
}

func (r ControlMutation) validate() error {
	if !r.ProjectID.Valid() || !r.ID.Valid() || r.ExpectedRevision < 1 || strings.TrimSpace(r.Reason) == "" || len(r.Reason) > 4096 {
		return invalid("control target, revision and reason required")
	}
	if r.LatestVersionID != "" && (!r.LatestVersionID.Valid() || r.Kind != "asset" || r.Action != "ledger.archive" && r.Action != "ledger.unarchive") {
		return invalid("latest version binding is only valid for asset archival")
	}
	switch r.Action {
	case "ledger.unlock":
		if r.Kind != "lock" {
			return invalid("unlock requires lock target")
		}
	case "ledger.disable_version", "ledger.enable_version":
		if r.Kind != "version" {
			return invalid("availability requires version target")
		}
	case "ledger.suspend":
		if r.Kind != "asset" {
			return invalid("suspend requires asset target")
		}
	case "ledger.archive", "ledger.unarchive":
		if !slices.Contains([]string{"asset", "version"}, r.Kind) {
			return invalid("archive requires asset or version target")
		}
	default:
		return invalid("unsupported control mutation")
	}
	return nil
}
func (s *Service) controlResource(ctx context.Context, r ControlMutation) (ids.ID, int64, error) {
	if err := r.validate(); err != nil {
		return "", 0, err
	}
	switch r.Kind {
	case "lock":
		l, err := readJSON[Lock](ctx, s.db, `SELECT record FROM ledger_locks WHERE lock_id=?`, r.ID)
		return l.ProjectID, l.Revision, err
	case "asset":
		a, err := s.Asset(ctx, r.ID)
		if err != nil {
			return "", 0, err
		}
		c, err := s.AssetControl(ctx, r.ID)
		return a.ProjectID, c.Revision, err
	case "version":
		v, err := s.VersionByID(ctx, r.ID)
		if err != nil {
			return "", 0, err
		}
		c, err := s.VersionControl(ctx, r.ID)
		return v.ProjectID, c.Revision, err
	}
	return "", 0, invalid("invalid target kind")
}

// ControlHumanAction is a trusted server-side builder. Challenge delivery must
// pass Service as HumanTargets; creation and acceptance verify real ownership.
func (s *Service) ControlHumanAction(ctx context.Context, r ControlMutation) (identity.HumanAction, error) {
	project, revision, err := s.controlResource(ctx, r)
	if err != nil {
		return identity.HumanAction{}, err
	}
	if project != r.ProjectID {
		return identity.HumanAction{}, errcode.New(errcode.RefMismatch, "")
	}
	if r.ExpectedRevision > revision {
		return identity.HumanAction{}, errcode.New(errcode.PreconditionFailed, "")
	}
	raw, err := canonjson.CanonicalizeValue(r)
	if err != nil {
		return identity.HumanAction{}, err
	}
	return identity.HumanAction{Action: r.Action, ProjectID: project, ResourceID: r.ID, ResourceRevision: r.ExpectedRevision, Request: raw}, nil
}
func (s *Service) ValidateHumanTarget(ctx context.Context, _ authz.Context, a identity.HumanAction) error {
	var r ControlMutation
	if err := json.Unmarshal(a.Request, &r); err != nil {
		return err
	}
	expected, err := s.ControlHumanAction(ctx, r)
	if err != nil {
		return err
	}
	one, err := canonjson.CanonicalizeValue(expected)
	if err != nil {
		return err
	}
	two, err := canonjson.CanonicalizeValue(a)
	if err != nil {
		return err
	}
	if string(one) != string(two) {
		return errcode.New(errcode.HumanGrantMismatch, "control target binding differs")
	}
	return nil
}
func (s *Service) ChangeControl(ctx context.Context, who authz.Context, r ControlMutation, grant, child ids.ID, human HumanCommands, activity Activity) (commands.Receipt, error) {
	if human == nil {
		return commands.Receipt{}, errcode.New(errcode.HumanProofRequired, "")
	}
	a, err := s.ControlHumanAction(ctx, r)
	if err != nil {
		return commands.Receipt{}, err
	}
	return human.AcceptHumanItem(ctx, who, grant, child, a, commands.Request{Security: commands.ModeExclusive, Projects: []string{string(r.ProjectID)}}, &humanControl{s: s, who: who, request: r, activity: activity})
}

type humanControl struct {
	s        *Service
	who      authz.Context
	request  ControlMutation
	activity Activity
}

func (h *humanControl) Receipt(ctx context.Context, id ids.ID) (commands.Receipt, error) {
	r, err := h.s.store.ReceiptByOperation(ctx, h.s.db, id)
	if err != nil {
		return commands.Receipt{}, err
	}
	return *r, nil
}
func (h *humanControl) Commit(ctx context.Context, cmd commands.Context, _ identity.HumanAction) (commands.Receipt, error) {
	s, r := h.s, h.request
	project, rev, err := s.controlResource(ctx, r)
	if err != nil {
		return commands.Receipt{}, err
	}
	if project != cmd.ProjectID || r.ProjectID != project || r.ExpectedRevision != rev {
		return commands.Receipt{}, errcode.New(errcode.PreconditionFailed, "control revision changed")
	}
	if r.Action == "ledger.archive" || r.Action == "ledger.unarchive" {
		asset := r.ID
		if r.LatestVersionID != "" {
			v, err := s.LatestVersion(ctx, r.ID)
			if err != nil {
				return commands.Receipt{}, err
			}
			if v.VersionID != r.LatestVersionID {
				return commands.Receipt{}, errcode.New(errcode.PreconditionFailed, "asset acquired another version after confirmation")
			}
		}
		if r.Kind == "version" {
			v, err := s.VersionByID(ctx, r.ID)
			if err != nil {
				return commands.Receipt{}, err
			}
			asset = v.AssetID
		}
		if h.activity == nil {
			return commands.Receipt{}, errcode.New(errcode.AssetInUse, "T05 activity authority required")
		}
		if err = h.activity.RequireIdle(ctx, project, []ids.ID{asset}); err != nil {
			return commands.Receipt{}, err
		}
	}
	result, err := s.store.Execute(ctx, s.db, cmd, func(ctx context.Context, tx *sql.Tx) (commands.Result, error) {
		var summary any
		var events []event.Envelope
		switch r.Kind {
		case "lock":
			lock, err := readJSON[Lock](ctx, tx, `SELECT record FROM ledger_locks WHERE lock_id=?`, r.ID)
			if err != nil {
				return commands.Result{}, err
			}
			if !lock.Active {
				return commands.Result{}, conflict()
			}
			lock.Active = false
			lock.Revision++
			if _, err = tx.ExecContext(ctx, `UPDATE ledger_locks SET active=0,revision=?,record=? WHERE lock_id=?`, lock.Revision, encoded(lock), lock.ID); err != nil {
				return commands.Result{}, err
			}
			summary = lock
		case "asset":
			a, err := assetControl(ctx, tx, r.ID)
			if err != nil {
				return commands.Result{}, err
			}
			if a.PendingOperationID != "" || a.Lifecycle == "trashed" || a.Lifecycle == "purged" {
				return commands.Result{}, conflict()
			}
			if r.Action == "ledger.suspend" {
				if a.PublicationState != "published" {
					return commands.Result{}, errcode.New(errcode.NotPublished, "")
				}
				e, err := s.suspendVersion(ctx, tx, cmd, a.AssetID, a.PublishedVersionID, r.Reason)
				if err != nil {
					return commands.Result{}, err
				}
				if e != nil {
					events = append(events, *e)
				}
				summary, err = assetControl(ctx, tx, a.AssetID)
				if err != nil {
					return commands.Result{}, err
				}
				break
			}
			want := "archived"
			if r.Action == "ledger.unarchive" {
				want = "active"
			}
			if a.Lifecycle == want {
				return commands.Result{}, conflict()
			}
			a.Lifecycle = want
			a.Revision++
			if err = saveAssetControl(ctx, tx, a); err != nil {
				return commands.Result{}, err
			}
			summary = a
		case "version":
			version, err := readJSON[struct{ AssetID ids.ID }](ctx, tx, `SELECT record FROM ledger_versions WHERE version_id=?`, r.ID)
			if err != nil {
				return commands.Result{}, err
			}
			parent, err := assetControl(ctx, tx, version.AssetID)
			if err != nil {
				return commands.Result{}, err
			}
			if parent.PendingOperationID != "" || parent.Lifecycle == "trashed" || parent.Lifecycle == "purged" {
				return commands.Result{}, conflict()
			}
			v, err := versionControl(ctx, tx, r.ID)
			if err != nil {
				return commands.Result{}, err
			}
			if v.PendingOperationID != "" || v.Lifecycle == "trashed" || v.Lifecycle == "purged" {
				return commands.Result{}, conflict()
			}
			switch r.Action {
			case "ledger.disable_version":
				v.Availability = "disabled"
				v.DisabledReason = r.Reason
			case "ledger.enable_version":
				v.Availability = "enabled"
				v.DisabledReason = ""
			case "ledger.archive":
				if v.Lifecycle != "active" {
					return commands.Result{}, conflict()
				}
				v.Lifecycle = "archived"
			case "ledger.unarchive":
				if v.Lifecycle != "archived" {
					return commands.Result{}, conflict()
				}
				v.Lifecycle = "active"
			}
			v.Revision++
			if err = saveVersionControl(ctx, tx, v); err != nil {
				return commands.Result{}, err
			}
			if r.Action == "ledger.disable_version" {
				committed, err := readJSON[struct{ AssetID ids.ID }](ctx, tx, `SELECT record FROM ledger_versions WHERE version_id=?`, v.VersionID)
				if err != nil {
					return commands.Result{}, err
				}
				e, err := s.suspendVersion(ctx, tx, cmd, committed.AssetID, v.VersionID, r.Reason)
				if err != nil {
					return commands.Result{}, err
				}
				if e != nil {
					events = append(events, *e)
				}
			}
			summary = v
		}
		e, err := s.event(cmd, "ledger.control_changed", r.Kind, r.ID, r.ExpectedRevision+1, map[string]any{"action": r.Action, "reason": r.Reason, "human_grant_id": cmd.HumanGrantID})
		if err != nil {
			return commands.Result{}, err
		}
		events = append(events, e)
		return commands.Result{Status: commands.ReceiptSucceeded, ResponseCode: 200, Summary: summary, Events: events}, nil
	})
	if err != nil {
		return commands.Receipt{}, err
	}
	return *result.Receipt, nil
}

type Publication struct {
	ID               ids.ID `json:"publication_id"`
	AssetID          ids.ID `json:"asset_id"`
	FromVersionID    ids.ID `json:"from_version_id,omitempty"`
	ToVersionID      ids.ID `json:"to_version_id,omitempty"`
	Action           string `json:"action"`
	ActorID          ids.ID `json:"actor_id"`
	Reason           string `json:"reason"`
	OperationID      ids.ID `json:"operation_id"`
	PreviousRevision int64  `json:"previous_revision"`
	Revision         int64  `json:"revision"`
	CreatedAt        string `json:"created_at"`
}

func (s *Service) suspendVersion(ctx context.Context, tx *sql.Tx, cmd commands.Context, asset, version ids.ID, reason string) (*event.Envelope, error) {
	a, err := assetControl(ctx, tx, asset)
	if err != nil {
		return nil, err
	}
	if a.PublicationState != "published" || a.PublishedVersionID != version {
		return nil, nil
	}
	id, err := s.ids.New()
	if err != nil {
		return nil, err
	}
	p := Publication{ID: id, AssetID: asset, FromVersionID: version, Action: "suspend", ActorID: cmd.ActorID, Reason: reason, OperationID: cmd.OperationID, PreviousRevision: a.PublicationRevision, Revision: a.PublicationRevision + 1, CreatedAt: clock.Format(s.clock.Now())}
	a.PublicationState = "suspended"
	a.PublishedVersionID = ""
	a.PublicationRevision++
	a.Revision++
	if err = saveAssetControl(ctx, tx, a); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO ledger_publications VALUES(?,?,?,?,?)`, id, asset, p.Revision, cmd.OperationID, encoded(p)); err != nil {
		return nil, err
	}
	e, err := s.event(cmd, "publication.changed", "asset", asset, a.PublicationRevision, p)
	return &e, err
}
