// Package ledger owns authoritative committed versions, namespace claims and
// effective metadata revisions. Its only database is ledger.db.
package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/event"
	"github.com/oujinhaoai/lantai/internal/contract/execution"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/install"
)

const Module = "ledger"
const (
	EvVersionCommitted  = "version.committed"
	EvProjectRegistered = "ledger.project_registered"
	EvMetadataCommitted = "ledger.metadata_committed"
)

type Authority interface {
	authz.Authorizer
	authz.EpochSource
}

// Deps uses already migrated instance resources. Verifiers may be connected
// after construction to resolve the catalog/storage/ledger assembly cycle.
type Deps struct {
	DB         *sql.DB
	Gate       *commands.Gate
	Authority  Authority
	Clock      clock.Clock
	IDs        *ids.Generator
	Installer  install.Installer
	Revisions  commit.RevisionVerifier
	Acceptance commit.AcceptanceVerifier
}

type Service struct {
	db         *sql.DB
	gate       *commands.Gate
	authority  Authority
	clock      clock.Clock
	ids        *ids.Generator
	store      *commands.Store
	wiring     sync.RWMutex
	installer  install.Installer
	revisions  commit.RevisionVerifier
	acceptance commit.AcceptanceVerifier
	guard      CheckoutGuard
}

var (
	_ commit.Ledger    = (*Service)(nil)
	_ commit.Reader    = (*Service)(nil)
	_ commit.Namespace = (*Service)(nil)
	_ commit.Metadata  = (*Service)(nil)
	_ commit.Projects  = (*Service)(nil)
)

func New(d Deps) (*Service, error) {
	if d.DB == nil || d.Gate == nil || d.Authority == nil || d.IDs == nil {
		return nil, errors.New("ledger: database, gate, authority and id generator are required")
	}
	if d.Clock == nil {
		d.Clock = clock.System{}
	}
	st, err := commands.NewStore(Module, d.Clock)
	if err != nil {
		return nil, err
	}
	return &Service{db: d.DB, gate: d.Gate, authority: d.Authority, clock: d.Clock, ids: d.IDs, store: st, installer: d.Installer, revisions: d.Revisions, acceptance: d.Acceptance}, nil
}

func (s *Service) SetInstaller(v install.Installer) {
	s.wiring.Lock()
	defer s.wiring.Unlock()
	s.installer = v
}
func (s *Service) SetRevisionVerifier(v commit.RevisionVerifier) {
	s.wiring.Lock()
	defer s.wiring.Unlock()
	s.revisions = v
}
func (s *Service) SetAcceptanceVerifier(v commit.AcceptanceVerifier) {
	s.wiring.Lock()
	defer s.wiring.Unlock()
	s.acceptance = v
}
func (s *Service) verifiers() (install.Installer, commit.RevisionVerifier, commit.AcceptanceVerifier) {
	s.wiring.RLock()
	defer s.wiring.RUnlock()
	return s.installer, s.revisions, s.acceptance
}

func (s *Service) write(ctx context.Context, project ids.ID) (context.Context, func(), error) {
	req := commands.Request{Security: commands.ModeShared}
	if project != "" {
		req.Projects = []string{string(project)}
	}
	lctx, h, err := s.gate.Acquire(ctx, req)
	if err != nil {
		return ctx, nil, err
	}
	return lctx, h.Release, nil
}

func (s *Service) authorize(ctx context.Context, who authz.Context, action authz.Action, project ids.ID, kind string, id ids.ID) error {
	d, err := s.authority.Authorize(ctx, who, action, authz.Resource{ProjectID: project, Kind: kind, ID: id})
	if err != nil {
		return err
	}
	return d.Err()
}
func (s *Service) finalCheck(ctx context.Context, cmd commands.Context, who authz.Context, action authz.Action, project ids.ID, kind string, id ids.ID) error {
	if who.PrincipalID != cmd.ActorID {
		return errcode.New(errcode.Forbidden, "only the accepting actor can complete this operation")
	}
	if err := s.authorize(ctx, who, action, project, kind, id); err != nil {
		return err
	}
	epoch, err := s.authority.RecoveryEpoch(ctx)
	if err != nil {
		return err
	}
	return execution.CheckRecoveryEpoch(execution.SubjectOperation, cmd.RecoveryEpoch, epoch)
}

func encoded(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}
func missing(err error) error {
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, commands.ErrNotFound) {
		return errcode.New(errcode.NotFound, "")
	}
	return err
}
func invalid(msg string) error { return errcode.New(errcode.SchemaInvalid, msg) }
func busy(id ids.ID) error     { return errcode.New(errcode.ResourceBusy, "").WithOperation(string(id)) }
func conflict() error {
	return errcode.New(errcode.InvalidStateTransition, "operation cannot complete in its current stage")
}
func inTx(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) event(cmd commands.Context, typ, kind string, id ids.ID, rev int64, payload any) (event.Envelope, error) {
	return event.New(s.ids, s.clock, event.Params{EventType: typ, SchemaVersion: 1, AggregateType: kind, AggregateID: id, AggregateRevision: rev, ActorID: cmd.ActorID, SessionID: cmd.SessionID, ProjectID: cmd.ProjectID, OperationID: cmd.OperationID, CorrelationID: cmd.Correlation(), Payload: payload})
}

func (s *Service) Operation(ctx context.Context, id ids.ID) (*commands.View, error) {
	v, err := s.store.View(ctx, s.db, id)
	if err == nil && v.OwnerModule != Module {
		return nil, errcode.New(errcode.NotFound, "")
	}
	return v, missing(err)
}

// OpenOperations is a read-only recovery dispatcher hook. Calling it never
// installs or commits content and does not infer acceptance from directories.
func (s *Service) OpenOperations(ctx context.Context) ([]ids.ID, error) {
	return s.store.OpenOperations(ctx, s.db)
}

func (s *Service) lookup(ctx context.Context, q commands.DBTX, cmd commands.Context) (*commands.Receipt, error) {
	r, err := s.store.LookupReceipt(ctx, q, cmd.Key())
	if err != nil {
		return nil, err
	}
	if r != nil && r.RequestHash != cmd.RequestHash {
		return nil, errcode.New(errcode.IdempotencyConflict, "").WithOperation(string(r.OperationID))
	}
	return r, nil
}

func (s *Service) unusedOperation(ctx context.Context, q commands.DBTX, id ids.ID) error {
	r, err := s.store.ReceiptByOperation(ctx, q, id)
	if errors.Is(err, commands.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return errcode.New(errcode.IdempotencyConflict, "operation ID is already bound to another command").WithOperation(string(r.OperationID))
}

func (s *Service) changeStage(ctx context.Context, id ids.ID, stage commands.Stage, reason error) error {
	return inTx(ctx, s.db, func(tx *sql.Tx) error {
		op, err := s.store.GetOperation(ctx, tx, id)
		if err != nil {
			return missing(err)
		}
		if op.Stage.Final() || op.Stage == commands.StageQuarantined {
			return conflict()
		}
		code := errcode.CodeOf(reason)
		if _, ok := errcode.Lookup(code); !ok {
			code = errcode.OperationNeedsReconciliation
		}
		if op.Stage != stage {
			if err := s.store.Advance(ctx, tx, id, op.Stage, stage, commands.Update{FailureCode: code}); err != nil {
				return err
			}
		}
		if stage == commands.StageQuarantined {
			return s.release(ctx, tx, id)
		}
		return nil
	})
}
