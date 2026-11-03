package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/oujinhaoai/lantai/internal/application"
	"github.com/oujinhaoai/lantai/internal/backupmgmt"
	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/identity/masterkey"
	"github.com/oujinhaoai/lantai/internal/operations"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

func TestDownloadLinksCLIAndMCPToIsolatedRestore(t *testing.T) {
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	e.setRole(e.admin.Context.PrincipalID, identity.RoleViewer, true)
	p, machine := e.agent("download-link@synthetic", identity.RoleContributor)
	p, err := e.id.GetPrincipal(t.Context(), e.login().Context, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	credential := e.sudo(&identity.IssueCredential{PrincipalID: p.ID, ExpectedRevision: p.Revision, Scopes: []identity.Scope{identity.ScopeRead}})
	content := bytes.Repeat([]byte("synthetic download and recovery verification\n"), 8192)
	v := e.ingest(machine.Context, "download-links/source", content, *rightsOwned())
	oldRead, err := e.storage.IssueReadGrant(t.Context(), storage.ReadRequest{Who: machine.Context, AssetID: v.AssetID, VersionID: v.VersionID, Path: "content.txt", Purpose: "archive_review"})
	if err != nil {
		t.Fatal(err)
	}
	a := reopenApplication(t, e)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	const rootID = "synthetic-download-links"
	if err = os.WriteFile(filepath.Join(root, application.BackupRootMarker), []byte(rootID+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	a.ConfigureBackups(t.Context(), root, rootID)
	srv := remoteServers(t, a, true)
	origin := "http://" + srv.Addresses.API
	admin, err := a.Identity.Login(t.Context(), identity.LoginRequest{Name: "ada", Password: adminPassword, Code: e.fresh(), Channel: identity.ChannelCLI, Scopes: []identity.Scope{identity.ScopeSelf, identity.ScopeRead, identity.ScopeAdmin}})
	if err != nil {
		t.Fatal(err)
	}
	b := backupBrowser{t: t, base: origin, token: admin.Token}
	start := backupApproval(t, e, b, string(identity.ActBackupStart), identity.ApproveBackup{Kind: "start"})
	var accepted backupmgmt.Job
	b.must("POST", "admin/backups", "download-links-backup", map[string]any{"grant_id": start}, &accepted, 202)
	j := awaitBackup(t, b, accepted.ID)
	if j.Package == nil {
		t.Fatal("missing complete package")
	}
	h := newConsistencyClient(t, buildConsistencyCLI(t), origin, admin.Token)
	download := func(t *testing.T, link client.DownloadLink, token string, byteRange string, want int) []byte {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), link.Method, link.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range link.Headers {
			req.Header.Set(k, v)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if byteRange != "" {
			req.Header.Set("Range", byteRange)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		raw, err := io.ReadAll(res.Body)
		if err != nil || res.StatusCode != want {
			t.Fatalf("download status=%d want=%d err=%v", res.StatusCode, want, err)
		}
		if want == 200 && (int64(len(raw)) != link.Size || digest.Of(raw).Hex() != link.SHA256) {
			t.Fatal("download integrity mismatch")
		}
		return raw
	}
	decode := func(t *testing.T, value any) client.DownloadLink {
		t.Helper()
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(raw, []byte(admin.Token)) {
			t.Fatal("session leaked into descriptor")
		}
		var link client.DownloadLink
		if err = json.Unmarshal(raw, &link); err != nil || !link.SessionRequired || link.ExpiresAt.IsZero() {
			t.Fatal("invalid descriptor", err)
		}
		return link
	}
	var saved string
	downloadedDirectory := t.TempDir()
	for _, mode := range []string{"cli", "mcp"} {
		t.Run(mode, func(t *testing.T) {
			var fileLink client.DownloadLink
			if mode == "cli" {
				out, code := h.cli(t, "download-link", "--asset", string(v.AssetID), "--version", string(v.VersionID), "--path", "content.txt", "--purpose", "archive_review")
				if code != 0 {
					t.Fatalf("file link CLI: %d %+v", code, out)
				}
				fileLink = decode(t, out)
			} else {
				out, err := h.mcp.CallTool(t.Context(), &mcp.CallToolParams{Name: "resource_download_link", Arguments: map[string]any{"asset_id": v.AssetID, "version_id": v.VersionID, "path": "content.txt", "purpose": "archive_review"}})
				if err != nil || out.IsError {
					t.Fatalf("file link MCP: %+v %v", out, err)
				}
				fileLink = decode(t, out.StructuredContent)
			}
			if !bytes.Equal(download(t, fileLink, admin.Token, "", 200), content) {
				t.Fatal("ordinary bytes differ")
			}
			download(t, fileLink, "", "", 401)
			approval := backupApproval(t, e, b, string(identity.ActBackupExport), identity.ApproveBackup{Kind: "download", JobID: j.ID, PackageSHA256: j.Package.SHA256})
			in := client.BackupDownloadInput{GrantID: approval, SHA256: j.Package.SHA256}
			var link client.DownloadLink
			if mode == "cli" {
				input := filepath.Join(t.TempDir(), "approval.json")
				raw, _ := json.Marshal(in)
				if err := os.WriteFile(input, raw, 0600); err != nil {
					t.Fatal(err)
				}
				out, code := h.cli(t, "backups", "download-link", "--id", string(j.ID), "--input", input, "--idempotency-key", "link-"+mode)
				if code != 0 {
					t.Fatalf("backup CLI: %d %+v", code, out)
				}
				link = decode(t, out)
			} else {
				out, err := h.mcp.CallTool(t.Context(), &mcp.CallToolParams{Name: "backup_download_link", Arguments: map[string]any{"job_id": j.ID, "approval": in, "idempotency_key": "link-" + mode}})
				if err != nil || out.IsError {
					t.Fatalf("backup MCP: %+v %v", out, err)
				}
				link = decode(t, out.StructuredContent)
			}
			if !link.Sensitive || len(link.Headers) != 1 {
				t.Fatal("missing sensitive headers metadata")
			}
			raw := download(t, link, admin.Token, "", 200)
			if mode == "mcp" {
				saved = filepath.Join(downloadedDirectory, "downloaded.lantai")
				if err := os.WriteFile(saved, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			download(t, link, admin.Token, "", 200) // same approval permits retry
			if len(download(t, link, admin.Token, "bytes=0-63", 206)) != 64 {
				t.Fatal("range download")
			}
			download(t, link, machine.Token, "", 403)
			download(t, link, "", "", 401)
			bad := link
			bad.Headers = map[string]string{"X-Lantai-Backup-Download": "expired-or-unknown"}
			download(t, bad, admin.Token, "", 403)
		})
	}
	if saved == "" {
		t.Fatal("MCP download did not finish")
	}
	// Restore only bytes actually fetched through the public download route.
	snapshot := filepath.Join(t.TempDir(), "downloaded-snapshot")
	var key *masterkey.Key
	_, err = operations.ExtractRecoveryPackage(t.Context(), saved, snapshot, func(r io.Reader) error { var err error; key, err = masterkey.ReadRecoveryMaterial(r); return err })
	if err != nil || key == nil {
		t.Fatal("downloaded package verification", err)
	}
	manifest, err := operations.VerifyBackup(t.Context(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertManagedBackupRestores(t, e, snapshot, manifest, key, machine, credential.Secret, oldRead.URL, v.AssetID, v.VersionID, content, false)
	t.Logf("CLI and actual stdio MCP: file and complete package downloads verified; repeated GET and Range accepted, unauthenticated/wrong-session/invalid-permit refused; downloaded backup restored, credentials rotated, admin rebuilt, index and %d source bytes verified; job=%s sha256=%s", len(content), j.ID, j.Package.SHA256)
}
