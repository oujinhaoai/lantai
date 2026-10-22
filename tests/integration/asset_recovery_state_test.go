package integration

import (
	"context"
	"encoding/base32"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/application"
	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/identity/totp"
	"github.com/oujinhaoai/lantai/internal/operations"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

// Keep the public origin stable while replacing the stopped source with its
// older recovery point. Only the proxy drops a response; all state is produced
// by real public HTTP commands against real databases and files.
func TestAssetRecoveryOriginalClientState(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, storage.Config{}, transfer.Limits{})
	principal, maker := e.agent("recovery-state@synthetic", identity.RoleContributor)
	principal, err := e.id.GetPrincipal(ctx, e.login().Context, principal.ID)
	if err != nil {
		t.Fatal(err)
	}
	issued := e.sudo(&identity.IssueCredential{PrincipalID: principal.ID, ExpectedRevision: principal.Revision, Scopes: []identity.Scope{identity.ScopeRead, identity.ScopeIngest}})
	limited, err := e.id.ExchangeToken(ctx, issued.Secret, identity.SessionRequest{Channel: identity.ChannelCLI})
	if err != nil {
		t.Fatal(err)
	}
	a := reopenApplication(t, e)
	server := assetRemoteServers(t, a)
	var mu sync.Mutex
	target, _ := url.Parse("http://" + server.Addresses.API)
	proxy := &httputil.ReverseProxy{}
	proxy.Rewrite = func(r *httputil.ProxyRequest) {
		mu.Lock()
		upstream := *target
		mu.Unlock()
		r.SetURL(&upstream)
	}
	var lose atomic.Bool
	var requests, writes atomic.Int64
	var lostReceipt []byte
	proxy.ModifyResponse = func(r *http.Response) error {
		if strings.HasSuffix(r.Request.URL.Path, "/commit") && lose.Swap(false) {
			b, readErr := io.ReadAll(r.Body)
			r.Body.Close()
			if readErr != nil {
				return readErr
			}
			mu.Lock()
			lostReceipt = b
			mu.Unlock()
			return errors.New("synthetic lost commit response")
		}
		return nil
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		conn, _, hijackErr := w.(http.Hijacker).Hijack()
		if hijackErr == nil {
			conn.Close()
		}
	}
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodGet {
			writes.Add(1)
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(gateway.Close)
	c, err := client.New(client.Config{BaseURL: gateway.URL, AllowHTTP: true, SessionToken: maker.Token})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	work := assetRealTempDir(t)
	type savedCase struct {
		name                       string
		captured                   bool
		plan                       client.ImportPlan
		source, state, destination string
		original                   client.PushState
		expected                   client.ImportResult
	}
	var cases []savedCase
	create := func(when, status string) {
		t.Helper()
		name := when + "-" + status
		root := filepath.Join(work, name, "source")
		if er := os.MkdirAll(root, 0700); er != nil {
			t.Fatal(er)
		}
		if er := os.WriteFile(filepath.Join(root, "note.txt"), []byte("synthetic "+name), 0600); er != nil {
			t.Fatal(er)
		}
		push, er := client.ApplyAssetProfile(client.PushInput{ProjectID: string(e.project.ProjectID), Slug: name, Content: client.ContentInput{AssetType: "doc", Files: []client.InputFile{{Path: "note.txt", Role: "primary"}}, Rights: mustJSON(rightsOwned())}}, client.AssetProfile{Source: "synthetic", BusinessState: "active"})
		if er != nil {
			t.Fatal(er)
		}
		plan, er := client.PrepareImport(ctx, client.ImportInput{Schema: client.ImportInputSchema, Assets: []client.ImportAsset{{Key: "document", Directory: ".", Classification: "final", Push: push}}}, root)
		if er != nil {
			t.Fatal(er)
		}
		state := filepath.Join(work, name, "state")
		token := maker.Token
		if status == "pending" {
			token = limited.Token
		}
		if er = c.SetSessionToken(token); er != nil {
			t.Fatal(er)
		}
		lose.Store(status == "unknown")
		report, applyErr := c.ApplyImport(ctx, plan, root, state)
		if status == "complete" && (applyErr != nil || !report.Complete) {
			t.Fatal(name, report, applyErr)
		}
		if status != "complete" && (applyErr == nil || report.Complete) {
			t.Fatal(name, "expected partial", report, applyErr)
		}
		expected := report.Results[0]
		if status == "unknown" {
			if expected.Status != "unknown" {
				t.Fatal(report)
			}
			mu.Lock()
			raw := append([]byte(nil), lostReceipt...)
			mu.Unlock()
			if er = json.Unmarshal(raw, &expected); er != nil || expected.AssetID == "" || expected.VersionID == "" {
				t.Fatal("lost server receipt", er)
			}
		} else if status == "pending" && expected.Status != "description_pending" {
			t.Fatal(report)
		}
		var original client.PushState
		if er = client.LoadState(filepath.Join(state, "document.push.json"), &original); er != nil {
			t.Fatal(er)
		}
		destination := filepath.Join(work, name, "verified")
		if status != "unknown" {
			if er = c.SetSessionToken(maker.Token); er != nil {
				t.Fatal(er)
			}
			verified, er := c.VerifyImport(ctx, plan, report, root, state, destination, "archive_review")
			if er != nil || !verified.Complete || !verified.CurrentRemoteVerified {
				t.Fatal(name, verified, er)
			}
		}
		cases = append(cases, savedCase{name, when == "before", plan, root, state, destination, original, expected})
	}
	for _, status := range []string{"complete", "pending", "unknown"} {
		create("before", status)
	}
	backupRoot := filepath.Join(work, "backup")
	manifest, err := a.Backup(ctx, operations.BackupOptions{Destination: backupRoot})
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"complete", "pending", "unknown"} {
		create("after", status)
	}
	if err = server.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err = a.Close(ctx); err != nil {
		t.Fatal(err)
	}

	// Restore only the captured package. Client directories remain outside it.
	home := filepath.Join(work, "restored")
	marker, err := application.RestorePackage(ctx, operations.RecoveryPackagePath(backupRoot), operations.RestoreOptions{Home: home}, identity.Config{Password: fastPassword})
	if err != nil {
		t.Fatal(err)
	}
	opts := application.Options{Instance: operations.Options{Home: home, Clock: e.clk}, Identity: identity.Config{Password: fastPassword}}
	restored, err := application.OpenOffline(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restored.Close(context.Background()) })
	var secret []byte
	err = restored.Maintenance(ctx, func(c context.Context) error {
		reset, er := restored.Identity.BeginOfflineReset(c, "ada", adminPassword)
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
		_, er = reset.Confirm(c, totp.Code(secret, totp.Counter(e.clk.Now())), "synthetic client state recovery")
		if er != nil {
			return er
		}
		report, er := restored.Recover(c)
		if er != nil {
			return er
		}
		if len(report.NeedsReconciliation) != 0 {
			return errors.New("unexpected unresolved operations")
		}
		fsck, er := restored.FSCK(c, true)
		if er != nil {
			return er
		}
		return fsck.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	proof := mustJSON(map[string]any{"core_review": map[string]any{"run_id": marker.Restore.RunID, "automatic_checks_passed": true, "unresolved_operations": 0}, "operator_accepted_recovery_point": true, "operator_confirmed_known_changes_reviewed": true, "operator_confirmed_source_offline": true, "unknown_after_capture_verified": false})
	review := operations.RecoveryReview{Contract: "lantai.recovery-review/v2", RunID: marker.Restore.RunID, BackupID: manifest.BackupID, ManifestDigest: marker.Restore.ManifestDigest, RecoveryEpoch: marker.RecoveryEpoch, Administrator: string(e.admin.Context.PrincipalID), AcceptRecoveryPoint: true, UnknownAfterCapture: true, EvidenceDigest: digest.Of(proof), Note: "Synthetic older capture point; later commands are intentionally outside the package."}
	if err = restored.CompleteRestore(ctx, review, proof); err != nil {
		t.Fatal(err)
	}
	if err = restored.Close(ctx); err != nil {
		t.Fatal(err)
	}
	restored, err = application.Open(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	e.inst, e.id, e.secret = restored.Instance, restored.Identity, secret
	e.clk.Advance(30 * time.Second)
	principal, err = e.id.GetPrincipal(ctx, e.login().Context, principal.ID)
	if err != nil {
		t.Fatal(err)
	}
	issued = e.sudo(&identity.IssueCredential{PrincipalID: principal.ID, ExpectedRevision: principal.Revision, Scopes: []identity.Scope{identity.ScopeRead, identity.ScopeIngest, identity.ScopeOrganize}})
	fresh, err := e.id.ExchangeToken(ctx, issued.Secret, identity.SessionRequest{Channel: identity.ChannelCLI})
	if err != nil {
		t.Fatal(err)
	}
	nextServer := assetRemoteServers(t, restored)
	mu.Lock()
	target, _ = url.Parse("http://" + nextServer.Addresses.API)
	mu.Unlock()
	if err = c.SetSessionToken(maker.Token); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Do(ctx, http.MethodGet, "/api/v1/projects", nil, client.Options{}); assetAPIErrorCode(err) != errcode.TokenRevoked {
		t.Fatal("old authentication was not explicitly revoked", err)
	}
	if err = c.SetSessionToken(fresh.Token); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			oldWrites := writes.Load()
			oldRequests := requests.Load()
			report, applyErr := c.ApplyImport(ctx, tc.plan, tc.source, tc.state)
			if report.Results[0].OperationID != tc.original.OperationID {
				t.Fatal("report replaced original operation", report)
			}
			if report.CompletionBasis != client.ImportHistoricalReceipts || report.CurrentRemoteVerified {
				t.Fatal("receipt claimed live verification", report)
			}
			if tc.captured {
				if applyErr != nil || !report.Complete || report.Results[0].AssetID != tc.expected.AssetID || report.Results[0].VersionID != tc.expected.VersionID {
					t.Fatal("captured command failed to reconcile", report, applyErr)
				}
			} else if strings.HasSuffix(tc.name, "complete") {
				if applyErr != nil || !report.Complete || requests.Load() != oldRequests {
					t.Fatal("historical replay changed", report, applyErr)
				}
			} else if applyErr == nil || report.Complete || writes.Load() != oldWrites {
				t.Fatal("uncaptured operation was resubmitted", report, applyErr)
			}
			if tc.captured || strings.HasSuffix(tc.name, "complete") || strings.HasSuffix(tc.name, "pending") {
				oldRequests = requests.Load()
				verified, verifyErr := c.VerifyImport(ctx, tc.plan, report, tc.source, tc.state, tc.destination, "archive_review")
				if tc.captured {
					if verifyErr != nil || !verified.Complete || !verified.CurrentRemoteVerified {
						t.Fatal("captured version not verified", verified, verifyErr)
					}
				} else if verifyErr == nil || verified.Complete || verified.CurrentRemoteVerified {
					t.Fatal("cached bytes masked absent remote version", verified, verifyErr)
				}
				if requests.Load() == oldRequests {
					t.Fatal("verification skipped current remote")
				}
			}
			var current client.PushState
			if er := client.LoadState(filepath.Join(tc.state, "document.push.json"), &current); er != nil {
				t.Fatal(er)
			}
			if current.CreateKey != tc.original.CreateKey || current.CommitKey != tc.original.CommitKey || current.OperationID != tc.original.OperationID || current.UploadID != tc.original.UploadID || current.RequestHash != tc.original.RequestHash || current.Origin != tc.original.Origin {
				t.Fatal("reconciliation replaced original state")
			}
			t.Logf("captured=%v historical_complete=%v current_remote_verified=%v; original operation and keys preserved", tc.captured, report.Complete, tc.captured)
		})
	}
	found, err := c.FindAssets(ctx, client.AssetFilter{ProjectID: string(e.project.ProjectID)}, client.AssetFindOptions{})
	if err != nil || !found.Complete || len(found.Items) != 3 {
		t.Fatal("recovery/replay added a replacement asset", found, err)
	}
	for _, tc := range cases {
		matched := false
		for _, item := range found.Items {
			if item.AssetID != tc.expected.AssetID {
				continue
			}
			matched = true
			if !tc.captured || item.VersionID != tc.expected.VersionID || item.VersionNumber != 1 {
				t.Fatal("reconciliation created or changed a version", tc.name, item)
			}
		}
		if matched != tc.captured {
			t.Fatal("captured asset set changed", tc.name)
		}
	}
}
