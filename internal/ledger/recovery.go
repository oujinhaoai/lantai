package ledger

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/pin"
)

// RecoveryOperation exposes a durable intent to the trusted maintenance dispatcher.
// Command retains its original actor/session/epoch; recovery must not replace them.
type RecoveryOperation struct {
	Operation        commands.Operation
	Command          commands.Context
	PreparedVersion  *commit.Prepared
	PreparedMetadata *commit.PreparedMetadata
}

type RecoveryInventory struct {
	Open     []RecoveryOperation
	Versions []commit.Committed
	Metadata []commit.CommittedMetadata
	Pins     []pin.Pin
}

// RecoveryInventory reads only ledger-owned facts. A caller requiring a common
// backup point must hold the instance maintenance barrier for this entire call.
func (s *Service) RecoveryInventory(ctx context.Context) (RecoveryInventory, error) {
	var out RecoveryInventory
	open, err := s.OpenOperations(ctx)
	if err != nil {
		return out, err
	}
	for _, id := range open {
		op, err := s.store.GetOperation(ctx, s.db, id)
		if err != nil {
			return out, err
		}
		r := RecoveryOperation{Operation: *op}
		switch op.CommandType {
		case commit.CommandType:
			o, err := s.versionOp(ctx, s.db, id)
			if err != nil {
				return out, err
			}
			r.Command, r.PreparedVersion = o.Command, &o.Prepared
		case commit.CommandCommitMetadata:
			o, err := s.metadataOp(ctx, s.db, id)
			if err != nil {
				return out, err
			}
			r.Command, r.PreparedMetadata = o.Command, &o.Prepared
		default:
			return out, invalid("unknown ledger recovery command")
		}
		out.Open = append(out.Open, r)
	}
	for after := ids.ID(""); ; {
		batch, err := s.Versions(ctx, after, 1000)
		if err != nil {
			return out, err
		}
		out.Versions = append(out.Versions, batch...)
		if len(batch) < 1000 {
			break
		}
		after = batch[len(batch)-1].VersionID
	}
	rows, err := s.db.QueryContext(ctx, `SELECT record FROM ledger_metadata_revisions ORDER BY kind,target_id,revision`)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var raw string
		var m commit.CommittedMetadata
		if err = rows.Scan(&raw); err == nil {
			err = json.Unmarshal([]byte(raw), &m)
		}
		if err != nil {
			rows.Close()
			return out, err
		}
		out.Metadata = append(out.Metadata, m)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	out.Pins, err = s.commitPins(ctx)
	return out, err
}

var _ pin.Source = (*Service)(nil)

// PinsFor derives commit retention from the durable prepared intent and its
// operation. There is no TTL and no independent, potentially stale pin table.
// A missing operation or malformed intent fails closed rather than losing a pin.
func (s *Service) PinsFor(ctx context.Context, sha string) ([]pin.Pin, error) {
	if !digest.ValidHex(sha) {
		return nil, invalid("invalid blob digest")
	}
	all, err := s.commitPins(ctx)
	if err != nil {
		return nil, err
	}
	var out []pin.Pin
	for _, p := range all {
		if slices.Contains(p.Blobs, sha) {
			out = append(out, p)
		}
	}
	return out, nil
}

func (s *Service) commitPins(ctx context.Context) ([]pin.Pin, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT operation_id FROM ledger_prepared ORDER BY operation_id`)
	if err != nil {
		return nil, err
	}
	var all []ids.ID
	for rows.Next() {
		var id ids.ID
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		all = append(all, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	var out []pin.Pin
	for _, id := range all {
		o, err := s.versionOp(ctx, s.db, id)
		if err != nil {
			return nil, err
		}
		op, err := s.store.GetOperation(ctx, s.db, id)
		if err != nil {
			return nil, err
		}
		if op.OwnerModule != Module || op.CommandType != commit.CommandType || o.Command.OperationID != id || o.Prepared.OperationID != id ||
			o.Command.RequestHash != op.RequestHash || o.Command.RecoveryEpoch != op.RecoveryEpoch || !op.Stage.Valid() {
			return nil, invalid("commit retention intent disagrees with its operation")
		}
		if err = o.Command.Validate(); err != nil {
			return nil, err
		}
		if err = o.Prepared.InstallRequest().Validate(); err != nil {
			return nil, err
		}
		pinID, err := ids.DeriveChild(id, "pin:commit")
		if err != nil {
			return nil, err
		}
		p := pin.Pin{PinID: pinID, Kind: pin.KindCommit, OwnerModule: Module, Owner: pin.Owner{Kind: "operation", ID: id}, CreatedAt: op.CreatedAt}
		for _, f := range o.Prepared.Files {
			p.Blobs = append(p.Blobs, f.SHA256)
		}
		slices.Sort(p.Blobs)
		p.Blobs = slices.Compact(p.Blobs)
		// These transitions durably release the reservation. Blocked and needs-
		// reconciliation intents retain all bytes regardless of elapsed time.
		if op.Stage.Final() || op.Stage == commands.StageQuarantined {
			p.ReleasedAt = op.UpdatedAt
		}
		if err = p.Validate(); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}
