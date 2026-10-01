package tasks

import (
	"context"
	"crypto/rand"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/ledger"
	"github.com/oujinhaoai/lantai/internal/platform/sqlite"
	"github.com/oujinhaoai/lantai/internal/platform/sqlite/migrations"
)

// 单元夹具：真实 runtime 迁移、命令回执/outbox 与锁协调；T01/T03 为显式
// 端口替身，端到端接线另见 tests/integration。

type fakeAuth struct {
	mu    sync.Mutex
	deny  map[string]bool
	epoch int64
	// hidden 中的项目对所有调用者不可见，像非成员一样得到 NOT_FOUND。
	hidden map[ids.ID]bool
}

func (a *fakeAuth) Authorize(_ context.Context, who authz.Context, action authz.Action, res authz.Resource) (authz.Decision, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.hidden[res.ProjectID] {
		return authz.Decision{Code: errcode.NotFound}, nil
	}
	if a.deny[string(action)+"|"+string(who.PrincipalID)] || a.deny[string(action)] {
		return authz.Decision{Code: errcode.Forbidden}, nil
	}
	return authz.Decision{Allowed: true}, nil
}
func (a *fakeAuth) RecoveryEpoch(context.Context) (int64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.epoch, nil
}

type fakePolicies map[string]identity.PolicyValue

func (p fakePolicies) ResolvePolicies(context.Context, ids.ID) (map[string]identity.PolicyValue, error) {
	return p, nil
}

type fakeMembers struct {
	mu      sync.Mutex
	members []identity.Member
}

func (m *fakeMembers) ProjectRecipients(context.Context, ids.ID) ([]identity.Member, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]identity.Member(nil), m.members...), nil
}

type fakeResources struct {
	mu       sync.Mutex
	assets   map[ids.ID]commit.Asset
	versions map[ids.ID]commit.Committed
	latest   map[ids.ID]ids.ID
	locked   map[ids.ID]bool
}

func (r *fakeResources) Asset(_ context.Context, id ids.ID) (commit.Asset, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.assets[id]
	if !ok {
		return a, errcode.New(errcode.NotFound, "")
	}
	return a, nil
}
func (r *fakeResources) VersionByID(_ context.Context, id ids.ID) (commit.Committed, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.versions[id]
	if !ok {
		return v, errcode.New(errcode.NotFound, "")
	}
	return v, nil
}
func (r *fakeResources) LatestVersion(ctx context.Context, asset ids.ID) (commit.Committed, error) {
	r.mu.Lock()
	id, ok := r.latest[asset]
	r.mu.Unlock()
	if !ok {
		return commit.Committed{}, errcode.New(errcode.NotFound, "")
	}
	return r.VersionByID(ctx, id)
}
func (r *fakeResources) CheckVersionRead(context.Context, ids.ID, ids.ID) error { return nil }
func (r *fakeResources) CheckAssetWrite(_ context.Context, id ids.ID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.locked[id] {
		return errcode.New(errcode.AssetLocked, "")
	}
	return nil
}

type fakeDiscussions map[ids.ID]ledger.Message

func (d fakeDiscussions) DiscussionMessage(_ context.Context, id ids.ID) (ledger.Message, error) {
	m, ok := d[id]
	if !ok {
		return m, errcode.New(errcode.NotFound, "")
	}
	return m, nil
}

type fakeAuthorities struct {
	reviews  map[ids.ID]ledger.ReviewFact
	evidence map[ids.ID]ledger.AcceptedEvidence
}

func (a *fakeAuthorities) ReviewByOperation(_ context.Context, op ids.ID) (ledger.ReviewFact, error) {
	f, ok := a.reviews[op]
	if !ok {
		return f, errcode.New(errcode.NotFound, "")
	}
	return f, nil
}
func (a *fakeAuthorities) EvidenceByOperation(_ context.Context, _ authz.Context, op ids.ID) (ledger.AcceptedEvidence, error) {
	e, ok := a.evidence[op]
	if !ok {
		return e, errcode.New(errcode.NotFound, "")
	}
	return e, nil
}

type fixture struct {
	t        *testing.T
	s        *Service
	db       interface{ Close() error }
	clk      *clock.Fake
	auth     *fakeAuth
	members  *fakeMembers
	res      *fakeResources
	msgs     fakeDiscussions
	facts    *fakeAuthorities
	instance ids.ID
	project  ids.ID
	owner    authz.Context
	makerA   authz.Context
	makerB   authz.Context
	checker  authz.Context
	keyN     int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "runtime.db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, m := range migrations.For(ownership.Runtime) {
		if _, err = db.ExecContext(t.Context(), m.SQL); err != nil {
			t.Fatal(err)
		}
	}
	gate := commands.NewGate(commands.NewCoordinator())
	gate.Open()
	clk := clock.NewFake(time.Date(2026, 9, 29, 9, 0, 0, 0, time.UTC))
	f := &fixture{t: t, db: db, clk: clk, auth: &fakeAuth{deny: map[string]bool{}, epoch: 1}, members: &fakeMembers{}, msgs: fakeDiscussions{},
		res:      &fakeResources{assets: map[ids.ID]commit.Asset{}, versions: map[ids.ID]commit.Committed{}, latest: map[ids.ID]ids.ID{}, locked: map[ids.ID]bool{}},
		facts:    &fakeAuthorities{reviews: map[ids.ID]ledger.ReviewFact{}, evidence: map[ids.ID]ledger.AcceptedEvidence{}},
		instance: ids.New(), project: ids.New()}
	f.owner = f.principal(authz.Human, identity.RoleOwner)
	f.makerA = f.principal(authz.Agent, identity.RoleContributor)
	f.makerB = f.principal(authz.Agent, identity.RoleContributor)
	f.checker = f.principal(authz.Agent, identity.RoleChecker)
	f.s, err = New(Deps{DB: db, Gate: gate, Authority: f.auth, Policies: fakePolicies{}, Members: f.members, Resources: f.res, Discussions: f.msgs, Authorities: f.facts,
		Clock: clk, IDs: &ids.Generator{Clock: clk, Rand: rand.Reader}, InstanceID: f.instance})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) principal(kind authz.PrincipalKind, roles ...identity.Role) authz.Context {
	who := authz.Context{InstanceID: f.instance, PrincipalID: ids.New(), PrincipalKind: kind, SessionID: ids.New(), AuthEpoch: 1, RecoveryEpoch: 1, ExpiresAt: f.clk.Now().Add(24 * time.Hour)}
	f.members.members = append(f.members.members, identity.Member{PrincipalID: who.PrincipalID, Kind: kind, Roles: roles})
	return who
}

// session 返回同一主体的另一个会话。
func (f *fixture) session(who authz.Context) authz.Context {
	who.SessionID = ids.New()
	return who
}

func (f *fixture) key() string {
	f.keyN++
	return "k" + string(rune('a'+f.keyN%26)) + ids.New().String()[16:]
}

// version 登记一个已提交版本；asset 为空时新建资产。
func (f *fixture) version(asset ids.ID, by ids.ID) ids.PermanentRef {
	f.res.mu.Lock()
	defer f.res.mu.Unlock()
	if asset == "" {
		asset = ids.New()
		f.res.assets[asset] = commit.Asset{AssetID: asset, ProjectID: f.project}
	}
	v := commit.Committed{OperationID: ids.New(), ProjectID: f.project, AssetID: asset, VersionID: ids.New(), VersionNumber: 1, CommittedBy: by}
	f.res.versions[v.VersionID] = v
	f.res.latest[asset] = v.VersionID
	return v.Ref(f.instance)
}

func (f *fixture) create(in CreateRequest) Result {
	f.t.Helper()
	if in.ProjectID == "" {
		in.ProjectID = f.project
	}
	if in.Type == "" {
		in.Type = "produce"
	}
	if in.Title == "" {
		in.Title = "synthetic task"
	}
	if in.AcceptanceCriteria == nil {
		in.AcceptanceCriteria = []string{"matches the synthetic brief"}
	}
	r, err := f.s.Create(f.t.Context(), f.owner, f.key(), in)
	if err != nil {
		f.t.Fatal(err)
	}
	return r
}

func (f *fixture) view(id ids.ID) View {
	f.t.Helper()
	v, err := f.s.Task(f.t.Context(), f.owner, id)
	if err != nil {
		f.t.Fatal(err)
	}
	return v
}

func (f *fixture) claim(who authz.Context, id ids.ID) (Result, error) {
	v := f.view(id)
	return f.s.Claim(f.t.Context(), who, f.key(), ClaimRequest{ProjectID: f.project, TaskID: id, SeatID: v.Seat.ID, ExpectedRevision: v.Seat.Revision})
}

func (f *fixture) mustClaim(who authz.Context, id ids.ID) Attempt {
	f.t.Helper()
	r, err := f.claim(who, id)
	if err != nil {
		f.t.Fatal(err)
	}
	return *r.Attempt
}

func ref(f *fixture, id ids.ID, a Attempt) AttemptRef {
	return AttemptRef{ProjectID: f.project, TaskID: id, AttemptID: a.ID, ExpectedRevision: a.Revision, Fence: a.Fence}
}

// current 读取当前 Attempt 修订后的引用（续期会递增修订）。
func (f *fixture) current(id ids.ID) AttemptRef {
	v := f.view(id)
	return ref(f, id, *v.Attempt)
}

func expectCode(t *testing.T, err error, code errcode.Code) {
	t.Helper()
	if errcode.CodeOf(err) != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func (f *fixture) message(task ids.ID, kind string, author ids.ID, reply ids.ID) ids.ID {
	id := ids.New()
	f.msgs[id] = ledger.Message{ID: id, AuthorID: author, MessageInput: ledger.MessageInput{Target: ledger.DiscussionTarget{ProjectID: f.project, Kind: "task", ID: task}, Kind: kind, Text: "synthetic", ReplyTo: reply}}
	return id
}

// reviewFact 登记一条绑定某轮交付的审定事实。
func (f *fixture) reviewFact(task ids.ID, a Attempt, round int64, version ids.ID, verdict string) ids.ID {
	op := ids.New()
	flow := ledger.ReviewFlow{TaskID: task, AttemptID: a.ID, Round: round, Fence: a.Fence.LeaseFence}
	var rv ledger.Review
	rv.ID, rv.OperationID, rv.VersionID, rv.Verdict, rv.ActorID = ids.New(), op, version, verdict, f.owner.PrincipalID
	var target ledger.ReviewTarget
	target.ID, target.VersionID, target.Flow = ids.New(), version, flow
	f.facts.reviews[op] = ledger.ReviewFact{Review: rv, Target: target}
	return op
}

func (f *fixture) guard(who authz.Context, asset ids.ID, a *Attempt) error {
	cmd := commands.Context{OperationID: ids.New(), IdempotencyKey: "commit", CommandType: "ledger.commit_version", ActorID: who.PrincipalID, SessionID: who.SessionID, ProjectID: f.project, RecoveryEpoch: 1}
	if a != nil {
		cmd.TaskID, cmd.AttemptID, cmd.LeaseFence = a.TaskID, a.ID, a.Fence.LeaseFence
	}
	return f.s.CheckVersionWrite(f.t.Context(), who, cmd, f.project, asset)
}
