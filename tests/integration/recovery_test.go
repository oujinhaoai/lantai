package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/application"
	"github.com/oujinhaoai/lantai/internal/catalog"
	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/operations"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

type interruptedAcceptance struct{}

func (interruptedAcceptance) VerifyAcceptance(context.Context, authz.Context, commit.Prepared) error {
	return errcode.New(errcode.StorageUnavailable, "synthetic interruption before ledger acceptance")
}

func recoveryPending(t *testing.T, e *env, who authz.Context) (commit.Prepared, []byte) {
	t.Helper()
	body := []byte("synthetic durable pending bytes")
	sha := shaOf(body)
	u, err := e.storage.CreateUpload(t.Context(), storage.CreateUploadRequest{Who: who, IdempotencyKey: e.key(), ProjectID: e.project.ProjectID, Files: []storage.FileSpec{{SHA256: sha, Size: int64(len(body))}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.storage.PutPart(t.Context(), storage.PartRequest{Who: who, UploadID: u.UploadID, SHA256: sha, PartNumber: 1, PartSHA256: sha, Size: int64(len(body)), Body: bytes.NewReader(body)}); err != nil {
		t.Fatal(err)
	}
	if _, err = e.storage.CompleteFile(t.Context(), who, u.UploadID, sha); err != nil {
		t.Fatal(err)
	}
	e.ledger.SetAcceptanceVerifier(interruptedAcceptance{})
	_, err = e.catalog.CommitVersion(t.Context(), catalog.VersionRequest{Who: who, IdempotencyKey: e.key(), UploadID: u.UploadID, Slug: "recovery/pending", Content: catalog.ContentInput{AssetType: manifest.TypeDoc, Rights: rightsOwned(), Files: []manifest.InputFile{{Path: "a.md", Role: "primary", SHA256: sha, Size: int64(len(body))}}}})
	if errcode.CodeOf(err) != errcode.StorageUnavailable {
		t.Fatalf("interruption: %v", err)
	}
	p, err := e.ledger.PreparedOperation(t.Context(), u.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	e.ledger.SetAcceptanceVerifier(e.catalog)
	return p, body
}

func TestRecoveryDispatcherRestartsSameOperationAndMetrics(t *testing.T) {
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	_, session := e.agent("recovery@node-a", identity.RoleContributor)
	p, _ := recoveryPending(t, e, session.Context)
	if _, err := e.ledger.Version(t.Context(), p.AssetID, p.VersionID); errcode.CodeOf(err) != errcode.NotFound {
		t.Fatalf("installed became committed early: %v", err)
	}
	a := reopenApplication(t, e)
	v, err := a.Ledger.Version(t.Context(), p.AssetID, p.VersionID)
	if err != nil || v.OperationID != p.OperationID || v.VersionNumber != 1 {
		t.Fatalf("restart created different result: %+v %v", v, err)
	}
	check, err := a.FSCK(t.Context(), true)
	if err != nil || check.Err() != nil {
		t.Fatalf("healthy fsck: %+v %v", check.Findings, err)
	}
	pins, err := a.BlobRetention(t.Context(), p.Files[0].SHA256)
	if err != nil {
		t.Fatal(err)
	}
	commitReleased := false
	for _, p := range pins {
		if p.OwnerModule == "ledger" && !p.ReleasedAt.IsZero() {
			commitReleased = true
		}
	}
	if !commitReleased {
		t.Fatal("commit pin not explicitly released after recovered commit")
	}
	s := remoteServers(t, a, false)
	resp, err := http.Get("http://" + s.Addresses.Operations + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("metrics: %d %s", resp.StatusCode, b)
	}
	for _, want := range []string{`lantai_collector_available{collector="storage"} 1`, `lantai_collector_available{collector="backups"} 1`, "lantai_backup_pins 0", "lantai_backups_complete 0", "lantai_gc_supported 0"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("missing %s: %s", want, b)
		}
	}
}

func TestRecoveryExpiredAndOldEpochRemainPendingWithPins(t *testing.T) {
	for _, mode := range []string{"expired", "old_epoch", "revoked"} {
		t.Run(mode, func(t *testing.T) {
			e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
			principal, session := e.agent("recovery@node-a", identity.RoleContributor)
			p, _ := recoveryPending(t, e, session.Context)
			if mode == "expired" {
				e.clk.Advance(8 * 24 * time.Hour)
			}
			if mode == "revoked" {
				e.setRole(principal.ID, identity.RoleContributor, false)
			}
			if mode == "old_epoch" {
				e.xfer.Close()
				if err := e.inst.Close(context.Background()); err != nil {
					t.Fatal(err)
				}
				marker := e.inst.Marker()
				marker.RecoveryEpoch++
				b, err := json.Marshal(marker)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(filepath.Join(e.inst.Layout().Home, "instance.json"), b, 0600); err != nil {
					t.Fatal(err)
				}
			}
			a := reopenApplication(t, e)
			if _, err := a.Ledger.Version(t.Context(), p.AssetID, p.VersionID); errcode.CodeOf(err) != errcode.NotFound {
				t.Fatalf("stale authority committed: %v", err)
			}
			m, err := a.Instance.Maintain(t.Context(), commands.ReasonRecovering)
			if err != nil {
				t.Fatal(err)
			}
			report, err := a.Recover(m.Context())
			m.End()
			if err != nil || len(report.NeedsReconciliation) != 1 || report.NeedsReconciliation[0].OperationID != p.OperationID {
				t.Fatalf("pending report: %+v %v", report, err)
			}
			pins, err := a.Ledger.PinsFor(t.Context(), p.Files[0].SHA256)
			if err != nil || len(pins) != 1 || !pins[0].ActiveAt(e.clk.Now().Add(365*24*time.Hour)) {
				t.Fatalf("pending pin lost: %+v %v", pins, err)
			}
		})
	}
}

func TestRecoveryRejectsCorruptCommittedBytesBeforeStartup(t *testing.T) {
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	_, session := e.agent("recovery@node-a", identity.RoleContributor)
	p, _ := recoveryPending(t, e, session.Context)
	a := reopenApplication(t, e)
	path := filepath.Join(a.Storage.Layout().VersionDir(p.ProjectID, p.AssetID, p.VersionNumber), storage.FilesDir, "a.md")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("corrupt installed bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	check, err := a.FSCK(t.Context(), true)
	if err != nil || check.Err() == nil || len(check.Findings) == 0 {
		t.Fatalf("corrupt authority accepted: %+v %v", check, err)
	}
	home := a.Instance.Layout().Home
	if err = a.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	broken, err := application.Open(t.Context(), application.Options{Instance: operations.Options{Home: home, Clock: e.clk}, Identity: identity.Config{Password: fastPassword}, Storage: storage.Config{MinFreeBytes: 1 << 20}})
	if err == nil {
		broken.Close(context.Background())
		t.Fatal("startup accepted corrupted committed bytes")
	}
	if _, err = os.Stat(path); err != nil {
		t.Fatal("fsck deleted evidence", err)
	}
}
