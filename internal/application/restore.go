package application

import (
	"context"
	"errors"
	"github.com/oujinhaoai/lantai/internal/platform/fsutil"
	"io"
	"os"
	"path/filepath"

	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/identity/masterkey"
	"github.com/oujinhaoai/lantai/internal/operations"
)

// Restore copies only a complete archive, verifies the separately recovered old
// key against encrypted factors, revokes all restored credentials, then installs
// a fresh key. It returns with a durable maintenance latch, never a running App.
func Restore(ctx context.Context, opts operations.RestoreOptions, key *masterkey.Key, cfg identity.Config) (operations.Marker, error) {
	if key == nil {
		return operations.Marker{}, errors.New("application: separately recovered master key is required")
	}
	opts.KeyID = key.ID()
	marker, err := operations.RestoreTo(ctx, opts)
	if err != nil {
		return marker, err
	}
	inst, err := operations.Open(ctx, operations.Options{Home: opts.Home})
	if err != nil {
		return marker, err
	}
	defer inst.Close(context.Background())
	if _, err = inst.Migrate(ctx, opts.BackupDir); err != nil {
		return inst.Marker(), err
	}
	err = inst.OfflineMaintenance(ctx, func(c context.Context) error {
		r := inst.Marker().Restore
		if r == nil {
			return errors.New("application: missing restore latch")
		}
		if r.Stage == "credentials_required" {
			old, err := identity.New(identity.Deps{Main: inst.DB(ownership.Main), Runtime: inst.DB(ownership.Runtime), Gate: inst.Gate(), Epochs: inst, Clock: inst.Clock(), IDs: inst.IDs(), Key: key, InstanceID: inst.InstanceID(), InstanceName: inst.Marker().Name}, cfg)
			if err != nil {
				return err
			}
			if err = old.CheckStartup(c); err != nil {
				return err
			}
			if err = old.InvalidateForRestore(c, r.RunID, inst.Marker().RecoveryEpoch); err != nil {
				return err
			}
			if err = inst.SetRestoreStage(c, "key_required"); err != nil {
				return err
			}
		}
		if inst.Marker().Restore.Stage == "key_required" {
			fresh, err := masterkey.Load(inst.Config().SecretsDir)
			if errors.Is(err, masterkey.ErrMissing) {
				fresh, err = masterkey.Generate(inst.IDs(), inst.Clock(), nil)
				if err == nil {
					err = masterkey.Save(inst.Config().SecretsDir, fresh)
				}
			}
			if err != nil {
				return err
			}
			if fresh.ID() == key.ID() {
				return errors.New("application: recovered credentials require a new master key")
			}
			if err = inst.SetRestoreStage(c, "reconciliation_required"); err != nil {
				return err
			}
		}
		return nil
	})
	return inst.Marker(), err
}

// CompleteRestore records explicit local reconciliation of changes newer than
// the archive. The evidence digest binds a separately supplied report. A fresh
// administrator, owner fsck, outbox/audit and rebuilt index are all mandatory.
func (a *App) CompleteRestore(ctx context.Context, review operations.RecoveryReview, evidence []byte) error {
	if len(evidence) == 0 || len(evidence) > 8<<20 || digest.Of(evidence) != review.EvidenceDigest {
		return errors.New("application: recovery review evidence digest mismatch")
	}
	return a.Maintenance(ctx, func(c context.Context) error {
		return a.Instance.CompleteRestore(c, review, func(c context.Context) error {
			admin := ids.ID(review.Administrator)
			if !admin.Valid() {
				return errors.New("application: recovery administrator must be a principal ID")
			}
			if err := a.Identity.VerifyRestoreAdministrator(c, review.RunID, admin); err != nil {
				return err
			}
			if err := a.Identity.CheckStartup(c); err != nil {
				return err
			}
			if err := a.Extensions.RegisterBuiltins(c); err != nil {
				return err
			}
			if _, err := a.Recover(c); err != nil {
				return err
			}
			rep, err := a.FSCK(c, true)
			if err != nil {
				return err
			}
			if err = rep.Err(); err != nil {
				return err
			}
			if err = a.sync(c, true); err != nil {
				return err
			}
			_, err = a.Query.Rebuild(c)
			if err != nil {
				return err
			}
			dir := filepath.Join(a.Instance.Layout().Home, "logs")
			if err = os.MkdirAll(dir, 0700); err != nil {
				return err
			}
			return fsutil.WriteFileAtomic(filepath.Join(dir, "restore-"+string(review.RunID)+"-evidence.txt"), evidence, 0600)
		})
	})
}

// ReadRecoveryEvidence is bounded and never follows a path from the archive.
func ReadRecoveryEvidence(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Size() > 8<<20 {
		return nil, errors.New("application: reconciliation evidence must be a regular file of at most 8 MiB")
	}
	b, err := io.ReadAll(io.LimitReader(f, 8<<20+1))
	if len(b) > 8<<20 {
		return nil, errors.New("application: reconciliation evidence exceeds 8 MiB")
	}
	return b, err
}
