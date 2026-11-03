package integration

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/application"
	"github.com/oujinhaoai/lantai/internal/backupmgmt"
	"github.com/oujinhaoai/lantai/internal/cli"
	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

func TestRemoteBackupCLIOriginalApprovalCancelResumeAndSchedule(t *testing.T) {
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	e.setRole(e.admin.Context.PrincipalID, identity.RoleViewer, true)
	_, agent := e.agent("remote-backup-cli@synthetic", identity.RoleOwner)
	e.ingest(agent.Context, "remote-backup-cli/source", []byte("synthetic backup CLI bytes"), *rightsOwned())
	a := reopenApplication(t, e)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	const rootID = "synthetic-cli-backup-volume"
	if err := os.WriteFile(filepath.Join(root, application.BackupRootMarker), []byte(rootID+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	a.ConfigureBackups(t.Context(), root, rootID)
	servers := backupHTTPServers(t, a)
	origin := "http://" + servers.Addresses.API
	human, err := a.Identity.Login(t.Context(), identity.LoginRequest{Name: "ada", Password: adminPassword, Code: e.fresh(), Channel: identity.ChannelCLI, Scopes: []identity.Scope{identity.ScopeSelf, identity.ScopeRead, identity.ScopeAdmin}})
	if err != nil {
		t.Fatal(err)
	}
	b := backupBrowser{t: t, base: origin, token: human.Token}
	docs := t.TempDir()
	write := func(name string, value any) string {
		t.Helper()
		path := filepath.Join(docs, name)
		if err := client.SaveState(path, value); err != nil {
			t.Fatal(err)
		}
		return path
	}
	humanFile := write("human.json", map[string]any{"schema": "lantai.client-session/v1", "origin": origin, "token": human.Token})
	agentFile := write("agent.json", map[string]any{"schema": "lantai.client-session/v1", "origin": origin, "token": agent.Token})
	run := func(session string, wantError bool, args ...string) map[string]any {
		t.Helper()
		argv := append([]string{"backups"}, args...)
		argv = append(argv, "--server", origin, "--allow-http", "--session-file", session)
		var out, stderr bytes.Buffer
		exit := cli.Run(t.Context(), argv, &out, &stderr)
		if (exit != 0) != wantError {
			t.Fatalf("CLI %v: %d %s %s", args, exit, out.String(), stderr.String())
		}
		if wantError {
			return consistencyJSON(t, stderr.Bytes())
		}
		return consistencyJSON(t, out.Bytes())
	}
	empty := run(humanFile, false, "history", "--explicit-completion", "--limit", "1")
	if empty["complete"] != true || len(empty["items"].([]any)) != 0 {
		t.Fatal(empty)
	}
	input := write("start.json", map[string]any{})
	missing := run(humanFile, true, "start", "--input", input, "--idempotency-key", "missing-approval")
	if !strings.Contains(string(mustJSON(missing)), "HUMAN_PROOF_REQUIRED") {
		t.Fatal("missing approval not preserved", missing)
	}
	command := identity.ApproveBackup{Kind: "start", DrainSeconds: 15, TimeoutSeconds: 60}
	grant := backupApproval(t, e, b, string(identity.ActBackupStart), command)
	input = write("start.json", map[string]any{"grant_id": grant, "drain_seconds": 15, "timeout_seconds": 60})
	for _, args := range [][]string{{"overview"}, {"history", "--explicit-completion", "--limit", "1"}, {"schedule-get"}, {"start", "--input", input, "--idempotency-key", "agent-denied"}} {
		denied := run(agentFile, true, args...)
		if !strings.Contains(string(mustJSON(denied)), "FORBIDDEN") {
			t.Fatal("Agent gained backup authority", denied)
		}
	}
	// Hold an already-admitted real business request so cancellation precedes
	// capture; no test double replaces the coordinator or its maintenance lock.
	browser := backupLogin(t, e, origin, []identity.Scope{identity.ScopeAdmin, identity.ScopeRead})
	release := holdBackupBusinessRequest(t, browser)
	started := run(humanFile, false, "start", "--input", input, "--idempotency-key", "original-backup-start")
	id := started["id"].(string)
	run(humanFile, false, "cancel", "--id", id)
	deadline := time.Now().Add(5 * time.Second)
	for {
		job := run(humanFile, false, "show", "--id", id)
		if job["state"] == "cancelled" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cancellation did not exit", job)
		}
		time.Sleep(25 * time.Millisecond)
	}
	release()
	resume := identity.ApproveBackup{Kind: "resume", JobID: ids.ID(id), DrainSeconds: 15, TimeoutSeconds: 60}
	resumeGrant := backupApproval(t, e, b, string(identity.ActBackupResume), resume)
	resumeFile := write("resume.json", map[string]any{"grant_id": resumeGrant, "drain_seconds": 15, "timeout_seconds": 60})
	resumed := run(humanFile, false, "resume", "--id", id, "--input", resumeFile, "--idempotency-key", "original-backup-resume")
	if resumed["id"] != id || resumed["attempt"] != float64(2) {
		t.Fatal("resume changed original job", resumed)
	}
	completed := awaitBackup(t, b, ids.ID(id))
	replay := run(humanFile, false, "resume", "--id", id, "--input", resumeFile, "--idempotency-key", "original-backup-resume")
	if replay["id"] != id || replay["state"] != "complete" || replay["attempt"] != float64(2) {
		t.Fatal("resume replay repeated work", replay)
	}
	if completed.Package == nil {
		t.Fatal("missing verified package receipt")
	}
	// A second genuine snapshot exercises continuation without journal edits.
	secondGrant := backupApproval(t, e, b, string(identity.ActBackupStart), identity.ApproveBackup{Kind: "start"})
	secondFile := write("second.json", map[string]any{"grant_id": secondGrant})
	second := run(humanFile, false, "start", "--input", secondFile, "--idempotency-key", "second-backup-start")
	awaitBackup(t, b, ids.ID(second["id"].(string)))
	firstPage := run(humanFile, false, "history", "--explicit-completion", "--limit", "1")
	if firstPage["complete"] != false || firstPage["next_cursor"] == nil {
		t.Fatal("history silently truncated", firstPage)
	}
	lastPage := run(humanFile, false, "history", "--explicit-completion", "--limit", "1", "--cursor", firstPage["next_cursor"].(string))
	if lastPage["complete"] != true || len(lastPage["items"].([]any)) != 1 || firstPage["items"].([]any)[0].(map[string]any)["id"] == lastPage["items"].([]any)[0].(map[string]any)["id"] {
		t.Fatal("history continuation lost identity", lastPage)
	}
	legacy := run(humanFile, false, "history")
	if legacy["complete"] != nil || len(legacy["items"].([]any)) != 2 {
		t.Fatal("legacy history changed", legacy)
	}
	expired := run(humanFile, true, "history", "--explicit-completion", "--limit", "1", "--cursor", string(ids.New()))
	if !strings.Contains(string(mustJSON(expired)), "CURSOR_EXPIRED") {
		t.Fatal(expired)
	}
	schedule := run(humanFile, false, "schedule-get")
	plan := identity.BackupScheduleInput{Enabled: false, Timezone: "Asia/Shanghai", Time: "02:30", Revision: int64(schedule["revision"].(float64)), DrainSeconds: 10, TimeoutSeconds: 60}
	scheduleGrant := backupApproval(t, e, b, string(identity.ActBackupSchedule), identity.ApproveBackup{Kind: "schedule", Schedule: &plan})
	scheduleFile := write("schedule.json", struct {
		Grant ids.ID `json:"grant_id"`
		identity.BackupScheduleInput
	}{scheduleGrant, plan})
	saved := run(humanFile, false, "schedule-set", "--input", scheduleFile, "--idempotency-key", "original-schedule-change")
	if saved["timezone"] != "Asia/Shanghai" || saved["revision"] != schedule["revision"].(float64)+1 {
		t.Fatal("schedule lost exact revision or IANA zone", saved)
	}
	var history struct {
		Items []backupmgmt.Job `json:"items"`
	}
	if err := json.Unmarshal(mustJSON(legacy), &history); err != nil || len(history.Items) != 2 {
		t.Fatal(err)
	}
	run(agentFile, true, "resume", "--id", id, "--input", resumeFile, "--idempotency-key", "agent-resume-denied")
	run(agentFile, true, "cancel", "--id", id)
	run(agentFile, true, "schedule-set", "--input", scheduleFile, "--idempotency-key", "agent-schedule-denied")
}
