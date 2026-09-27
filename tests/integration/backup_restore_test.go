package integration

import (
	"bytes"
	"context"
	"encoding/base32"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/application"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/identity/masterkey"
	"github.com/oujinhaoai/lantai/internal/identity/totp"
	"github.com/oujinhaoai/lantai/internal/operations"
	"github.com/oujinhaoai/lantai/internal/query"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

func TestCommonBackupRestoresWithNewCredentialsAndRebuiltIndex(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	e.setRole(e.admin.Context.PrincipalID, identity.RoleViewer, true)
	principal, session := e.agent("backup@node-a", identity.RoleContributor)
	principal, err := e.id.GetPrincipal(ctx, e.login().Context, principal.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Capture a real issued machine credential, not a session-only stand-in.
	cred := e.sudo(&identity.IssueCredential{PrincipalID: principal.ID, ExpectedRevision: principal.Revision, Scopes: []identity.Scope{identity.ScopeRead}})
	body := []byte("synthetic restoration source bytes")
	version := e.ingest(session.Context, "backup/source", body, *rightsOwned())
	oldGrant, err := e.storage.IssueReadGrant(ctx, storage.ReadRequest{Who: session.Context, AssetID: version.AssetID, VersionID: version.VersionID, Path: "content.txt", Purpose: "archive_review"})
	if err != nil {
		t.Fatal(err)
	}
	a := reopenApplication(t, e)
	oldKey, err := masterkey.Load(a.Instance.Config().SecretsDir)
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "complete")
	m, err := a.Backup(ctx, operations.BackupOptions{Destination: archive})
	if err != nil {
		t.Fatal(err)
	}
	if m.State != "complete" || m.Watermarks["events"] == 0 {
		t.Fatalf("missing frozen state: %+v", m)
	}
	for _, p := range []string{"db/index.db", "secrets/master.key"} {
		if _, err = os.Stat(filepath.Join(archive, p)); !os.IsNotExist(err) {
			t.Fatalf("forbidden backup member %s: %v", p, err)
		}
	}
	metrics, err := a.Events.Metrics(ctx)
	if err != nil || metrics.BackupThrough != m.Watermarks["events"] {
		t.Fatalf("backup ack: %+v %v", metrics, err)
	}
	// A later revocation is deliberately absent from the chosen archive. Restoring
	// must revoke all copied credentials rather than resurrect this token.
	e.setRole(principal.ID, identity.RoleContributor, false)
	if err = a.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "restored")
	marker, err := application.Restore(ctx, operations.RestoreOptions{Home: target, BackupDir: archive, MinimumEpoch: m.RecoveryEpoch + 2}, oldKey, identity.Config{Password: fastPassword})
	if err != nil {
		t.Fatal(err)
	}
	if marker.Restore == nil || marker.Restore.Stage != "reconciliation_required" || marker.RecoveryEpoch != m.RecoveryEpoch+3 {
		t.Fatalf("latch/epoch: %+v", marker)
	}
	opts := application.Options{Instance: operations.Options{Home: target, Clock: e.clk}, Identity: identity.Config{Password: fastPassword}, Storage: storage.Config{MinFreeBytes: 1 << 20}}
	if opened, err := application.Open(ctx, opts); err == nil {
		opened.Close(context.Background())
		t.Fatal("restored instance opened before reconciliation")
	}
	r, err := application.OpenOffline(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close(context.Background()) })
	key, err := masterkey.Load(r.Instance.Config().SecretsDir)
	if err != nil || key.ID() == oldKey.ID() {
		t.Fatalf("key not rotated: %v", err)
	}
	evidence := []byte("Synthetic rehearsal: later credential revocation is covered by revoking every archived credential; no later deletion; open operations inspected and retained.")
	run := marker.Restore
	review := operations.RecoveryReview{Contract: "lantai.recovery-review/v1", RunID: run.RunID, BackupID: m.BackupID, ManifestDigest: run.ManifestDigest, RecoveryEpoch: marker.RecoveryEpoch, Administrator: string(e.admin.Context.PrincipalID), RevocationsReconciled: true, DeletionsReconciled: true, OpenOperationsReconciled: true, EvidenceDigest: digest.Of(evidence), Note: "synthetic isolated restoration rehearsal"}
	if err = r.CompleteRestore(ctx, review, evidence); err == nil {
		t.Fatal("completed without fresh administrator")
	}
	const newPassword = "restored correct horse battery staple"
	var secret []byte
	err = r.Maintenance(ctx, func(c context.Context) error {
		reset, err := r.Identity.BeginOfflineReset(c, "ada", newPassword)
		if err != nil {
			return err
		}
		secret, err = base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(reset.Enrollment.Secret)
		if err != nil {
			return err
		}
		_, err = reset.Confirm(c, totp.Code(secret, totp.Counter(e.clk.Now())), "synthetic restore administrator reset")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = r.CompleteRestore(ctx, review, []byte("different report")); err == nil {
		t.Fatal("unbound evidence accepted")
	}
	if err = r.CompleteRestore(ctx, review, evidence); err != nil {
		t.Fatal(err)
	}
	if r.Instance.Readiness().Ready {
		t.Fatal("completion unexpectedly started listeners/gate")
	}
	if err = r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	restored, err := application.Open(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { restored.Close(context.Background()) })
	if _, err = restored.Identity.VerifySession(ctx, session.Context.SessionID); err == nil {
		t.Fatal("old session survived epoch")
	}
	if _, err = restored.Identity.ExchangeToken(ctx, cred.Secret, identity.SessionRequest{Channel: identity.ChannelCLI}); err == nil {
		t.Fatal("archived machine credential resurrected")
	}
	e.clk.Advance(totp.Period * time.Second)
	if _, err = restored.Identity.Login(ctx, identity.LoginRequest{Name: "ada", Password: adminPassword, Code: totp.Code(e.secret, totp.Counter(e.clk.Now())), Channel: identity.ChannelCLI, Source: "192.0.2.10"}); err == nil {
		t.Fatal("old human credentials resurrected")
	}
	login, err := restored.Identity.Login(ctx, identity.LoginRequest{Name: "ada", Password: newPassword, Code: totp.Code(secret, totp.Counter(e.clk.Now())), Channel: identity.ChannelCLI, Source: "192.0.2.11"})
	if err != nil {
		t.Fatal(err)
	}
	page, err := restored.Query.Search(ctx, query.SearchRequest{Who: login.Context, Filter: query.Filter{ProjectID: e.project.ProjectID}})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("rebuilt index: %+v %v", page, err)
	}
	grant, err := restored.Storage.IssueReadGrant(ctx, storage.ReadRequest{Who: login.Context, AssetID: version.AssetID, VersionID: version.VersionID, Path: "content.txt", Purpose: "archive_review"})
	if err != nil {
		t.Fatal(err)
	}
	servers := remoteServers(t, restored, false)
	download := func(u string) (int, []byte) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+servers.Addresses.Transfer+u, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+login.Token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, b
	}
	status, got := download(grant.URL)
	if status != 200 || !bytes.Equal(got, body) {
		t.Fatalf("restored download: %d %s", status, got)
	}
	oldURL, err := url.Parse(oldGrant.URL)
	if err != nil {
		t.Fatal(err)
	}
	status, _ = download(oldURL.String())
	if status == 200 {
		t.Fatal("old transfer capability revived")
	}
	if _, err = operations.VerifyBackup(ctx, archive); err != nil {
		t.Fatal("restore modified its archive", err)
	}
}
