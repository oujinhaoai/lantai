package integration

import (
	"bytes"
	"context"
	"encoding/base32"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
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

// A 100 Continue response is sent only when the real application handler reads
// the body: authentication and business admission have already succeeded. Keep
// that body open to make draining deterministic, without replacing the copier,
// manipulating its journal or acquiring a second instance lock.
func holdBackupBusinessRequest(t *testing.T, browser backupBrowser) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	ready := make(chan struct{})
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{Got100Continue: func() { close(ready) }})
	reader, writer := io.Pipe()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, browser.base+"/api/v1/projects", reader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Expect", "100-continue")
	req.Header.Set("Origin", backupTestOrigin)
	req.Header.Set(httpauth.CSRFHeader, browser.csrf)
	req.Header.Set("Idempotency-Key", "held-business-request")
	req.AddCookie(browser.cookie)
	req.ContentLength = 2
	done := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		done <- err
	}()
	released := false
	release := func() {
		t.Helper()
		if released {
			return
		}
		released = true
		cancel()
		_ = writer.CloseWithError(context.Canceled)
		_ = reader.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("held HTTP request did not exit after cancellation")
		}
	}
	t.Cleanup(release)
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("business request exited before admitting its body: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("application did not admit held business request")
	}
	return release
}

func TestBackupManagementHTTPCancelResumeVerifiedRestore(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	e.setRole(e.admin.Context.PrincipalID, identity.RoleViewer, true)
	principal, agentSession := e.agent("resume-backup@synthetic", identity.RoleContributor)
	principal, err := e.id.GetPrincipal(ctx, e.login().Context, principal.ID)
	if err != nil {
		t.Fatal(err)
	}
	credential := e.sudo(&identity.IssueCredential{PrincipalID: principal.ID, ExpectedRevision: principal.Revision, Scopes: []identity.Scope{identity.ScopeRead}})
	content := bytes.Repeat([]byte("synthetic cancelled then resumed HTTP backup\n"), 1024)
	version := e.ingest(agentSession.Context, "backup-resume/source", content, *rightsOwned())
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
	const rootID = "synthetic-http-resume-volume"
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, application.BackupRootMarker), []byte(rootID+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	a.ConfigureBackups(ctx, root, rootID)
	servers := backupHTTPServers(t, a)
	admin := backupLogin(t, e, "http://"+servers.Addresses.API, []identity.Scope{identity.ScopeAdmin, identity.ScopeRead})
	start := identity.ApproveBackup{Kind: "start", DrainSeconds: 15, TimeoutSeconds: 60}
	grant := backupApproval(t, e, admin, string(identity.ActBackupStart), start)
	release := holdBackupBusinessRequest(t, admin)
	var initial backupmgmt.Job
	admin.must(http.MethodPost, "admin/backups", "cancel-before-capture", map[string]any{"grant_id": grant, "drain_seconds": 15, "timeout_seconds": 60}, &initial, http.StatusAccepted)
	var draining backupmgmt.Job
	admin.must(http.MethodGet, "admin/backups/"+string(initial.ID), "", nil, &draining, http.StatusOK)
	if draining.State != "draining" || draining.CaptureStarted || a.Readiness().Ready {
		t.Fatalf("in-flight request was not held by real HTTP admission: %+v", draining)
	}
	admin.must(http.MethodGet, "projects", "", nil, nil, http.StatusServiceUnavailable)
	var cancelling backupmgmt.Job
	admin.must(http.MethodPost, "admin/backups/"+string(initial.ID)+"/cancel", "", map[string]any{}, &cancelling, http.StatusAccepted)
	if cancelling.ID != initial.ID || (cancelling.State != "cancelling" && cancelling.State != "cancelled") {
		t.Fatalf("cancel acknowledged another task or invalid state: %+v", cancelling)
	}
	deadline := time.Now().Add(5 * time.Second)
	var cancelled backupmgmt.Job
	for {
		admin.must(http.MethodGet, "admin/backups/"+string(initial.ID), "", nil, &cancelled, http.StatusOK)
		if cancelled.State == "cancelled" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cancel did not reach terminal state: %+v", cancelled)
		}
		time.Sleep(25 * time.Millisecond)
	}
	if cancelled.CaptureStarted || cancelled.Attempt != 1 || !a.Readiness().Ready {
		t.Fatalf("cancelled drain changed capture identity or failed to restore admission: %+v", cancelled)
	}
	release()

	resumePath := "admin/backups/" + string(initial.ID) + "/resume"
	admin.must(http.MethodPost, resumePath, "resume-without-proof", map[string]any{}, nil, http.StatusForbidden)
	resume := identity.ApproveBackup{Kind: "resume", JobID: initial.ID, DrainSeconds: 15, TimeoutSeconds: 60}
	resumeGrant := backupApproval(t, e, admin, string(identity.ActBackupResume), resume)
	if resumeGrant == grant {
		t.Fatal("resume did not obtain a new bound HumanGrant")
	}
	body := map[string]any{"grant_id": resumeGrant, "drain_seconds": 15, "timeout_seconds": 60}
	var resumed backupmgmt.Job
	admin.must(http.MethodPost, resumePath, "resume-cancelled-backup", body, &resumed, http.StatusAccepted)
	if resumed.ID != initial.ID || resumed.Attempt != 2 || !resumed.CreatedAt.Equal(initial.CreatedAt) {
		t.Fatalf("resume replaced the persisted job identity: before=%+v after=%+v", initial, resumed)
	}
	complete := awaitBackup(t, admin, resumed.ID)
	var replay backupmgmt.Job
	admin.must(http.MethodPost, resumePath, "resume-cancelled-backup", body, &replay, http.StatusAccepted)
	if replay.ID != complete.ID || replay.Attempt != 2 || replay.State != "complete" {
		t.Fatalf("resume retry repeated work: %+v", replay)
	}
	var history struct {
		Items []backupmgmt.Job `json:"items"`
	}
	admin.must(http.MethodGet, "admin/backups/history", "", nil, &history, http.StatusOK)
	if len(history.Items) != 1 || history.Items[0].ID != complete.ID || !a.Readiness().Ready {
		t.Fatalf("resumed history or readiness: %+v", history)
	}
	archive := filepath.Join(root, string(complete.ID))
	manifest, err := operations.VerifyBackup(ctx, archive)
	if err != nil || manifest.BackupID != complete.BackupID || manifest.State != "complete" {
		t.Fatalf("resumed backup independent verification: %+v %v", manifest, err)
	}
	t.Log("real HTTP draining cancellation -> terminal cancelled -> new bound HumanGrant -> same job attempt 2 -> verified complete; same-key resume replay did not repeat work")
	if err := servers.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(ctx); err != nil {
		t.Fatal(err)
	}
	assertManagedBackupRestores(t, e, archive, manifest, oldKey, agentSession, credential.Secret, oldGrant.URL, version.AssetID, version.VersionID, content)
}

func TestBackupManagementNotProjectedByRuntimeMCP(t *testing.T) {
	bin := buildConsistencyCLI(t)
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	_, machine := e.agent("backup-mcp@synthetic", "")
	admin := e.login()
	a := reopenApplication(t, e)
	// Even when the real core advertises backup_management, it must not become
	// an MCP tool, prompt or resource for either an Agent or an admin session.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	const rootID = "synthetic-mcp-unprojected-volume"
	if err := os.WriteFile(filepath.Join(root, application.BackupRootMarker), []byte(rootID+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	backups := a.ConfigureBackups(t.Context(), root, rootID)
	overview, err := backups.Overview()
	if err != nil || !overview.Availability.Available {
		t.Fatalf("MCP test requires usable backup storage: %+v %v", overview, err)
	}
	servers := remoteServers(t, a, true)
	origin := "http://" + servers.Addresses.API
	meta := backupBrowser{t: t, base: origin}
	status, raw, _ := meta.request(http.MethodGet, "meta", "", nil)
	if status != http.StatusOK || !bytes.Contains(raw, []byte(`"backup_management"`)) {
		t.Fatalf("MCP test core did not advertise backup capability: HTTP %d", status)
	}
	for _, session := range []struct {
		name      string
		token     string
		principal ids.ID
		session   ids.ID
	}{{"agent", machine.Token, machine.Context.PrincipalID, machine.Context.SessionID}, {"administrator", admin.Token, admin.Context.PrincipalID, admin.Context.SessionID}} {
		t.Run(session.name, func(t *testing.T) {
			h := newConsistencyClient(t, bin, origin, session.token)
			check := func(label string, value any) {
				t.Helper()
				raw, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				lower := strings.ToLower(string(raw))
				for _, forbidden := range []string{"backup", "admin/backups", "identity/challenges", "grant_id"} {
					if strings.Contains(lower, forbidden) {
						t.Fatalf("runtime MCP %s projected a privileged backup surface: %s", label, forbidden)
					}
				}
			}
			var names []string
			for tool, err := range h.mcp.Tools(t.Context(), nil) {
				if err != nil {
					t.Fatal("runtime tools/list", err)
				}
				check("tools/list", tool)
				names = append(names, tool.Name)
			}
			if len(names) == 0 {
				t.Fatal("runtime MCP did not expose its ordinary tools")
			}
			for resource, err := range h.mcp.Resources(t.Context(), nil) {
				if err != nil {
					t.Fatal("runtime resources/list", err)
				}
				check("resources/list", resource)
			}
			for prompt, err := range h.mcp.Prompts(t.Context(), nil) {
				if err != nil {
					t.Fatal("runtime prompts/list", err)
				}
				check("prompts/list", prompt)
			}
			who, err := h.mcp.CallTool(t.Context(), &mcp.CallToolParams{Name: "whoami"})
			if err != nil || who.IsError || who.StructuredContent == nil {
				t.Fatalf("actual MCP authentication failed: %+v %v", who, err)
			}
			var identity struct {
				Principal struct {
					ID ids.ID `json:"principal_id"`
				} `json:"principal"`
				Session struct {
					ID ids.ID `json:"session_id"`
				} `json:"session"`
			}
			identityRaw, err := json.Marshal(who.StructuredContent)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(identityRaw, &identity); err != nil || identity.Principal.ID != session.principal || identity.Session.ID != session.session {
				t.Fatalf("MCP lost the actual principal/session binding: %+v %v", identity, err)
			}
			for _, name := range []string{"backup_start", "backup_resume", "backup_cancel", "backup_schedule"} {
				out, err := h.mcp.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: map[string]any{}})
				if err == nil && out != nil && !out.IsError {
					t.Fatalf("unlisted privileged MCP tool executed: %s", name)
				}
			}
			history, err := backups.History()
			if err != nil || len(history) != 0 {
				t.Fatalf("rejected MCP calls created backup work: %+v %v", history, err)
			}
			t.Logf("actual stdio MCP negotiated %s; enumerated %d ordinary tools and no backup/approval tools, resources or prompts; privileged tool probes refused; tools=%v", h.mcp.InitializeResult().ProtocolVersion, len(names), names)
		})
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
	info, err := restored.ManagementInfo(ctx)
	if err != nil || info.InstanceID != marker.InstanceID || info.RecoveryEpoch != marker.RecoveryEpoch || info.State != "ready" || info.RecoveryError != "" {
		t.Fatalf("restored management projection: %+v %v", info, err)
	}
	if len(info.RecoveryReceipts) != 1 {
		t.Fatalf("completed real restore missing from management projection: %+v", info.RecoveryReceipts)
	}
	completed := info.RecoveryReceipts[0]
	if completed.RunID != review.RunID || completed.BackupID != review.BackupID || completed.ManifestDigest != review.ManifestDigest || completed.RecoveryEpoch != review.RecoveryEpoch || completed.EvidenceDigest != review.EvidenceDigest || completed.UnknownAfterCapture {
		t.Fatalf("completed management receipt lost its fixed restore basis: %+v", completed)
	}
	t.Logf("empty-directory restore reconciled with a new administrator; old credentials/capability refused; rebuilt index and %d source bytes match SHA256", len(content))
}
