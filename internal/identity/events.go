package identity

import (
	"github.com/oujinhaoai/lantai/internal/contract/event"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

// 身份模块的事件类型。payload 只含状态与引用，不含口令、种子、恢复码、
// 令牌或其摘要。
const (
	EvBootstrapCompleted  = "principal.bootstrap_completed"
	EvPrincipalRegistered = "principal.registered"
	EvPrincipalUpdated    = "principal.updated"
	EvPrincipalDisabled   = "principal.disabled"
	EvPrincipalEnabled    = "principal.enabled"
	EvCredentialIssued    = "principal.credential_issued"
	EvCredentialRevoked   = "principal.credential_revoked"
	EvSystemRoleGranted   = "principal.system_role_granted"
	EvSystemRoleRevoked   = "principal.system_role_revoked"
	EvFactorReset         = "principal.factor_reset"
	EvRecoveryStarted     = "principal.recovery_started"
	EvFactorEnrolled      = "principal.factor_enrolled"
	EvOfflineRecovery     = "principal.offline_recovery"
	EvProjectRoleGranted  = "project.role_granted"
	EvProjectRoleRevoked  = "project.role_revoked"
	EvProjectPolicySet    = "project.policy_set"
	EvSystemPolicySet     = "policy.system_policy_set"
	EvHumanGrantIssued    = "human_grant.issued"
	EvSessionStarted      = "session.started"
	EvSessionEnded        = "session.ended"
	EvSessionRevoked      = "session.revoked"
)

type evParams struct {
	typ       string
	aggType   string
	aggID     ids.ID
	revision  int64
	actor     ids.ID
	session   ids.ID
	project   ids.ID
	operation ids.ID
	payload   any
}

func (s *Service) event(p evParams) (event.Envelope, error) {
	return event.New(s.ids, s.clock, event.Params{
		EventType: p.typ, SchemaVersion: 1, AggregateType: p.aggType, AggregateID: p.aggID,
		AggregateRevision: p.revision, ActorID: p.actor, SessionID: p.session, ProjectID: p.project,
		OperationID: p.operation, Payload: p.payload,
	})
}
