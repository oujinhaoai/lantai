package identity

import (
	"context"
	"strings"
	"testing"

	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

func TestBackupApprovalBindsRequestAndUsesDurableReplay(t *testing.T) {
	f := newFixture(t)
	who := f.adminLogin().Context
	cmd := &ApproveBackup{Kind: "start", DrainSeconds: 20, TimeoutSeconds: 60}
	ch, err := f.svc.CreateChallenge(t.Context(), who, cmd)
	if err != nil {
		t.Fatal(err)
	}
	if ch.Action != ActBackupStart || len(ch.Targets) != 1 || ch.Targets[0].ID != "instance" || !strings.Contains(ch.Summary, "Drain limit 20 seconds; total limit 60 seconds") {
		t.Fatalf("challenge %+v", ch)
	}
	g, err := f.svc.VerifyChallenge(t.Context(), who, ch.ChallengeID, f.fresh(f.adminSecret), "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.Execute(t.Context(), who, g.GrantID, "backup-key", &ApproveBackup{Kind: "start", DrainSeconds: 21, TimeoutSeconds: 60})
	wantCode(t, err, errcode.HumanGrantMismatch)
	out, err := f.svc.Execute(t.Context(), who, g.GrantID, "backup-key", cmd)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := f.svc.Execute(t.Context(), who, g.GrantID, "backup-key", cmd)
	if err != nil || !replay.Replayed || out.OperationID != replay.OperationID {
		t.Fatalf("replay %+v %v", replay, err)
	}
	_, err = f.svc.Execute(t.Context(), who, g.GrantID, "different-key", cmd)
	wantCode(t, err, errcode.IdempotencyConflict)
	// An approval receipt never launches work or grants ordinary authorization.
	d := f.decision(who, ActBackupStart, "")
	if d.Code != errcode.HumanProofRequired {
		t.Fatalf("direct decision %+v", d)
	}
}

func TestBackupAdminReadAndCancelRemainReadOnlyDuringMaintenance(t *testing.T) {
	f := newFixture(t)
	session := f.adminLogin()
	who := session.Context
	m, err := f.inst.Maintain(t.Context(), "backup")
	if err != nil {
		t.Fatal(err)
	}
	defer m.End()
	// The exact same database connection becomes read-only. Authentication and
	// final live admin authorization must not perform activity/session writes.
	f.svc.main.SetMaxOpenConns(1)
	f.svc.runtime.SetMaxOpenConns(1)
	if _, err = f.svc.main.ExecContext(t.Context(), "PRAGMA query_only=ON"); err != nil {
		t.Fatal(err)
	}
	if _, err = f.svc.runtime.ExecContext(t.Context(), "PRAGMA query_only=ON"); err != nil {
		t.Fatal(err)
	}
	defer f.svc.main.ExecContext(context.Background(), "PRAGMA query_only=OFF")
	defer f.svc.runtime.ExecContext(context.Background(), "PRAGMA query_only=OFF")
	if _, err = f.svc.AuthenticateToken(t.Context(), session.Token); err != nil {
		t.Fatal(err)
	}
	if _, err = f.svc.WhoAmI(t.Context(), who); err != nil {
		t.Fatal(err)
	}
	for _, a := range []authz.Action{ActBackupRead, ActBackupCancel} {
		err = f.svc.BeginRead(t.Context(), who, a, authz.Resource{}, func(context.Context, authz.Decision) error { return nil })
		if err != nil {
			t.Fatal(a, err)
		}
	}
	_, err = f.svc.CreateChallenge(t.Context(), who, &ApproveBackup{Kind: "start"})
	wantCode(t, err, errcode.MaintenanceMode)
}

func TestBackupAuthorityRejectsScopePrincipalAndRevokedSession(t *testing.T) {
	f := newFixture(t)
	admin := f.adminLogin()
	limited, err := f.svc.Login(t.Context(), LoginRequest{Name: "ada", Password: adminPassword, Code: f.fresh(f.adminSecret), Channel: ChannelCLI, Scopes: []Scope{ScopeSelf, ScopeRead}})
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range []authz.Action{ActBackupRead, ActBackupCancel, ActBackupStart, ActBackupResume, ActBackupSchedule} {
		if f.decision(limited.Context, action, "").Allowed {
			t.Fatal("scope escalation", action)
		}
	}
	person, setup := f.register(admin.Context, authz.Human, "not-admin")
	secret := f.completeSetup("not-admin", setup, adminPassword)
	nonadmin := f.login("not-admin", adminPassword, secret, ChannelCLI)
	agent, _ := f.register(admin.Context, authz.Agent, "backup-agent@test")
	agentSession := f.exchange(f.issueToken(admin.Context, agent, []Scope{ScopeSelf, ScopeRead}), SessionRequest{})
	for _, who := range []authz.Context{nonadmin.Context, agentSession.Context} {
		for _, action := range []authz.Action{ActBackupRead, ActBackupCancel, ActBackupStart, ActBackupSchedule} {
			if f.decision(who, action, "").Allowed {
				t.Fatalf("unauthorized principal %s got %s", who.PrincipalID, action)
			}
		}
		_, err = f.svc.CreateChallenge(t.Context(), who, &ApproveBackup{Kind: "start"})
		wantCode(t, err, errcode.Forbidden)
	}
	_ = person
	if err = f.svc.EndSession(t.Context(), admin.Context, admin.Session.SessionID); err != nil {
		t.Fatal(err)
	}
	err = f.svc.BeginRead(t.Context(), admin.Context, ActBackupRead, authz.Resource{}, func(context.Context, authz.Decision) error { return nil })
	wantCode(t, err, errcode.TokenRevoked)
}

func TestBackupCommandRejectsMalformedAndBindsScheduleRevision(t *testing.T) {
	for _, cmd := range []*ApproveBackup{
		{Kind: "other"}, {Kind: "start", JobID: ids.New()}, {Kind: "resume"}, {Kind: "start", DrainSeconds: -1},
		{Kind: "schedule"}, {Kind: "schedule", Schedule: &BackupScheduleInput{Time: "24:00", Timezone: "UTC"}},
		{Kind: "schedule", Schedule: &BackupScheduleInput{Time: "03:00", Timezone: "Local"}},
		{Kind: "schedule", Schedule: &BackupScheduleInput{Time: "03:00", Timezone: "UTC", Revision: -1}},
	} {
		if _, err := bind(cmd); err == nil {
			t.Fatalf("accepted %+v", cmd)
		}
	}
	a, _ := bind(&ApproveBackup{Kind: "schedule", Schedule: &BackupScheduleInput{Time: "03:00", Timezone: "UTC", Revision: 1}})
	b, _ := bind(&ApproveBackup{Kind: "schedule", Schedule: &BackupScheduleInput{Time: "03:00", Timezone: "UTC", Revision: 2}})
	if a.requestHash == b.requestHash || a.targetDigest == b.targetDigest {
		t.Fatal("revision not approval-bound")
	}
}

func TestBackupScheduleOwnerRevalidatesRoleAndStateWithoutSession(t *testing.T) {
	f := newFixture(t)
	if err := f.svc.CheckBackupScheduleOwner(t.Context(), f.admin.ID); err != nil {
		t.Fatal(err)
	}
	// Expiring/revoking a login is independent of the explicitly approved plan.
	session := f.adminLogin()
	if err := f.svc.EndSession(t.Context(), session.Context, session.Session.SessionID); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.CheckBackupScheduleOwner(t.Context(), f.admin.ID); err != nil {
		t.Fatal(err)
	}
	f.svc.main.SetMaxOpenConns(1)
	if _, err := f.svc.main.ExecContext(t.Context(), "PRAGMA query_only=ON"); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.CheckBackupScheduleOwner(t.Context(), f.admin.ID); err != nil {
		t.Fatalf("owner check wrote identity DB: %v", err)
	}
	if _, err := f.svc.main.ExecContext(t.Context(), "PRAGMA query_only=OFF"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.main.ExecContext(t.Context(), "UPDATE identity_principals SET state = 'disabled' WHERE principal_id = ?", f.admin.ID); err != nil {
		t.Fatal(err)
	}
	wantCode(t, f.svc.CheckBackupScheduleOwner(t.Context(), f.admin.ID), errcode.Forbidden)
	if _, err := f.svc.main.ExecContext(t.Context(), "UPDATE identity_principals SET state = 'active' WHERE principal_id = ?", f.admin.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.main.ExecContext(t.Context(), "DELETE FROM identity_system_roles WHERE principal_id = ?", f.admin.ID); err != nil {
		t.Fatal(err)
	}
	wantCode(t, f.svc.CheckBackupScheduleOwner(t.Context(), f.admin.ID), errcode.Forbidden)
	wantCode(t, f.svc.CheckBackupScheduleOwner(t.Context(), ids.New()), errcode.Forbidden)
}
