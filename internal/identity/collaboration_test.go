package identity

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	"github.com/oujinhaoai/lantai/internal/contract/tasks"
)

// A real domain-owned receipt store in ledger.db; no domain business state is
// simulated as production functionality. This adapter isolates T01's boundary.
type testHumanDomain struct {
	db      *sql.DB
	store   *commands.Store
	calls   int
	failure error
}

func (d *testHumanDomain) Receipt(ctx context.Context, id ids.ID) (commands.Receipt, error) {
	r, e := d.store.ReceiptByOperation(ctx, d.db, id)
	if e != nil {
		return commands.Receipt{}, e
	}
	return *r, nil
}
func (d *testHumanDomain) Commit(ctx context.Context, c commands.Context, a HumanAction) (commands.Receipt, error) {
	if d.failure != nil {
		return commands.Receipt{}, d.failure
	}
	r, e := d.store.Execute(ctx, d.db, c, func(context.Context, *sql.Tx) (commands.Result, error) {
		d.calls++
		return commands.Result{Status: commands.ReceiptSucceeded, ResponseCode: 200, Summary: map[string]any{"target": a.ResourceID}}, nil
	})
	if e != nil {
		return commands.Receipt{}, e
	}
	return *r.Receipt, nil
}
func domainAction(project ids.ID) HumanAction {
	return HumanAction{Action: ActRecordReview, ProjectID: project, ResourceID: ids.New(), ResourceRevision: 3, ManifestDigest: digest.Of([]byte("manifest")), Request: json.RawMessage(`{"decision":"approve","profile_revision":2}`)}
}
func TestHumanBatchPartialRecoveryAndTargetBinding(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	who := f.adminLogin().Context
	project := ids.New()
	f.grantRole(who, project, f.admin, RoleOwner)
	items := []HumanAction{domainAction(project), domainAction(project)}
	ch, err := f.svc.CreateDomainChallenge(ctx, who, items, targetFixture(items))
	if err != nil {
		t.Fatal(err)
	}
	g, err := f.svc.VerifyChallenge(ctx, who, ch.ChallengeID, f.fresh(f.adminSecret), "test")
	if err != nil {
		t.Fatal(err)
	}
	batch, err := f.svc.DomainItems(ctx, who, g.GrantID)
	if err != nil {
		t.Fatal(err)
	}
	if len(batch) != 2 || batch[0].OperationID == batch[1].OperationID {
		t.Fatal(batch)
	}
	store, _ := commands.NewStore("ledger", f.clk)
	d := &testHumanDomain{db: f.inst.DB(ownership.Ledger), store: store}
	accept := func(gid ids.ID, item HumanGrantItem) (commands.Receipt, error) {
		return f.svc.AcceptHumanItem(ctx, who, gid, item.OperationID, item.Action, commands.Request{}, d)
	}
	changed := batch[0]
	changed.Action.ResourceRevision++
	_, err = accept(g.GrantID, changed)
	wantCode(t, err, errcode.HumanGrantMismatch)
	changed = batch[0]
	changed.Action.Request = json.RawMessage(`{"decision":"reject","profile_revision":2}`)
	_, err = accept(g.GrantID, changed)
	wantCode(t, err, errcode.HumanGrantMismatch)
	_, err = f.svc.AcceptHumanItem(ctx, who, g.GrantID, ids.New(), items[0], commands.Request{}, d)
	wantCode(t, err, errcode.HumanGrantMismatch)
	first, err := accept(g.GrantID, batch[0])
	if err != nil {
		t.Fatal(err)
	}
	f.clk.Advance(6 * time.Minute)
	f.svc = f.newService(f.key)
	replay, err := accept(g.GrantID, batch[0])
	if err != nil || replay.OperationID != first.OperationID || d.calls != 1 {
		t.Fatalf("replay %+v %v calls=%d", replay, err, d.calls)
	}
	_, err = accept(g.GrantID, batch[1])
	wantCode(t, err, errcode.HumanProofRequired)
	ch, err = f.svc.RechallengeDomain(ctx, who, ch.OperationID, targetFixture(items))
	if err != nil {
		t.Fatal(err)
	}
	g2, err := f.svc.VerifyChallenge(ctx, who, ch.ChallengeID, f.fresh(f.adminSecret), "test")
	if err != nil {
		t.Fatal(err)
	}
	same, err := f.svc.DomainItems(ctx, who, g2.GrantID)
	if err != nil || same[1].OperationID != batch[1].OperationID {
		t.Fatal(same, err)
	}
	if _, err = accept(g2.GrantID, batch[1]); err != nil {
		t.Fatal(err)
	}
	if _, err = accept(g2.GrantID, batch[0]); err != nil || d.calls != 2 {
		t.Fatalf("calls %d err %v", d.calls, err)
	}
	rev, _ := projectRevision(ctx, f.svc.main, project)
	f.mustSudo(who, f.adminSecret, &SetProjectRole{ProjectID: project, ExpectedRevision: rev, PrincipalID: f.admin.ID, Role: RoleOwner, Grant: false})
	_, err = accept(g2.GrantID, batch[0])
	wantCode(t, err, errcode.NotFound)
}
func TestHumanDomainKindsAndExtensionBinding(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	who := f.adminLogin().Context
	project := ids.New()
	for _, k := range []authz.PrincipalKind{authz.Agent, authz.Node, authz.Worker, authz.Runner, authz.Service} {
		name := map[authz.PrincipalKind]string{authz.Agent: "tester@local", authz.Node: "node:local", authz.Worker: "worker:test@local", authz.Runner: "runner:test@local", authz.Service: "service:test"}[k]
		p, _ := f.register(who, k, name)
		token := f.issueToken(who, p, AllowedScopes(k))
		actor := f.exchange(token, SessionRequest{}).Context
		_, err := f.svc.CreateDomainChallenge(ctx, actor, []HumanAction{domainAction(project)}, targetFixture(nil))
		wantCode(t, err, errcode.Forbidden)
	}
	a := HumanAction{Action: ActEnableExtension, ResourceID: ids.New(), ResourceRevision: 1, Extension: &ExtensionAuthorization{PluginID: "org.example.check", PackageDigest: digest.Of([]byte("package")), Target: "server", ScopeKind: "project", ScopeID: project, ConfigRevision: 2, ConfigDigest: digest.Of([]byte("config")), PolicyRevision: 4}, Request: json.RawMessage(`{"probe":true}`)}
	ch, err := f.svc.CreateDomainChallenge(ctx, who, []HumanAction{a}, targetFixture([]HumanAction{a}))
	if err != nil {
		t.Fatal(err)
	}
	g, err := f.svc.VerifyChallenge(ctx, who, ch.ChallengeID, f.fresh(f.adminSecret), "test")
	if err != nil {
		t.Fatal(err)
	}
	items, _ := f.svc.DomainItems(ctx, who, g.GrantID)
	store, _ := commands.NewStore("extensions", f.clk)
	d := &testHumanDomain{db: f.svc.main, store: store}
	x := *a.Extension
	x.ConfigRevision++
	a.Extension = &x
	_, err = f.svc.AcceptHumanItem(ctx, who, g.GrantID, items[0].OperationID, a, commands.Request{}, d)
	wantCode(t, err, errcode.HumanGrantMismatch)
	if d.calls != 0 {
		t.Fatal("changed config accepted")
	}
}

type taskProgressStub struct {
	tasks []tasks.Task
	err   error
}

func (r taskProgressStub) MilestoneTasks(context.Context, authz.Context, ids.ID, []ids.ID) ([]tasks.Task, error) {
	return r.tasks, r.err
}
func TestMilestoneRevisionOwnershipAndProjectIsolation(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	who := f.adminLogin().Context
	project := ids.New()
	f.grantRole(who, project, f.admin, RoleOwner)
	task := tasks.Task{ID: ids.New(), ProjectID: project, State: "done"}
	r := taskProgressStub{tasks: []tasks.Task{task}}
	m := Milestone{ProjectID: project, Name: "First delivery", DueDate: "2026-10-01", OwnerID: f.admin.ID, TaskIDs: []ids.ID{task.ID}}
	key := f.idemKey()
	first, err := f.svc.PutMilestone(ctx, who, key, 0, m, r)
	if err != nil {
		t.Fatal(err)
	}
	again, err := f.svc.PutMilestone(ctx, who, key, 0, m, r)
	if err != nil || !again.Replayed || first.OperationID != again.OperationID {
		t.Fatal(again, err)
	}
	if err = json.Unmarshal(first.Summary, &m); err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.PutMilestone(ctx, who, f.idemKey(), 2, m, r)
	wantCode(t, err, errcode.PreconditionFailed)
	p, err := f.svc.MilestoneProgress(ctx, who, project, m.ID, r)
	if err != nil || p.Completed != 1 || p.Total != 1 {
		t.Fatal(p, err)
	}
	task.ProjectID = ids.New()
	_, err = f.svc.MilestoneProgress(ctx, who, project, m.ID, taskProgressStub{tasks: []tasks.Task{task}})
	wantCode(t, err, errcode.SchemaInvalid)
	rev, _ := projectRevision(ctx, f.svc.main, project)
	f.mustSudo(who, f.adminSecret, &SetProjectRole{ProjectID: project, ExpectedRevision: rev, PrincipalID: f.admin.ID, Role: RoleOwner, Grant: false})
	_, err = f.svc.PutMilestone(ctx, who, f.idemKey(), 1, m, r)
	wantCode(t, err, errcode.NotFound)
}

func TestHumanBatchSessionRenewalAndStaleDomain(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	who := f.adminLogin().Context
	p := ids.New()
	f.grantRole(who, p, f.admin, RoleOwner)
	a := domainAction(p)
	ch, e := f.svc.CreateDomainChallenge(ctx, who, []HumanAction{a}, targetFixture([]HumanAction{a}))
	if e != nil {
		t.Fatal(e)
	}
	g, e := f.svc.VerifyChallenge(ctx, who, ch.ChallengeID, f.fresh(f.adminSecret), "test")
	if e != nil {
		t.Fatal(e)
	}
	items, e := f.svc.DomainItems(ctx, who, g.GrantID)
	if e != nil {
		t.Fatal(e)
	}
	store, _ := commands.NewStore("ledger", f.clk)
	d := &testHumanDomain{db: f.inst.DB(ownership.Ledger), store: store, failure: errcode.New(errcode.ReviewTargetStale, "target revision changed")}
	_, e = f.svc.AcceptHumanItem(ctx, who, g.GrantID, items[0].OperationID, a, commands.Request{}, d)
	wantCode(t, e, errcode.ReviewTargetStale)
	next := f.adminLogin().Context
	_, e = f.svc.AcceptHumanItem(ctx, next, g.GrantID, items[0].OperationID, a, commands.Request{}, d)
	wantCode(t, e, errcode.HumanGrantMismatch)
	ch, e = f.svc.RechallengeDomain(ctx, next, ch.OperationID, targetFixture([]HumanAction{a}))
	if e != nil {
		t.Fatal(e)
	}
	g, e = f.svc.VerifyChallenge(ctx, next, ch.ChallengeID, f.fresh(f.adminSecret), "test")
	if e != nil {
		t.Fatal(e)
	}
	d.failure = nil
	if _, e = f.svc.AcceptHumanItem(ctx, next, g.GrantID, items[0].OperationID, a, commands.Request{}, d); e != nil {
		t.Fatal(e)
	}
	if d.calls != 1 {
		t.Fatal(d.calls)
	}
}

// Only registered fixture targets can enter a challenge.
type targetFixture []HumanAction

func (f targetFixture) ValidateHumanTarget(_ context.Context, _ authz.Context, a HumanAction) error {
	for _, known := range f {
		if known.ResourceID == a.ResourceID && known.ProjectID == a.ProjectID && known.ResourceRevision == a.ResourceRevision {
			return nil
		}
	}
	return errcode.New(errcode.NotFound, "target not visible in scope")
}
func TestHumanChallengeRequiresAuthoritativeTarget(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	who := f.adminLogin().Context
	p := ids.New()
	f.grantRole(who, p, f.admin, RoleOwner)
	a := domainAction(p)
	known := a
	known.ProjectID = ids.New()
	_, e := f.svc.CreateDomainChallenge(ctx, who, []HumanAction{a}, targetFixture([]HumanAction{known}))
	wantCode(t, e, errcode.NotFound)
	_, e = f.svc.CreateDomainChallenge(ctx, who, []HumanAction{a}, nil)
	wantCode(t, e, errcode.SchemaInvalid)
}
