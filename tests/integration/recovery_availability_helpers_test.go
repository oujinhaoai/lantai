package integration

import (
	"context"
	"encoding/base32"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/application"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/identity/totp"
	"github.com/oujinhaoai/lantai/internal/operations"
)

func availabilityWork(t *testing.T) string {
	t.Helper()
	if root := os.Getenv("LANTAI_AVAILABILITY_EVIDENCE_DIR"); root != "" {
		dir := filepath.Join(root, strings.ReplaceAll(t.Name(), "/", "-"))
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal("new evidence directory required", err)
		}
		return dir
	}
	return assetRealTempDir(t)
}

// Capture through the active core coordinator, stop it, and restore only the
// complete package into an empty directory. Old data is moved out of reach;
// identity setup and recovery acceptance use the normal offline owner APIs.
func restoreAvailability(t *testing.T, e *env, a *application.App, s *application.HTTPServers) *application.App {
	t.Helper()
	ctx := t.Context()
	work := assetRealTempDir(t)
	backup, err := a.Backup(ctx, operations.BackupOptions{Destination: filepath.Join(work, "backup")})
	if err != nil {
		t.Fatal(err)
	}
	if s != nil {
		if err = s.Shutdown(ctx); err != nil {
			t.Fatal(err)
		}
	}
	oldHome := a.Instance.Layout().Home
	if err = a.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(oldHome, oldHome+".offline"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(oldHome + ".offline") })
	home := filepath.Join(work, "restored")
	marker, err := application.RestorePackage(ctx, operations.RecoveryPackagePath(filepath.Join(work, "backup")), operations.RestoreOptions{Home: home}, identity.Config{Password: fastPassword})
	if err != nil {
		t.Fatal(err)
	}
	opts := application.Options{Instance: operations.Options{Home: home, Clock: e.clk}, Identity: identity.Config{Password: fastPassword}}
	target, err := application.OpenOffline(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = target.Close(context.Background()) })
	var secret []byte
	err = target.Maintenance(ctx, func(c context.Context) error {
		reset, er := target.Identity.BeginOfflineReset(c, "ada", adminPassword)
		if er != nil {
			return er
		}
		if _, er = reset.PrepareRecoveryCodes(); er != nil {
			return er
		}
		secret, er = base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(reset.Enrollment.Secret)
		if er != nil {
			return er
		}
		if _, er = reset.Confirm(c, totp.Code(secret, totp.Counter(e.clk.Now())), "synthetic availability recovery"); er != nil {
			return er
		}
		recovered, er := target.Recover(c)
		if er != nil {
			return er
		}
		if len(recovered.NeedsReconciliation) != 0 {
			return fmt.Errorf("unresolved recovery operations: %d", len(recovered.NeedsReconciliation))
		}
		fsck, er := target.FSCK(c, true)
		if er != nil {
			return er
		}
		return fsck.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	proof := mustJSON(map[string]any{"core_review": map[string]any{"run_id": marker.Restore.RunID, "automatic_checks_passed": true, "unresolved_operations": 0}, "operator_accepted_recovery_point": true, "operator_confirmed_known_changes_reviewed": true, "operator_confirmed_source_offline": true, "unknown_after_capture_verified": false})
	review := operations.RecoveryReview{Contract: "lantai.recovery-review/v2", RunID: marker.Restore.RunID, BackupID: backup.BackupID, ManifestDigest: marker.Restore.ManifestDigest, RecoveryEpoch: marker.RecoveryEpoch, Administrator: string(e.admin.Context.PrincipalID), AcceptRecoveryPoint: true, UnknownAfterCapture: true, EvidenceDigest: digest.Of(proof), Note: "Synthetic source stopped; preserve captured governance and bytes."}
	if err = target.CompleteRestore(ctx, review, proof); err != nil {
		t.Fatal(err)
	}
	if err = target.Close(ctx); err != nil {
		t.Fatal(err)
	}
	target, err = application.Open(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	e.inst, e.id, e.catalog, e.storage, e.ledger, e.rights = target.Instance, target.Identity, target.Catalog, target.Storage, target.Ledger, target.Rights
	e.secret = secret
	e.clk.Advance(30 * time.Second)
	e.admin = e.login()
	t.Logf("complete-package recovery: instance=%s epoch=%d; source stopped; deep FSCK and explicit acceptance passed", target.Instance.InstanceID(), marker.RecoveryEpoch)
	return target
}
func availabilitySession(t *testing.T, e *env, id ids.ID, scopes []identity.Scope) identity.IssuedSession {
	t.Helper()
	p, err := e.id.GetPrincipal(t.Context(), e.login().Context, id)
	if err != nil {
		t.Fatal(err)
	}
	issued := e.sudo(&identity.IssueCredential{PrincipalID: p.ID, ExpectedRevision: p.Revision, Scopes: scopes})
	s, err := e.id.ExchangeToken(t.Context(), issued.Secret, identity.SessionRequest{Channel: identity.ChannelCLI})
	if err != nil {
		t.Fatal(err)
	}
	return s
}
