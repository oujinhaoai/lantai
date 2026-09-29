package provenance

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/event"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

type CancelAssertionRequest struct {
	ProjectID   ids.ID        `json:"project_id"`
	OperationID ids.ID        `json:"operation_id"`
	RequestHash digest.Digest `json:"request_hash"`
	Reason      string        `json:"reason"`
}
type CancelAssertionReceipt struct {
	OperationID          ids.ID `json:"operation_id"`
	CancelledOperationID ids.ID `json:"cancelled_operation_id"`
	Status               string `json:"status"`
}

// CancelAssertion terminates an exact unaccepted intent. It never undoes an
// active restriction, removes evidence bytes or transfers an old human grant.
// Owner authorization is current; no resource contents are returned. This also
// permits reconciliation after the old session/epoch or target became invalid.
func (s *Service) CancelAssertion(ctx context.Context, who authz.Context, key string, in CancelAssertionRequest) (CancelAssertionReceipt, error) {
	var out CancelAssertionReceipt
	if !in.ProjectID.Valid() || !in.OperationID.Valid() || !in.RequestHash.Valid() || strings.TrimSpace(in.Reason) == "" || len(in.Reason) > 4096 {
		return out, errcode.New(errcode.SchemaInvalid, "")
	}
	raw, err := canonjson.CanonicalizeValue(in)
	if err != nil {
		return out, err
	}
	hash, err := commands.RequestHash(commands.HashInput{CommandType: "provenance.cancel_assertion", ProjectID: in.ProjectID, Body: raw})
	if err != nil {
		return out, err
	}
	op, err := s.IDs.New()
	if err != nil {
		return out, err
	}
	cmd := commands.Context{OperationID: op, IdempotencyKey: key, CommandType: "provenance.cancel_assertion", RequestHash: hash, ActorID: who.PrincipalID, SessionID: who.SessionID, ProjectID: in.ProjectID, PolicyRevision: who.PolicyRevision, RecoveryEpoch: who.RecoveryEpoch}
	ctx, h, err := s.Gate.Acquire(ctx, commands.Request{Security: commands.ModeExclusive, Projects: []string{string(in.ProjectID)}})
	if err != nil {
		return out, err
	}
	defer h.Release()
	d, err := s.Authz.Authorize(ctx, who, "provenance.cancel_assertion", authz.Resource{ProjectID: in.ProjectID, Kind: "project", ID: in.ProjectID})
	if err != nil {
		return out, err
	}
	if err = d.Err(); err != nil {
		return out, err
	}
	response, err := s.store.Execute(ctx, s.DB, cmd, func(ctx context.Context, tx *sql.Tx) (commands.Result, error) {
		original, err := s.store.GetOperation(ctx, tx, in.OperationID)
		if err != nil {
			return commands.Result{}, err
		}
		receipt, err := s.store.ReceiptByOperation(ctx, tx, in.OperationID)
		if err != nil {
			return commands.Result{}, err
		}
		if original.OwnerModule != "provenance" || receipt.Key.ProjectID != in.ProjectID || (original.CommandType != "provenance.restrict" && original.CommandType != "provenance.assert_evidence" && original.CommandType != "provenance.release_restriction") {
			return commands.Result{}, errcode.New(errcode.NotFound, "")
		}
		if original.RequestHash != in.RequestHash {
			return commands.Result{}, errcode.New(errcode.PreconditionFailed, "")
		}
		if original.Stage != commands.StagePrepared || receipt.Status != commands.ReceiptInProgress {
			return commands.Result{}, errcode.New(errcode.InvalidStateTransition, "only unaccepted assertion intents can be cancelled")
		}
		if err = s.store.Cancel(ctx, tx, in.OperationID, commands.StagePrepared, errcode.InvalidStateTransition); err != nil {
			return commands.Result{}, err
		}
		removed, err := tx.ExecContext(ctx, `DELETE FROM provenance_assertion_prepared WHERE operation_id=?`, in.OperationID)
		if err != nil {
			return commands.Result{}, err
		}
		n, err := removed.RowsAffected()
		if err != nil {
			return commands.Result{}, err
		}
		if n != 1 {
			return commands.Result{}, errcode.New(errcode.OperationNeedsReconciliation, "assertion intent missing")
		}
		out = CancelAssertionReceipt{OperationID: op, CancelledOperationID: in.OperationID, Status: "cancelled"}
		ev, err := event.New(s.IDs, s.Clock, event.Params{EventType: "rights.assertion_cancelled", SchemaVersion: 1, AggregateType: "assertion_operation", AggregateID: in.OperationID, AggregateRevision: 1, ActorID: who.PrincipalID, SessionID: who.SessionID, ProjectID: in.ProjectID, OperationID: op, Payload: map[string]any{"operation_id": in.OperationID, "reason": in.Reason}})
		if err != nil {
			return commands.Result{}, err
		}
		return commands.Result{Status: commands.ReceiptSucceeded, ResponseCode: 200, Summary: out, Events: []event.Envelope{ev}}, nil
	})
	if err != nil {
		return out, err
	}
	return decodeCancellation(response.Receipt)
}

func decodeCancellation(r *commands.Receipt) (CancelAssertionReceipt, error) {
	var out CancelAssertionReceipt
	err := json.Unmarshal(r.ResponseSummary, &out)
	return out, err
}
