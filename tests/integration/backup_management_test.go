package integration

import (
	"bytes"
	"context"
	"encoding/base32"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/application"
	"github.com/oujinhaoai/lantai/internal/backupmgmt"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/identity/httpauth"
	"github.com/oujinhaoai/lantai/internal/identity/masterkey"
	"github.com/oujinhaoai/lantai/internal/identity/totp"
	"github.com/oujinhaoai/lantai/internal/operations"
	"github.com/oujinhaoai/lantai/internal/query"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

const backupTestOrigin = "https://backup.example.test"

// backupBrowser exercises the public HTTP authentication/CSRF boundary, rather
// than manufacturing an authz.Context or calling the coordinator to start work.
type backupBrowser struct {
	t      *testing.T
	base   string
	cookie *http.Cookie
	csrf   string
	token  string
}

func (b backupBrowser) request(method, path, key string, body any) (int, []byte, []*http.Cookie) {
	b.t.Helper()
	var input io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			b.t.Fatal(err)
		}
		input = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(b.t.Context(), method, b.base+"/api/v1/"+path, input)
	if err != nil {
		b.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Origin", backupTestOrigin)
	if b.cookie != nil {
		req.AddCookie(b.cookie)
	}
	if b.csrf != "" {
		req.Header.Set(httpauth.CSRFHeader, b.csrf)
	}
	if b.token != "" {
		req.Header.Set("Authorization", "Bearer "+b.token)
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		b.t.Fatal(err)
	}
	return resp.StatusCode, raw, resp.Cookies()
}

func (b backupBrowser) must(method, path, key string, body, out any, want int) {
	b.t.Helper()
	status, raw, _ := b.request(method, path, key, body)
	if status != want {
		b.t.Fatalf("%s %s: status %d, want %d: %s", method, path, status, want, raw)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			b.t.Fatalf("decode %s: %v", path, err)
		}
	}
}

func backupHTTPServers(t *testing.T, a *application.App) *application.HTTPServers {
	t.Helper()
	cfg := a.Instance.Config()
	cfg.Listen = operations.ListenConfig{API: "127.0.0.1:0", Transfer: "127.0.0.1:0", Operations: "127.0.0.1:0"}
	cfg.HTTP.AllowedOrigins = []string{backupTestOrigin}
	s, err := a.StartHTTP(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	return s
}

func backupLogin(t *testing.T, e *env, base string, scopes []identity.Scope) backupBrowser {
	t.Helper()
	b := backupBrowser{t: t, base: base}
	status, raw, cookies := b.request(http.MethodPost, "sessions/login", "", map[string]any{
		"name": "ada", "password": adminPassword, "code": e.fresh(),
		"channel": identity.ChannelBrowser, "scopes": scopes, "ttl_seconds": 86400,
	})
	if status != http.StatusCreated {
		t.Fatalf("browser login: status %d: %s", status, raw)
	}
	var session struct {
		Token     string `json:"token"`
		CSRFToken string `json:"csrf_token"`
	}
	if err := json.Unmarshal(raw, &session); err != nil {
		t.Fatal(err)
	}
	for _, cookie := range cookies {
		if cookie.Name == httpauth.CookieName {
			b.cookie = cookie
		}
	}
	b.csrf = session.CSRFToken
	if b.cookie == nil || b.csrf == "" || session.Token != "" {
		t.Fatal("browser login did not return a cookie-only session with CSRF")
	}
	return b
}

func backupApproval(t *testing.T, e *env, browser backupBrowser, action string, command any) ids.ID {
	t.Helper()
	var ch identity.Challenge
	browser.must(http.MethodPost, "identity/challenges", "", map[string]any{"action": action, "command": command}, &ch, http.StatusCreated)
	var grant struct {
		ID ids.ID `json:"grant_id"`
	}
	browser.must(http.MethodPost, "identity/challenges/"+string(ch.ChallengeID)+"/verify", "", map[string]string{"code": e.fresh()}, &grant, http.StatusCreated)
	if !grant.ID.Valid() {
		t.Fatal("missing bound HumanGrant")
	}
	return grant.ID
}

func awaitBackup(t *testing.T, browser backupBrowser, id ids.ID) backupmgmt.Job {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		var job backupmgmt.Job
		browser.must(http.MethodGet, "admin/backups/"+string(id), "", nil, &job, http.StatusOK)
		switch job.State {
		case "complete":
			return job
		case "failed", "cancelled", "interrupted":
			t.Fatalf("backup did not complete: %+v", job)
		}
		if time.Now().After(deadline) {
			t.Fatalf("backup completion deadline: %+v", job)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// This test takes a real online snapshot through the public administrator API,
// persists/replays its receipt across restart, lets the persisted daily schedule
// take another real snapshot, then restores and reconciles an empty instance.
func TestBackupManagementHTTPToVerifiedIsolatedRestore(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	e.setRole(e.admin.Context.PrincipalID, identity.RoleViewer, true)
	principal, agentSession := e.agent("backup-management@synthetic", identity.RoleContributor)
	principal, err := e.id.GetPrincipal(ctx, e.login().Context, principal.ID)
	if err != nil {
		t.Fatal(err)
	}
	credential := e.sudo(&identity.IssueCredential{PrincipalID: principal.ID, ExpectedRevision: principal.Revision, Scopes: []identity.Scope{identity.ScopeRead}})
	body := bytes.Repeat([]byte("synthetic online management restoration source\n"), 4096)
	version := e.ingest(agentSession.Context, "backup-management/source", body, *rightsOwned())
	oldGrant, err := e.storage.IssueReadGrant(ctx, storage.ReadRequest{Who: agentSession.Context, AssetID: version.AssetID, VersionID: version.VersionID, Path: "content.txt", Purpose: "archive_review"})
	if err != nil {
		t.Fatal(err)
	}
	a := reopenApplication(t, e, storage.Config{MinFreeBytes: 1 << 20})
	oldKey, err := masterkey.Load(a.Instance.Config().SecretsDir)
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const rootID = "synthetic-backup-management-volume"
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, application.BackupRootMarker), []byte(rootID+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	a.ConfigureBackups(ctx, root, rootID)
	servers := backupHTTPServers(t, a)
	base := "http://" + servers.Addresses.API
	anonymous := backupBrowser{t: t, base: base}
	anonymous.must(http.MethodGet, "admin/backups", "", nil, nil, http.StatusUnauthorized)
	readOnly := backupLogin(t, e, base, []identity.Scope{identity.ScopeRead})
	readOnly.must(http.MethodGet, "admin/backups", "", nil, nil, http.StatusForbidden)
	readOnly.must(http.MethodPost, "identity/challenges", "", map[string]any{"action": identity.ActBackupStart, "command": identity.ApproveBackup{Kind: "start"}}, nil, http.StatusForbidden)
	agent := backupBrowser{t: t, base: base, token: agentSession.Token}
	agent.must(http.MethodGet, "admin/backups", "", nil, nil, http.StatusForbidden)
	agent.must(http.MethodPost, "identity/challenges", "", map[string]any{"action": identity.ActBackupStart, "command": identity.ApproveBackup{Kind: "start"}}, nil, http.StatusForbidden)
	admin := backupLogin(t, e, base, []identity.Scope{identity.ScopeRead, identity.ScopeAdmin})
	admin.must(http.MethodPost, "admin/backups", "no-approval", map[string]any{}, nil, http.StatusForbidden)
	noCSRF := admin
	noCSRF.csrf = ""
	noCSRF.must(http.MethodPost, "identity/challenges", "", map[string]any{"action": identity.ActBackupStart, "command": identity.ApproveBackup{Kind: "start"}}, nil, http.StatusForbidden)
	var overview backupmgmt.Overview
	admin.must(http.MethodGet, "admin/backups", "", nil, &overview, http.StatusOK)
	if !overview.Availability.Available || overview.Current != nil {
		t.Fatalf("initial backup availability: %+v", overview)
	}
	t.Log("public HTTP: administrator authenticated; anonymous, read-only, Agent, missing approval and missing CSRF refused")

	// The setting and consent bind the exact wall time, zone and revision.
	scheduleInput := identity.BackupScheduleInput{Enabled: true, Timezone: "Etc/UTC", Time: e.clk.Now().Add(30 * time.Minute).Format("15:04"), Revision: overview.Schedule.Revision, DrainSeconds: 10, TimeoutSeconds: 60}
	scheduleGrant := backupApproval(t, e, admin, string(identity.ActBackupSchedule), identity.ApproveBackup{Kind: "schedule", Schedule: &scheduleInput})
	var schedule backupmgmt.Schedule
	admin.must(http.MethodPut, "admin/backups/schedule", "daily-plan", struct {
		GrantID ids.ID `json:"grant_id"`
		identity.BackupScheduleInput
	}{scheduleGrant, scheduleInput}, &schedule, http.StatusOK)
	if !schedule.Enabled || schedule.NextRunAt == nil || schedule.Timezone != "Etc/UTC" {
		t.Fatalf("saved schedule: %+v", schedule)
	}

	startCommand := identity.ApproveBackup{Kind: "start", DrainSeconds: 10, TimeoutSeconds: 60}
	startGrant := backupApproval(t, e, admin, string(identity.ActBackupStart), startCommand)
	startBody := map[string]any{"grant_id": startGrant, "drain_seconds": 10, "timeout_seconds": 60}
	var accepted backupmgmt.Job
	admin.must(http.MethodPost, "admin/backups", "online-complete", startBody, &accepted, http.StatusAccepted)
	complete := awaitBackup(t, admin, accepted.ID)
	if complete.Source != "manual" || !complete.BackupID.Valid() || !complete.CaptureStarted {
		t.Fatalf("missing online snapshot identity: %+v", complete)
	}
	archive := filepath.Join(root, string(complete.ID))
	manifest, err := operations.VerifyBackup(ctx, archive)
	if err != nil || manifest.State != "complete" || manifest.Watermarks["events"] == 0 {
		t.Fatalf("verified online backup: %+v %v", manifest, err)
	}
	for _, forbidden := range []string{"db/index.db", "secrets/master.key"} {
		if _, err := os.Stat(filepath.Join(archive, forbidden)); !os.IsNotExist(err) {
			t.Fatalf("forbidden backup member %s: %v", forbidden, err)
		}
	}
	admin.must(http.MethodGet, "admin/backups", "", nil, &overview, http.StatusOK)
	if overview.Current != nil || overview.LastSuccess == nil || overview.LastSuccess.ID != complete.ID || !a.Readiness().Ready {
		t.Fatalf("backup failed to restore normal readiness: %+v", overview)
	}

	// Existing cookie/CSRF sessions and one-time approval receipts survive this
	// ordinary restart. Replaying the exact HTTP request cannot create a job.
	if err := servers.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(ctx); err != nil {
		t.Fatal(err)
	}
	a = reopenApplication(t, e, storage.Config{MinFreeBytes: 1 << 20})
	a.ConfigureBackups(ctx, root, rootID)
	servers = backupHTTPServers(t, a)
	admin.base = "http://" + servers.Addresses.API
	var restartedSchedule backupmgmt.Schedule
	admin.must(http.MethodGet, "admin/backups/schedule", "", nil, &restartedSchedule, http.StatusOK)
	if restartedSchedule.Revision != schedule.Revision || restartedSchedule.NextRunAt == nil || !restartedSchedule.NextRunAt.Equal(*schedule.NextRunAt) || restartedSchedule.Time != schedule.Time {
		t.Fatalf("schedule changed across restart: before=%+v after=%+v", schedule, restartedSchedule)
	}
	var replay backupmgmt.Job
	admin.must(http.MethodPost, "admin/backups", "online-complete", startBody, &replay, http.StatusAccepted)
	if replay.ID != complete.ID || replay.State != "complete" {
		t.Fatalf("restart replay did not retain job: %+v", replay)
	}
	var history struct {
		Items []backupmgmt.Job `json:"items"`
	}
	admin.must(http.MethodGet, "admin/backups/history", "", nil, &history, http.StatusOK)
	if len(history.Items) != 1 || history.Items[0].ID != complete.ID {
		t.Fatalf("restart history: %+v", history)
	}
	t.Log("manual backup independently verified; restart retained schedule/history and replay returned the same completed job")

	// Advance only the injected instance clock. The actual scheduler goroutine
	// (its normal ten-second tick) owns admission and creates the second archive.
	e.clk.Advance(restartedSchedule.NextRunAt.Sub(e.clk.Now()))
	deadline := time.Now().Add(25 * time.Second)
	var scheduled backupmgmt.Job
	for !scheduled.ID.Valid() {
		admin.must(http.MethodGet, "admin/backups/history", "", nil, &history, http.StatusOK)
		for _, job := range history.Items {
			if job.Source == "schedule" {
				scheduled = job
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("persisted scheduler did not run: %+v", history)
		}
		if !scheduled.ID.Valid() {
			time.Sleep(250 * time.Millisecond)
		}
	}
	scheduled = awaitBackup(t, admin, scheduled.ID)
	if scheduled.ScheduledFor != e.clk.Now().Format("2006-01-02") || scheduled.ScheduleTimezone != "Etc/UTC" || scheduled.ID == complete.ID {
		t.Fatalf("scheduled backup identity: %+v", scheduled)
	}
	if _, err := operations.VerifyBackup(ctx, filepath.Join(root, string(scheduled.ID))); err != nil {
		t.Fatal("scheduled backup failed independent verification", err)
	}
	admin.must(http.MethodGet, "admin/backups/history", "", nil, &history, http.StatusOK)
	if len(history.Items) != 2 || !a.Readiness().Ready {
		t.Fatalf("unexpected jobs or readiness after scheduler: %+v", history)
	}
	t.Log("persisted daily schedule produced a second independently verified real backup and normal readiness returned")
	if err := servers.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(ctx); err != nil {
		t.Fatal(err)
	}

	assertManagedBackupRestores(t, e, archive, manifest, oldKey, agentSession, credential.Secret, oldGrant.URL, version.AssetID, version.VersionID, body)
}

func assertManagedBackupRestores(t *testing.T, e *env, archive string, manifest operations.BackupManifest, oldKey *masterkey.Key, oldSession identity.IssuedSession, oldCredential, oldReadURL string, asset, version ids.ID, content []byte) {
	t.Helper()
	ctx := t.Context()
	target := filepath.Join(t.TempDir(), "restored")
	marker, err := application.Restore(ctx, operations.RestoreOptions{Home: target, BackupDir: archive, MinimumEpoch: manifest.RecoveryEpoch + 2}, oldKey, identity.Config{Password: fastPassword})
	if err != nil {
		t.Fatal(err)
	}
	if marker.Restore == nil || marker.Restore.Stage != "reconciliation_required" || marker.RecoveryEpoch != manifest.RecoveryEpoch+3 {
		t.Fatalf("restore did not latch reconciliation: %+v", marker)
	}
	opts := application.Options{Instance: operations.Options{Home: target, Clock: e.clk}, Identity: identity.Config{Password: fastPassword}, Storage: storage.Config{MinFreeBytes: 1 << 20}}
	if unsafe, err := application.Open(ctx, opts); err == nil {
		unsafe.Close(context.Background())
		t.Fatal("restored instance opened before reconciliation")
	}
	offline, err := application.OpenOffline(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { offline.Close(context.Background()) })
	newKey, err := masterkey.Load(offline.Instance.Config().SecretsDir)
	if err != nil || newKey.ID() == oldKey.ID() {
		t.Fatalf("restore key not rotated: %v", err)
	}
	evidence := []byte("Synthetic online-backup rehearsal: all archived credentials revoked; no later deletion; open operations inspected and retained; independent manifest verification passed.")
	review := operations.RecoveryReview{Contract: "lantai.recovery-review/v1", RunID: marker.Restore.RunID, BackupID: manifest.BackupID, ManifestDigest: marker.Restore.ManifestDigest, RecoveryEpoch: marker.RecoveryEpoch, Administrator: string(e.admin.Context.PrincipalID), RevocationsReconciled: true, DeletionsReconciled: true, OpenOperationsReconciled: true, EvidenceDigest: digest.Of(evidence), Note: "synthetic managed backup isolated restore"}
	if err := offline.CompleteRestore(ctx, review, evidence); err == nil {
		t.Fatal("restoration completed without a fresh administrator")
	}
	const newPassword = "managed backup fresh administrator password"
	var newSecret []byte
	err = offline.Maintenance(ctx, func(c context.Context) error {
		reset, err := offline.Identity.BeginOfflineReset(c, "ada", newPassword)
		if err != nil {
			return err
		}
		newSecret, err = base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(reset.Enrollment.Secret)
		if err != nil {
			return err
		}
		_, err = reset.Confirm(c, totp.Code(newSecret, totp.Counter(e.clk.Now())), "synthetic backup-management restoration")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := offline.CompleteRestore(ctx, review, []byte("unbound report")); err == nil {
		t.Fatal("restoration accepted unbound reconciliation evidence")
	}
	if err := offline.CompleteRestore(ctx, review, evidence); err != nil {
		t.Fatal(err)
	}
	if err := offline.Close(ctx); err != nil {
		t.Fatal(err)
	}
	restored, err := application.Open(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { restored.Close(context.Background()) })
	if _, err := restored.Identity.VerifySession(ctx, oldSession.Context.SessionID); err == nil {
		t.Fatal("archived session survived restoration")
	}
	if _, err := restored.Identity.ExchangeToken(ctx, oldCredential, identity.SessionRequest{Channel: identity.ChannelCLI}); err == nil {
		t.Fatal("archived machine credential survived restoration")
	}
	e.clk.Advance(totp.Period * time.Second)
	if _, err := restored.Identity.Login(ctx, identity.LoginRequest{Name: "ada", Password: adminPassword, Code: totp.Code(e.secret, totp.Counter(e.clk.Now())), Channel: identity.ChannelCLI, Source: "192.0.2.20"}); err == nil {
		t.Fatal("archived administrator factor survived restoration")
	}
	login, err := restored.Identity.Login(ctx, identity.LoginRequest{Name: "ada", Password: newPassword, Code: totp.Code(newSecret, totp.Counter(e.clk.Now())), Channel: identity.ChannelCLI, Source: "192.0.2.21"})
	if err != nil {
		t.Fatal(err)
	}
	page, err := restored.Query.Search(ctx, query.SearchRequest{Who: login.Context, Filter: query.Filter{ProjectID: e.project.ProjectID}})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("restored derived index did not rebuild: %+v %v", page, err)
	}
	grant, err := restored.Storage.IssueReadGrant(ctx, storage.ReadRequest{Who: login.Context, AssetID: asset, VersionID: version, Path: "content.txt", Purpose: "archive_review"})
	if err != nil {
		t.Fatal(err)
	}
	servers := remoteServers(t, restored, false)
	download := func(path string) (int, []byte) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+servers.Addresses.Transfer+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+login.Token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		got, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, got
	}
	status, got := download(grant.URL)
	if status != http.StatusOK || shaOf(got) != shaOf(content) {
		t.Fatalf("restored source bytes differ: HTTP %d, digest %s", status, shaOf(got))
	}
	if status, _ := download(oldReadURL); status == http.StatusOK {
		t.Fatal("archived read capability survived restoration")
	}
	if _, err := operations.VerifyBackup(ctx, archive); err != nil {
		t.Fatal("restoration altered its source archive", err)
	}
	t.Logf("empty-directory restore reconciled with a new administrator; old credentials/capability refused; rebuilt index and %d source bytes match SHA256", len(content))
}
