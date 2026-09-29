package identity

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

// HumanAction fixes the complete domain request, not merely a UI description.
// T03/T09 must reconstruct it from the actual command at final acceptance.
// ResourceRevision binds the current review/lock/trash/rights target; the domain
// remains responsible for checking that revision and all business invariants.
type HumanAction struct {
	Action           authz.Action            `json:"action"`
	ProjectID        ids.ID                  `json:"project_id,omitempty"`
	ResourceID       ids.ID                  `json:"resource_id"`
	ResourceRevision int64                   `json:"resource_revision"`
	ManifestDigest   digest.Digest           `json:"manifest_digest,omitempty"`
	Extension        *ExtensionAuthorization `json:"extension,omitempty"`
	Request          json.RawMessage         `json:"request"`
}

type ExtensionAuthorization struct {
	PluginID       string        `json:"plugin_id"`
	PackageDigest  digest.Digest `json:"package_digest"`
	Target         string        `json:"target"`
	ScopeKind      string        `json:"scope_kind"`
	ScopeID        ids.ID        `json:"scope_id,omitempty"`
	ConfigRevision int64         `json:"config_revision"`
	ConfigDigest   digest.Digest `json:"config_digest"`
	PolicyRevision int64         `json:"policy_revision"`
}

func (a HumanAction) validate() error {
	if !a.ResourceID.Valid() || a.ResourceRevision < 1 {
		return fmt.Errorf("precise target ID and revision required")
	}
	switch a.Action {
	case ActRecordReview, ActRevokeReview, ActUnlock, ActReleaseRestriction, ActReleaseName, ActPurge, ActHold, ActUnhold, "ledger.disable_version", "ledger.enable_version", "ledger.archive", "ledger.unarchive", "ledger.suspend", "ledger.trash", "ledger.force_trash":
		if !a.ProjectID.Valid() || a.Extension != nil {
			return fmt.Errorf("resource action needs a project and no extension target")
		}
		if a.Action == ActRecordReview || a.Action == ActRevokeReview || a.Action == ActReleaseRestriction {
			if !a.ManifestDigest.Valid() {
				return fmt.Errorf("manifest digest required")
			}
		}
	case ActEnableExtension, ActDisableExtension:
		x := a.Extension
		if a.ProjectID != "" || x == nil || len(x.PluginID) == 0 || len(x.PluginID) > 256 || !x.PackageDigest.Valid() || !x.ConfigDigest.Valid() || x.ConfigRevision < 1 || x.PolicyRevision < 1 {
			return fmt.Errorf("extension identity, configuration and policy bindings required")
		}
		if !slices.Contains([]string{"server", "node", "cli"}, x.Target) {
			return fmt.Errorf("unsupported extension target")
		}
		if x.ScopeKind == "instance" {
			if x.ScopeID != "" {
				return fmt.Errorf("instance scope has no ID")
			}
		} else if !slices.Contains([]string{"project", "node", "user"}, x.ScopeKind) || !x.ScopeID.Valid() {
			return fmt.Errorf("invalid enablement scope")
		}
	default:
		return fmt.Errorf("unsupported human domain action")
	}
	if len(a.Request) == 0 || len(a.Request) > 64<<10 {
		return fmt.Errorf("bounded complete request required")
	}
	doc, err := canonjson.Decode(a.Request)
	if err != nil {
		return err
	}
	if _, ok := doc.(map[string]any); !ok {
		return fmt.Errorf("request must be an object")
	}
	return nil
}

func (a HumanAction) binding() (binding, error) {
	return bind(&domainBatch{Items: []HumanAction{a}})
}

// HumanTargets resolves each target against its owning module before a host
// displays a challenge. It verifies actual project/scope and the identity of the frozen revision;
// historical revisions remain valid challenge targets for partial-batch recovery.
// Current revision/state is checked only by the final domain command.
// it must not accept a caller's project label as evidence of ownership.
// This internal read port inherits the identity guard and may not reacquire it.
type HumanTargets interface {
	ValidateHumanTarget(context.Context, authz.Context, HumanAction) error
}

type domainBatch struct {
	Items     []HumanAction `json:"items"`
	validator HumanTargets
	who       authz.Context
}

func (b *domainBatch) action() authz.Action { return b.Items[0].Action }
func (b *domainBatch) project() ids.ID      { return b.Items[0].ProjectID }
func (b *domainBatch) body() any            { return b }
func (b *domainBatch) targets() []Target {
	out := make([]Target, len(b.Items))
	for i, a := range b.Items {
		out[i] = Target{Kind: "domain_target", ID: string(a.ResourceID), ExpectedRevision: a.ResourceRevision}
	}
	return out
}
func (b *domainBatch) check() error {
	if len(b.Items) == 0 || len(b.Items) > 100 {
		return fmt.Errorf("batch must contain 1..100 items")
	}
	seen := map[ids.ID]bool{}
	for _, a := range b.Items {
		if err := a.validate(); err != nil {
			return err
		}
		if a.Action != b.Items[0].Action || a.ProjectID != b.Items[0].ProjectID || seen[a.ResourceID] {
			return fmt.Errorf("batch must have one action/project and unique targets")
		}
		seen[a.ResourceID] = true
	}
	return nil
}
func (b *domainBatch) describe(ctx context.Context, _ commands.DBTX) (string, error) {
	if b.validator == nil {
		return "", fixRequest("authoritative human target validator required")
	}
	for _, a := range b.Items {
		if err := b.validator.ValidateHumanTarget(ctx, b.who, a); err != nil {
			return "", err
		}
	}
	return "Authorize exact domain requests: " + jsonText(b), nil
}
func (b *domainBatch) apply(context.Context, *applyEnv) (any, error) {
	return nil, errcode.New(errcode.Forbidden, "domain effects must be committed by their owning module")
}

// CreateDomainChallenge uses the host-owned TOTP flow. No grant is given to
// workers, plugins, delegated humans or recovery sessions. Child IDs survive
// revalidation and are returned by DomainItems after verification.
func (s *Service) CreateDomainChallenge(ctx context.Context, who authz.Context, items []HumanAction, targets HumanTargets) (Challenge, error) {
	return s.CreateChallenge(ctx, who, &domainBatch{Items: items, validator: targets, who: who})
}
func (s *Service) RechallengeDomain(ctx context.Context, who authz.Context, op ids.ID, targets HumanTargets) (Challenge, error) {
	// Verify before reading the frozen request; challenge() verifies ownership.
	if _, err := s.humanSession(ctx, who); err != nil {
		return Challenge{}, err
	}
	var raw string
	err := s.main.QueryRowContext(ctx, `SELECT b.request_json FROM identity_human_batches b WHERE b.operation_id=? AND EXISTS(SELECT 1 FROM identity_challenges c WHERE c.operation_id=b.operation_id AND c.human_id=?)`, op, who.PrincipalID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return Challenge{}, notFound("batch")
	}
	if err != nil {
		return Challenge{}, err
	}
	var b domainBatch
	if err = json.Unmarshal([]byte(raw), &b); err != nil {
		return Challenge{}, err
	}
	b.validator, b.who = targets, who
	return s.Rechallenge(ctx, who, op, &b)
}

// persistDomainBatch runs in the challenge transaction. It records authorization
// inputs only; no copy of a domain's committed status is kept in identity.
func persistDomainBatch(ctx context.Context, tx *sql.Tx, op ids.ID, cmd Command) error {
	b, ok := cmd.(*domainBatch)
	if !ok {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO identity_human_batches VALUES (?,?) ON CONFLICT(operation_id) DO NOTHING`, op, jsonText(b))
	return err
}
func grantDomainItems(ctx context.Context, tx *sql.Tx, op, gid ids.ID) error {
	var raw string
	err := tx.QueryRowContext(ctx, `SELECT request_json FROM identity_human_batches WHERE operation_id=?`, op).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var b domainBatch
	if err = json.Unmarshal([]byte(raw), &b); err != nil {
		return err
	}
	for i, a := range b.Items {
		child, e := ids.DeriveChild(op, fmt.Sprintf("human-item:%d", i))
		if e != nil {
			return e
		}
		binding, e := a.binding()
		if e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, `INSERT INTO identity_human_grant_items VALUES (?,?,?,?,?,?)`, gid, child, a.Action, binding.targetDigest, binding.requestHash, a.ResourceRevision); e != nil {
			return e
		}
	}
	return nil
}

type HumanGrantItem struct {
	OperationID ids.ID      `json:"operation_id"`
	Action      HumanAction `json:"action"`
}

func (s *Service) DomainItems(ctx context.Context, who authz.Context, grantID ids.ID) ([]HumanGrantItem, error) {
	v, err := s.humanSession(ctx, who)
	if err != nil {
		return nil, err
	}
	g, err := loadGrant(ctx, s.main, grantID)
	if err != nil {
		return nil, notFound("grant")
	}
	if g.HumanID != v.principal.ID || g.SessionID != v.sess.ID {
		return nil, errcode.New(errcode.HumanGrantMismatch, "")
	}
	var raw string
	if err = s.main.QueryRowContext(ctx, `SELECT request_json FROM identity_human_batches WHERE operation_id=?`, g.OperationID).Scan(&raw); err != nil {
		return nil, err
	}
	var b domainBatch
	if err = json.Unmarshal([]byte(raw), &b); err != nil {
		return nil, err
	}
	if err = s.require(ctx, s.main, v, b.action(), authz.Resource{ProjectID: b.project()}); err != nil {
		return nil, err
	}
	out := make([]HumanGrantItem, len(b.Items))
	for i, a := range b.Items {
		id, e := ids.DeriveChild(g.OperationID, fmt.Sprintf("human-item:%d", i))
		if e != nil {
			return nil, e
		}
		out[i] = HumanGrantItem{id, a}
	}
	return out, nil
}

// HumanDomain is a trusted in-process T03/T09 adapter. Receipt returns the owning
// module's durable receipt (commands.ErrNotFound if absent). Commit must atomically
// persist its effects, receipt and outbox and recheck domain revision/lease/fence.
// No database handle crosses this boundary. Neither method may call plugins.
type HumanDomain interface {
	Receipt(context.Context, ids.ID) (commands.Receipt, error)
	Commit(context.Context, commands.Context, HumanAction) (commands.Receipt, error)
}

// AcceptHumanItem serializes final proof checking and business acceptance with
// revocation. Replays require a current human session and current permission, but
// do not need another TOTP. Unfinished items always check the unexpired grant.
func (s *Service) AcceptHumanItem(ctx context.Context, who authz.Context, grantID, child ids.ID, action HumanAction, locks commands.Request, domain HumanDomain) (commands.Receipt, error) {
	var zero commands.Receipt
	if domain == nil {
		return zero, fixRequest("domain adapter required")
	}
	b, err := action.binding()
	if err != nil {
		return zero, err
	}
	if locks.Security != commands.ModeExclusive {
		locks.Security = commands.ModeShared
	}
	locks.Barrier = commands.ModeShared
	ctx, h, err := s.gate.Acquire(ctx, locks)
	if err != nil {
		return zero, err
	}
	defer h.Release()
	v, err := s.humanSession(ctx, who)
	if err != nil {
		return zero, err
	}
	g, err := loadGrant(ctx, s.main, grantID)
	if err != nil {
		return zero, proofErr("grant_missing", "human authorization required")
	}
	if g.HumanID != v.principal.ID || g.SessionID != v.sess.ID {
		return zero, errcode.New(errcode.HumanGrantMismatch, "")
	}
	var act authz.Action
	var td, rh digest.Digest
	var rev int64
	err = s.main.QueryRowContext(ctx, `SELECT action,target_digest,request_hash,expected_revision FROM identity_human_grant_items WHERE grant_id=? AND child_operation_id=?`, grantID, child).Scan(&act, &td, &rh, &rev)
	if errors.Is(err, sql.ErrNoRows) {
		return zero, errcode.New(errcode.HumanGrantMismatch, "item not in approved batch")
	}
	if err != nil {
		return zero, err
	}
	if act != b.action || td != b.targetDigest || rh != b.requestHash || rev != action.ResourceRevision || g.Action != b.action || g.Project != b.project {
		return zero, errcode.New(errcode.HumanGrantMismatch, "item request differs from approved request")
	}
	if err = s.require(ctx, s.main, v, act, authz.Resource{ProjectID: b.project}); err != nil {
		return zero, err
	}
	epoch, err := s.recoveryEpoch(ctx)
	if err != nil {
		return zero, err
	}
	cc := commands.Context{OperationID: child, IdempotencyKey: string(child), CommandType: string(act), ActorID: v.principal.ID, SessionID: v.sess.ID, ProjectID: b.project, RequestHash: rh, PolicyRevision: v.policyRev, RecoveryEpoch: epoch, HumanGrantID: grantID, CorrelationID: g.OperationID, ExpectedRevisions: map[string]int64{string(action.ResourceID): rev}}
	if err = cc.Validate(); err != nil {
		return zero, err
	}
	prior, err := domain.Receipt(ctx, child)
	if err == nil {
		if prior.OperationID != child || prior.Key != cc.Key() || prior.RequestHash != rh {
			return zero, errcode.New(errcode.IdempotencyConflict, "")
		}
		switch commands.Decide(&prior, rh, s.now()) {
		case commands.OutcomeReplay:
			return prior, nil
		case commands.OutcomeExpired:
			return zero, errcode.New(errcode.IdempotencyResultExpired, "")
		}
	} else if !errors.Is(err, commands.ErrNotFound) {
		return zero, err
	}
	// Parent binding is fixed in the grant; item binding was checked independently.
	if err = s.checkGrant(ctx, g, v, binding{action: g.Action, project: g.Project, targetDigest: g.TargetDigest, requestHash: g.RequestHash}); err != nil {
		return zero, err
	}
	return domain.Commit(ctx, cc, action)
}
