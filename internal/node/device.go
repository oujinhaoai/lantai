package node

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"time"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	dc "github.com/oujinhaoai/lantai/internal/contract/device"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/event"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity"
)

type DeviceSessions interface {
	WhoAmI(context.Context, authz.Context) (identity.Identity, error)
	EndSession(context.Context, authz.Context, ids.ID) error
}

var DeviceAlias = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
var DeviceDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)
var DeviceActions = []string{"import.plan", "import.apply", "import.verify", "workset.plan", "workset.pull", "references.plan", "references.copy", "references.rollback"}

type deviceRecord struct {
	Device         dc.Device     `json:"device"`
	Session        authz.Context `json:"session"`
	ClaimHash      string        `json:"claim_hash"`
	ClaimExpiresAt time.Time     `json:"claim_expires_at"`
}

func (s *Service) deviceLoad(ctx context.Context, id ids.ID) (deviceRecord, error) {
	var record deviceRecord
	if !id.Valid() {
		return record, errcode.New(errcode.SchemaInvalid, "device ID required")
	}
	var raw string
	err := s.d.DB.QueryRowContext(ctx, `SELECT record FROM node_devices WHERE device_id=?`, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return record, errcode.New(errcode.NotFound, "")
	}
	if err == nil {
		err = json.Unmarshal([]byte(raw), &record)
	}
	return record, err
}
func (s *Service) deviceAuth(ctx context.Context, who authz.Context, action authz.Action, project ids.ID) error {
	decision, err := s.d.Authority.Authorize(ctx, who, action, authz.Resource{ProjectID: project, Kind: "project", ID: project})
	if err != nil {
		return err
	}
	return decision.Err()
}
func (s *Service) deviceCommand(ctx context.Context, who authz.Context, key, typ string, project ids.ID, body any) (commands.Context, error) {
	b, err := canonjson.CanonicalizeValue(body)
	if err != nil {
		return commands.Context{}, err
	}
	hash, err := commands.RequestHash(commands.HashInput{CommandType: typ, ProjectID: project, Body: b})
	if err != nil {
		return commands.Context{}, err
	}
	epoch, err := s.d.Authority.RecoveryEpoch(ctx)
	if err != nil {
		return commands.Context{}, err
	}
	id, err := s.d.IDs.New()
	if err != nil {
		return commands.Context{}, err
	}
	cc := commands.Context{OperationID: id, IdempotencyKey: key, CommandType: typ, ActorID: who.PrincipalID, SessionID: who.SessionID, ProjectID: project, RequestHash: hash, RecoveryEpoch: epoch, PolicyRevision: who.PolicyRevision}
	return cc, cc.Validate()
}
func (s *Service) deviceReplay(ctx context.Context, cc commands.Context) (*dc.Device, error) {
	r, err := s.store.LookupReceipt(ctx, s.d.DB, cc.Key())
	if err != nil || r == nil {
		return nil, err
	}
	switch commands.Decide(r, cc.RequestHash, s.d.Clock.Now()) {
	case commands.OutcomeConflict:
		return nil, errcode.New(errcode.IdempotencyConflict, "")
	case commands.OutcomeExpired:
		return nil, errcode.New(errcode.IdempotencyResultExpired, "")
	case commands.OutcomeInProgress:
		return nil, errcode.New(errcode.ResourceBusy, "")
	}
	var view dc.Device
	err = json.Unmarshal(r.ResponseSummary, &view)
	return &view, err
}
func (s *Service) deviceSave(ctx context.Context, cc commands.Context, r deviceRecord) (dc.Device, error) {
	r.Device.Revision++
	_, err := s.store.Execute(ctx, s.d.DB, cc, func(ctx context.Context, tx *sql.Tx) (commands.Result, error) {
		body, e := json.Marshal(r)
		if e != nil {
			return commands.Result{}, e
		}
		_, e = tx.ExecContext(ctx, `INSERT INTO node_devices(device_id,project_id,owner_id,session_id,state,record) VALUES(?,?,?,?,?,?) ON CONFLICT(device_id) DO UPDATE SET state=excluded.state,record=excluded.record`, r.Device.ID, r.Device.ProjectID, r.Device.OwnerID, r.Session.SessionID, r.Device.State, string(body))
		if e != nil {
			return commands.Result{}, e
		}
		ev, e := event.New(s.d.IDs, s.d.Clock, event.Params{EventType: "node.device_changed", SchemaVersion: 1, AggregateType: "node", AggregateID: r.Device.ID, AggregateRevision: r.Device.Revision, ActorID: cc.ActorID, SessionID: cc.SessionID, ProjectID: cc.ProjectID, OperationID: cc.OperationID, CorrelationID: cc.Correlation(), Payload: map[string]any{"state": r.Device.State}})
		if e != nil {
			return commands.Result{}, e
		}
		return commands.Result{Status: commands.ReceiptSucceeded, ResponseCode: 200, Summary: r.Device, Events: []event.Envelope{ev}}, nil
	})
	return r.Device, err
}
func (s *Service) ClaimDevice(ctx context.Context, who authz.Context, key string, in dc.Claim) (dc.ClaimResponse, error) {
	if s.d.Sessions == nil {
		return dc.ClaimResponse{}, errcode.New(errcode.UnsupportedCapability, "")
	}
	if !in.ProjectID.Valid() || !DeviceAlias.MatchString(in.Alias) || !DeviceAlias.MatchString(in.WorkspaceAlias) || !DeviceDigest.MatchString(in.WorkspaceBinding) || len(in.Actions) < 1 || len(in.Actions) > len(DeviceActions) {
		return dc.ClaimResponse{}, errcode.New(errcode.SchemaInvalid, "bounded device alias, project, workspace and actions required")
	}
	seen := map[string]bool{}
	for _, action := range in.Actions {
		if !slices.Contains(DeviceActions, action) || seen[action] {
			return dc.ClaimResponse{}, errcode.New(errcode.UnsupportedCapability, "explicit eight-action subset required")
		}
		seen[action] = true
	}
	ctx, held, err := s.d.Gate.Acquire(ctx, commands.Request{Security: commands.ModeShared, Projects: []string{string(in.ProjectID)}, Tasks: []string{"node:devices"}})
	if err != nil {
		return dc.ClaimResponse{}, err
	}
	defer held.Release()
	if err = s.deviceAuth(ctx, who, identity.ActDeviceManage, in.ProjectID); err != nil {
		return dc.ClaimResponse{}, err
	}
	identityView, err := s.d.Sessions.WhoAmI(ctx, who)
	if err != nil {
		return dc.ClaimResponse{}, err
	}
	info := identityView.Session
	remaining := info.ExpiresAt.Sub(s.d.Clock.Now())
	if slices.Contains(info.Scopes, identity.ScopeAdmin) || info.Channel != identity.ChannelCLI || info.Kind != "normal" || info.ParentSessionID != "" || info.DelegatedBy != "" || info.CredentialID != "" || len(info.Projects) != 1 || info.Projects[0] != in.ProjectID || remaining <= 0 || remaining > 15*time.Minute || info.ExpiresAt.Sub(info.CreatedAt) > 15*time.Minute {
		return dc.ClaimResponse{}, errcode.New(errcode.Forbidden, "independent normal human CLI session limited to one project and at most 15 minutes required")
	}
	cc, err := s.deviceCommand(ctx, who, key, "node.device_claim", in.ProjectID, in)
	if err != nil {
		return dc.ClaimResponse{}, err
	}
	if old, e := s.deviceReplay(ctx, cc); old != nil || e != nil {
		if old != nil {
			return dc.ClaimResponse{Device: *old}, e
		}
		return dc.ClaimResponse{}, e
	}
	var existing string
	err = s.d.DB.QueryRowContext(ctx, `SELECT device_id FROM node_devices WHERE session_id=?`, who.SessionID).Scan(&existing)
	if err == nil {
		return dc.ClaimResponse{}, errcode.New(errcode.IdempotencyConflict, "this independent session already belongs to a device")
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return dc.ClaimResponse{}, err
	}
	id, err := s.d.IDs.New()
	if err != nil {
		return dc.ClaimResponse{}, err
	}
	secret := make([]byte, 16)
	if _, err = rand.Read(secret); err != nil {
		return dc.ClaimResponse{}, err
	}
	code := hex.EncodeToString(secret)
	hash := sha256.Sum256([]byte(code))
	r := deviceRecord{Device: dc.Device{ID: id, ProjectID: in.ProjectID, OwnerID: who.PrincipalID, Alias: in.Alias, WorkspaceAlias: in.WorkspaceAlias, WorkspaceBinding: in.WorkspaceBinding, Actions: slices.Clone(in.Actions), State: "pending", ExpiresAt: info.ExpiresAt, LastSeenAt: s.d.Clock.Now()}, Session: identityView.Context, ClaimHash: hex.EncodeToString(hash[:]), ClaimExpiresAt: s.d.Clock.Now().Add(2 * time.Minute)}
	view, err := s.deviceSave(ctx, cc, r)
	return dc.ClaimResponse{Device: view, ClaimCode: code}, err
}

// An enrolled device session is a worker, never a human management session
// for another device belonging to the same person.
func (s *Service) deviceManagementSession(ctx context.Context, who authz.Context) error {
	var bound int
	if err := s.d.DB.QueryRowContext(ctx, `SELECT count(*) FROM node_devices WHERE session_id=?`, who.SessionID).Scan(&bound); err != nil {
		return err
	}
	if bound != 0 {
		return errcode.New(errcode.NotFound, "")
	}
	return nil
}

// OwnedDevice and WorkerDevice are in-process owner read ports. Callers that
// perform an acceptance hold the existing shared security guard throughout.
func (s *Service) OwnedDevice(ctx context.Context, who authz.Context, id ids.ID, manage bool) (dc.Device, error) {
	r, err := s.deviceLoad(ctx, id)
	if err != nil {
		return dc.Device{}, err
	}
	action := identity.ActDeviceRead
	if manage {
		action = identity.ActDeviceManage
	}
	if err = s.deviceAuth(ctx, who, action, r.Device.ProjectID); err != nil {
		return dc.Device{}, err
	}
	if err = s.deviceManagementSession(ctx, who); err != nil {
		return dc.Device{}, err
	}
	if r.Device.OwnerID != who.PrincipalID || r.Session.SessionID == who.SessionID {
		return dc.Device{}, errcode.New(errcode.NotFound, "")
	}
	view := r.Device
	if view.State != "revoked" && (!s.d.Clock.Now().Before(view.ExpiresAt) || r.Session.RecoveryEpoch != who.RecoveryEpoch) {
		view.State = "expired"
	}
	return view, nil
}
func (s *Service) WorkerDevice(ctx context.Context, who authz.Context, id ids.ID) (dc.Device, error) {
	r, err := s.deviceLoad(ctx, id)
	if err != nil {
		return dc.Device{}, err
	}
	if err = s.deviceAuth(ctx, who, identity.ActDeviceWork, r.Device.ProjectID); err != nil {
		return dc.Device{}, err
	}
	if r.Device.OwnerID != who.PrincipalID || r.Session.SessionID != who.SessionID {
		return dc.Device{}, errcode.New(errcode.NotFound, "")
	}
	if r.Device.State != "active" || !s.d.Clock.Now().Before(r.Device.ExpiresAt) || r.Session.RecoveryEpoch != who.RecoveryEpoch {
		return dc.Device{}, errcode.New(errcode.Forbidden, "device inactive, expired or revoked")
	}
	return r.Device, nil
}
func (s *Service) ApproveDevice(ctx context.Context, who authz.Context, key string, id ids.ID, in dc.ApproveDevice) (dc.Device, error) {
	r, err := s.deviceLoad(ctx, id)
	if err != nil {
		return dc.Device{}, err
	}
	ctx, held, err := s.d.Gate.Acquire(ctx, commands.Request{Security: commands.ModeShared, Projects: []string{string(r.Device.ProjectID)}, Tasks: []string{"node:devices"}})
	if err != nil {
		return dc.Device{}, err
	}
	defer held.Release()
	if _, err = s.OwnedDevice(ctx, who, id, true); err != nil {
		return dc.Device{}, err
	}
	r, err = s.deviceLoad(ctx, id)
	if err != nil {
		return dc.Device{}, err
	}
	cc, err := s.deviceCommand(ctx, who, key, "node.device_approve", r.Device.ProjectID, struct {
		ID    ids.ID
		Input dc.ApproveDevice
	}{id, in})
	if err != nil {
		return dc.Device{}, err
	}
	if old, e := s.deviceReplay(ctx, cc); old != nil || e != nil {
		if old != nil {
			return *old, e
		}
		return dc.Device{}, e
	}
	if r.Device.State != "pending" || in.ExpectedRevision != r.Device.Revision || in.WorkspaceAlias != r.Device.WorkspaceAlias || in.WorkspaceBinding != r.Device.WorkspaceBinding || !s.d.Clock.Now().Before(r.ClaimExpiresAt) || !s.d.Clock.Now().Before(r.Device.ExpiresAt) {
		return dc.Device{}, errcode.New(errcode.PreconditionFailed, "device claim bindings or deadline changed")
	}
	hash := sha256.Sum256([]byte(in.ClaimCode))
	if subtle.ConstantTimeCompare([]byte(hex.EncodeToString(hash[:])), []byte(r.ClaimHash)) != 1 {
		return dc.Device{}, errcode.New(errcode.Forbidden, "claim code does not match")
	}
	if _, err = s.d.Sessions.WhoAmI(ctx, r.Session); err != nil {
		return dc.Device{}, err
	}
	r.Device.State = "active"
	r.ClaimHash = ""
	return s.deviceSave(ctx, cc, r)
}
func (s *Service) RevokeDevice(ctx context.Context, who authz.Context, key string, id ids.ID, in dc.Control) (dc.Device, error) {
	if s.d.Sessions == nil {
		return dc.Device{}, errcode.New(errcode.UnsupportedCapability, "")
	}
	r, err := s.deviceLoad(ctx, id)
	if err != nil {
		return dc.Device{}, err
	}
	locked, held, err := s.d.Gate.Acquire(ctx, commands.Request{Security: commands.ModeShared, Projects: []string{string(r.Device.ProjectID)}, Tasks: []string{"node:devices"}})
	if err != nil {
		return dc.Device{}, err
	}
	if _, err = s.OwnedDevice(locked, who, id, true); err != nil {
		held.Release()
		return dc.Device{}, err
	}
	r, err = s.deviceLoad(locked, id)
	if err != nil {
		held.Release()
		return dc.Device{}, err
	}
	cc, err := s.deviceCommand(locked, who, key, "node.device_revoke", r.Device.ProjectID, struct {
		ID    ids.ID
		Input dc.Control
	}{id, in})
	if err != nil {
		held.Release()
		return dc.Device{}, err
	}
	old, err := s.deviceReplay(locked, cc)
	if err != nil {
		held.Release()
		return dc.Device{}, err
	}
	view := r.Device
	if old == nil {
		if in.ExpectedRevision != r.Device.Revision {
			held.Release()
			return dc.Device{}, errcode.New(errcode.PreconditionFailed, "")
		}
		r.Device.State = "revoked"
		view, err = s.deviceSave(locked, cc, r)
	}
	held.Release()
	if err != nil {
		return dc.Device{}, err
	}
	// Cross-owner operations do not share a SQL transaction or nested guard.
	// Return success only after the independently bound device session ended.
	err = s.d.Sessions.EndSession(ctx, r.Session, r.Session.SessionID)
	if err != nil && errcode.CodeOf(err) != errcode.TokenExpired && errcode.CodeOf(err) != errcode.TokenRevoked {
		return view, err
	}
	return view, nil
}
func (s *Service) DeviceList(ctx context.Context, who authz.Context, project, after ids.ID, limit int) (dc.DevicePage, error) {
	if !project.Valid() || after != "" && !after.Valid() || limit < 1 || limit > 100 {
		return dc.DevicePage{}, errcode.New(errcode.SchemaInvalid, "")
	}
	ctx, held, err := s.d.Gate.Coordinator().Acquire(ctx, commands.Request{Security: commands.ModeShared})
	if err != nil {
		return dc.DevicePage{}, err
	}
	defer held.Release()
	if err = s.deviceAuth(ctx, who, identity.ActDeviceRead, project); err != nil {
		return dc.DevicePage{}, err
	}
	if err = s.deviceManagementSession(ctx, who); err != nil {
		return dc.DevicePage{}, err
	}
	rows, err := s.d.DB.QueryContext(ctx, `SELECT record FROM node_devices WHERE project_id=? AND owner_id=? AND session_id<>? AND device_id>? ORDER BY device_id LIMIT ?`, project, who.PrincipalID, who.SessionID, after, limit+1)
	if err != nil {
		return dc.DevicePage{}, err
	}
	defer rows.Close()
	out := dc.DevicePage{Items: []dc.Device{}}
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			return dc.DevicePage{}, err
		}
		var record deviceRecord
		if err = json.Unmarshal([]byte(raw), &record); err != nil {
			return dc.DevicePage{}, err
		}
		view := record.Device
		if view.State != "revoked" && (!s.d.Clock.Now().Before(view.ExpiresAt) || record.Session.RecoveryEpoch != who.RecoveryEpoch) {
			view.State = "expired"
		}
		out.Items = append(out.Items, view)
	}
	if len(out.Items) > limit {
		out.Items = out.Items[:limit]
		out.NextCursor = string(out.Items[limit-1].ID)
	}
	return out, rows.Err()
}
