// Package node records authenticated worker observations. Reported capability
// names never grant a role, session scope or extension execution permission.
package node

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/event"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity"
	"slices"
	"time"
)

type Authority interface {
	authz.Authorizer
	authz.EpochSource
}
type Deps struct {
	DB        *sql.DB
	Gate      *commands.Gate
	Authority Authority
	IDs       *ids.Generator
	Clock     clock.Clock
}
type Service struct {
	d     Deps
	store *commands.Store
}
type Observation struct {
	ProjectID    ids.ID   `json:"project_id"`
	Capabilities []string `json:"capabilities"`
	Slots        int      `json:"slots"`
	Busy         int      `json:"busy"`
	MemoryBytes  int64    `json:"memory_bytes"`
}

func New(d Deps) (*Service, error) {
	if d.DB == nil || d.Gate == nil || d.Authority == nil || d.IDs == nil {
		return nil, errors.New("node: owner ports required")
	}
	if d.Clock == nil {
		d.Clock = clock.System{}
	}
	s, e := commands.NewStore("node", d.Clock)
	return &Service{d, s}, e
}
func (s *Service) auth(ctx context.Context, w authz.Context, project ids.ID) error {
	if w.PrincipalKind != authz.Worker && w.PrincipalKind != authz.Node {
		return errcode.New(errcode.Forbidden, "worker or node identity required")
	}
	action := authz.Action("ledger.append_check")
	if w.PrincipalKind == authz.Node {
		action = "node.observe"
	}
	d, e := s.d.Authority.Authorize(ctx, w, action, authz.Resource{ProjectID: project, Kind: "project", ID: project})
	if e != nil {
		return e
	}
	return d.Err()
}
func (s *Service) Observe(ctx context.Context, w authz.Context, key string, in Observation) (Observation, error) {
	if !in.ProjectID.Valid() || in.Slots < 1 || in.Slots > 8 || in.Busy < 0 || in.Busy > in.Slots || in.MemoryBytes < 0 || len(in.Capabilities) != 1 || in.Capabilities[0] != "org.lantai.corecheck.manifest" {
		return Observation{}, errcode.New(errcode.UnsupportedCapability, "only the static official check capability is enabled")
	}
	ctx, h, e := s.d.Gate.Acquire(ctx, commands.Request{Security: commands.ModeShared, Projects: []string{string(in.ProjectID)}})
	if e != nil {
		return Observation{}, e
	}
	defer h.Release()
	if e = s.auth(ctx, w, in.ProjectID); e != nil {
		return Observation{}, e
	}
	b, e := canonjson.CanonicalizeValue(in)
	if e != nil {
		return Observation{}, e
	}
	hash, e := commands.RequestHash(commands.HashInput{CommandType: "node.observe", ProjectID: in.ProjectID, Body: b})
	if e != nil {
		return Observation{}, e
	}
	epoch, e := s.d.Authority.RecoveryEpoch(ctx)
	if e != nil {
		return Observation{}, e
	}
	op, e := s.d.IDs.New()
	if e != nil {
		return Observation{}, e
	}
	c := commands.Context{OperationID: op, IdempotencyKey: key, CommandType: "node.observe", ActorID: w.PrincipalID, SessionID: w.SessionID, ProjectID: in.ProjectID, RequestHash: hash, RecoveryEpoch: epoch, PolicyRevision: w.PolicyRevision}
	res, e := s.store.Execute(ctx, s.d.DB, c, func(ctx context.Context, tx *sql.Tx) (commands.Result, error) {
		var revision int64
		e := tx.QueryRowContext(ctx, `INSERT INTO node_observations(principal_id,project_id,session_id,observed_at,record,revision) VALUES(?,?,?,?,?,1) ON CONFLICT(principal_id,project_id) DO UPDATE SET session_id=excluded.session_id,observed_at=excluded.observed_at,record=excluded.record,revision=node_observations.revision+1 RETURNING revision`, w.PrincipalID, in.ProjectID, w.SessionID, clock.Millis(s.d.Clock.Now()), string(b)).Scan(&revision)
		if e != nil {
			return commands.Result{}, e
		}
		ev, e := event.New(s.d.IDs, s.d.Clock, event.Params{EventType: "node.observed", SchemaVersion: 1, AggregateType: "node", AggregateID: w.PrincipalID, AggregateRevision: revision, ActorID: w.PrincipalID, SessionID: w.SessionID, ProjectID: in.ProjectID, OperationID: op, CorrelationID: c.Correlation(), Payload: map[string]any{"slots": in.Slots}})
		if e != nil {
			return commands.Result{}, e
		}
		return commands.Result{Status: commands.ReceiptSucceeded, ResponseCode: 200, Summary: in, Events: []event.Envelope{ev}}, nil
	})
	if e != nil {
		return Observation{}, e
	}
	var out Observation
	e = json.Unmarshal(res.Receipt.ResponseSummary, &out)
	return out, e
}
func (s *Service) CheckWorker(ctx context.Context, w authz.Context, p ids.ID, capability string) error {
	if e := s.auth(ctx, w, p); e != nil {
		return e
	}
	var session ids.ID
	var when int64
	var raw string
	e := s.d.DB.QueryRowContext(ctx, `SELECT session_id,observed_at,record FROM node_observations WHERE principal_id=? AND project_id=?`, w.PrincipalID, p).Scan(&session, &when, &raw)
	if errors.Is(e, sql.ErrNoRows) {
		return errcode.New(errcode.UnsupportedCapability, "register worker capability before dispatch")
	}
	if e != nil {
		return e
	}
	var o Observation
	if e = json.Unmarshal([]byte(raw), &o); e != nil {
		return e
	}
	if session != w.SessionID || s.d.Clock.Now().Sub(clock.FromMillis(when)) >= 5*time.Minute || !slices.Contains(o.Capabilities, capability) || o.Busy >= o.Slots {
		return errcode.New(errcode.ResourceBusy, "worker observation stale, unavailable or full")
	}
	return nil
}
func (s *Service) Read(ctx context.Context, w authz.Context, p, principal ids.ID) (Observation, error) {
	d, e := s.d.Authority.Authorize(ctx, w, identity.ActTasksRead, authz.Resource{ProjectID: p, Kind: "project", ID: p})
	if e != nil {
		return Observation{}, e
	}
	if e = d.Err(); e != nil {
		return Observation{}, e
	}
	var b string
	e = s.d.DB.QueryRowContext(ctx, `SELECT record FROM node_observations WHERE principal_id=? AND project_id=?`, principal, p).Scan(&b)
	if errors.Is(e, sql.ErrNoRows) {
		return Observation{}, errcode.New(errcode.NotFound, "")
	}
	var o Observation
	if e == nil {
		e = json.Unmarshal([]byte(b), &o)
	}
	return o, e
}
