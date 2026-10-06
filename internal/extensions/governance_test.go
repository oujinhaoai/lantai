package extensions_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/commands"
	ae "github.com/oujinhaoai/lantai/internal/contract/agentexec"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/execution"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	tc "github.com/oujinhaoai/lantai/internal/contract/tasks"
	"github.com/oujinhaoai/lantai/internal/extensions"
	"github.com/oujinhaoai/lantai/internal/extensions/exttest"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/platform/sqlite"
	"github.com/oujinhaoai/lantai/internal/platform/sqlite/migrations"
	"github.com/oujinhaoai/lantai/internal/storage"
)

var instance = ids.MustParse("01K00000000000000000000000")

type fakeAuthority struct{ admin ids.ID }

func (a fakeAuthority) Authorize(_ context.Context, who authz.Context, act authz.Action, _ authz.Resource) (authz.Decision, error) {
	if act == identity.ActListExtensionCLIs || who.PrincipalID == a.admin {
		return authz.Decision{Allowed: true}, nil
	}
	return authz.Decision{Allowed: false, Code: errcode.Forbidden}, nil
}
func (fakeAuthority) RecoveryEpoch(context.Context) (int64, error) { return 1, nil }

// fakePackages serves committed plugin versions from directories; reviews are
// the ledger fact under test and change only through the test.
type fakePackages struct {
	mu      sync.Mutex
	dirs    map[ids.ID]string
	reviews map[ids.ID]extensions.PackageReview
	kind    string
}

func (p *fakePackages) PackageVersion(_ context.Context, _ authz.Context, ref ids.PermanentRef) (extensions.PackageVersion, error) {
	p.mu.Lock()
	dir, ok := p.dirs[ref.VersionID]
	p.mu.Unlock()
	if !ok {
		return extensions.PackageVersion{}, errcode.New(errcode.NotFound, "")
	}
	pkg, err := extensions.ReadPackage(os.DirFS(dir))
	id, version := "org.example.unknown", "0.0.1"
	if err == nil {
		id, version = pkg.Manifest.ID, pkg.Manifest.Version
	}
	kind := "plugin"
	if p.kind != "" {
		kind = p.kind
	}
	return extensions.PackageVersion{Ref: ref, ProjectID: ids.MustParse("01K00000000000000000000100"), ManifestDigest: digest.Of([]byte(ref.VersionID)), AssetType: kind, ExtensionID: id, ExtensionVersion: version}, nil
}
func (p *fakePackages) PackageFiles(_ context.Context, ref ids.PermanentRef) (map[string][]byte, error) {
	p.mu.Lock()
	dir := p.dirs[ref.VersionID]
	p.mu.Unlock()
	out := map[string][]byte{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		b, err := os.ReadFile(path)
		out[filepath.ToSlash(rel)] = b
		return err
	})
	return out, err
}
func (p *fakePackages) PackageReview(_ context.Context, ref ids.PermanentRef) (extensions.PackageReview, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if r, ok := p.reviews[ref.VersionID]; ok {
		return r, nil
	}
	return extensions.PackageReview{State: "draft"}, nil
}
func (p *fakePackages) approve(ref ids.PermanentRef) ids.ID {
	p.mu.Lock()
	defer p.mu.Unlock()
	id := ids.New()
	p.reviews[ref.VersionID] = extensions.PackageReview{Approved: true, ReviewID: id, State: "approved"}
	return id
}
func (p *fakePackages) withdraw(ref ids.PermanentRef) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reviews[ref.VersionID] = extensions.PackageReview{State: "withdrawn"}
}

type fakePolicies struct {
	mu      sync.Mutex
	allowed map[ids.ID][]string
	ints    map[string]int64
}

func (p *fakePolicies) ResolvePolicies(_ context.Context, project ids.ID) (map[string]identity.PolicyValue, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string]identity.PolicyValue{}
	list := p.allowed[project]
	if list == nil {
		list = []string{}
	}
	b, _ := json.Marshal(list)
	out["plugins.allowed"] = identity.PolicyValue{Key: "plugins.allowed", Value: b}
	for k, v := range p.ints {
		b, _ := json.Marshal(v)
		out[k] = identity.PolicyValue{Key: k, Value: b, Revision: 2}
	}
	return out, nil
}
func (p *fakePolicies) allow(project ids.ID, ext ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.allowed[project] = ext
}

// fakeHuman stands in for identity: it accepts only the exact frozen action.
type fakeHuman struct {
	approved  map[ids.ID]identity.HumanAction
	items     map[ids.ID][]identity.HumanGrantItem
	acceptErr error
}

func (h *fakeHuman) grant(a identity.HumanAction) (ids.ID, ids.ID) {
	child := ids.New()
	h.approved[child] = a
	grant := ids.New()
	h.items[grant] = []identity.HumanGrantItem{{OperationID: child, Action: a}}
	return grant, child
}
func (h *fakeHuman) DomainItems(_ context.Context, _ authz.Context, grant ids.ID) ([]identity.HumanGrantItem, error) {
	items, ok := h.items[grant]
	if !ok {
		return nil, errcode.New(errcode.NotFound, "")
	}
	return items, nil
}
func (h *fakeHuman) AcceptHumanItem(ctx context.Context, who authz.Context, grant, child ids.ID, a identity.HumanAction, _ commands.Request, domain identity.HumanDomain) (commands.Receipt, error) {
	if h.acceptErr != nil {
		return commands.Receipt{}, h.acceptErr
	}
	x, _ := canonjson.CanonicalizeValue(a)
	y, _ := canonjson.CanonicalizeValue(h.approved[child])
	if string(x) != string(y) {
		return commands.Receipt{}, errcode.New(errcode.HumanGrantMismatch, "")
	}
	if prior, err := domain.Receipt(ctx, child); err == nil {
		return prior, nil
	}
	cc := commands.Context{OperationID: child, IdempotencyKey: string(child), CommandType: string(a.Action), ActorID: who.PrincipalID, SessionID: who.SessionID, RequestHash: digest.Of(a.Request), RecoveryEpoch: 1, HumanGrantID: grant}
	return domain.Commit(ctx, cc, a)
}

type govEnv struct {
	t       *testing.T
	m       *extensions.Manager
	reg     *extensions.Registry
	main    *sql.DB
	runtime *sql.DB
	gate    *commands.Gate
	pkgs    *fakePackages
	pol     *fakePolicies
	human   *fakeHuman
	admin   authz.Context
	clk     *clock.Fake
	project ids.ID
}

func openDB(t *testing.T, db ownership.Database) *sql.DB {
	t.Helper()
	h, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), string(db)+".db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	for _, m := range migrations.For(db) {
		if _, err = h.ExecContext(t.Context(), m.SQL); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

func newGovEnv(t *testing.T) *govEnv {
	t.Helper()
	e := &govEnv{t: t, main: openDB(t, ownership.Main), runtime: openDB(t, ownership.Runtime), gate: commands.NewGate(commands.NewCoordinator()), clk: clock.NewFake(time.Now()), project: ids.MustParse("01K00000000000000000000200")}
	var err error
	e.reg, err = extensions.New(extensions.Deps{DB: e.main, Gate: e.gate, ReleaseDigest: digest.Of([]byte("release one"))})
	if err != nil {
		t.Fatal(err)
	}
	ctx, h, err := e.gate.Maintain(t.Context(), commands.ReasonStarting)
	if err != nil {
		t.Fatal(err)
	}
	err = e.reg.RegisterBuiltins(ctx)
	h.Release()
	e.gate.Open()
	if err != nil {
		t.Fatal(err)
	}
	e.admin = authz.Context{PrincipalID: ids.New(), SessionID: ids.New(), InstanceID: instance, PrincipalKind: authz.Human}
	e.pkgs = &fakePackages{dirs: map[ids.ID]string{}, reviews: map[ids.ID]extensions.PackageReview{}}
	e.pol = &fakePolicies{allowed: map[ids.ID][]string{}, ints: map[string]int64{}}
	e.human = &fakeHuman{approved: map[ids.ID]identity.HumanAction{}, items: map[ids.ID][]identity.HumanGrantItem{}}
	e.m = e.manager(e.reg)
	return e
}

func (e *govEnv) manager(reg *extensions.Registry) *extensions.Manager {
	m, err := extensions.NewManager(extensions.ManagerDeps{Registry: reg, Main: e.main, Runtime: e.runtime, Gate: e.gate, Authority: fakeAuthority{e.admin.PrincipalID}, Packages: e.pkgs, Policies: e.pol, Clock: e.clk, IDs: &ids.Generator{Clock: e.clk, Rand: rand.Reader}, InstanceID: instance})
	if err != nil {
		e.t.Fatal(err)
	}
	return m
}

// commit stages a package directory as a committed plugin asset version.
func (e *govEnv) commit(s exttest.Spec) (ids.PermanentRef, extensions.Package, string) {
	dir := e.t.TempDir()
	p := exttest.Write(e.t, dir, s)
	ref := ids.PermanentRef{InstanceID: instance, AssetID: ids.New(), VersionID: ids.New()}
	e.pkgs.mu.Lock()
	e.pkgs.dirs[ref.VersionID] = dir
	e.pkgs.mu.Unlock()
	return ref, p, dir
}

func (e *govEnv) enableRequest(p extensions.Package, target, scopeKind string, scopeID ids.ID, config map[string]any, configRevision int64) extensions.EnableRequest {
	raw, _ := json.Marshal(config)
	return extensions.EnableRequest{ExtensionID: p.Manifest.ID, ExtensionVersion: p.Manifest.Version, PackageDigest: p.Digest, Target: target, ScopeKind: scopeKind, ScopeID: scopeID, Config: raw, ConfigRevision: configRevision, Trust: extensions.TrustUnenforced, Probe: target == "server", Reason: "synthetic"}
}

func (e *govEnv) enable(in extensions.EnableRequest) (extensions.ChangeResult, error) {
	a, err := e.m.EnableHumanAction(e.t.Context(), in)
	if err != nil {
		return extensions.ChangeResult{}, err
	}
	grant, child := e.human.grant(a)
	return e.m.Enable(e.t.Context(), e.admin, in, grant, child, e.human)
}

func (e *govEnv) disable(id ids.ID, mode string) extensions.ChangeResult {
	in := extensions.DisableRequest{EnablementID: id, Mode: mode, Reason: "synthetic"}
	a, err := e.m.DisableHumanAction(e.t.Context(), in)
	if err != nil {
		e.t.Fatal(err)
	}
	grant, child := e.human.grant(a)
	out, err := e.m.Disable(e.t.Context(), e.admin, in, grant, child, e.human)
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

func mustCode(t *testing.T, err error, code errcode.Code) {
	t.Helper()
	if errcode.CodeOf(err) != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func TestPackageImportIsStaticImmutableAndGoverned(t *testing.T) {
	e := newGovEnv(t)
	ctx := t.Context()
	// The entry bytes are not executable here: import must never run them.
	ref, pkg, dir := e.commit(exttest.Spec{Server: true})
	if _, err := e.m.Import(ctx, authz.Context{PrincipalID: ids.New(), SessionID: ids.New(), InstanceID: instance}, "import-1", extensions.ImportRequest{AssetID: ref.AssetID, VersionID: ref.VersionID}); errcode.CodeOf(err) != errcode.Forbidden {
		t.Fatal("non-admin import", err)
	}
	rec, err := e.m.Import(ctx, e.admin, "import-1", extensions.ImportRequest{AssetID: ref.AssetID, VersionID: ref.VersionID})
	if err != nil || rec.PackageDigest != pkg.Digest || rec.Source != "package" {
		t.Fatal(rec, err)
	}
	if again, err := e.m.Import(ctx, e.admin, "import-1", extensions.ImportRequest{AssetID: ref.AssetID, VersionID: ref.VersionID}); err != nil || again.OperationID != rec.OperationID {
		t.Fatal("replay", again, err)
	}
	// Same ID/version with different bytes cannot replace the registration.
	ref2, _, _ := e.commit(exttest.Spec{Server: true, Mutate: func(m map[string]any) { m["license"].(map[string]any)["spdx"] = "LicenseRef-Other" }})
	_, err = e.m.Import(ctx, e.admin, "import-2", extensions.ImportRequest{AssetID: ref2.AssetID, VersionID: ref2.VersionID})
	mustCode(t, err, errcode.IdempotencyConflict)
	for name, s := range map[string]exttest.Spec{
		"node": {Server: true, Mutate: func(m map[string]any) {
			m["targets"].(map[string]any)["node"] = m["targets"].(map[string]any)["server"]
			delete(m["targets"].(map[string]any), "server")
			m["contributes"].([]map[string]any)[0]["target"] = "node"
		}},
		"builtin": {Server: true, ID: "org.lantai.corecheck"},
		"network": {Server: true, ID: "org.example.net", Mutate: func(m map[string]any) { m["permissions"].(map[string]any)["network"] = []string{"example.test"} }},
	} {
		r, _, _ := e.commit(s)
		if _, err := e.m.Import(ctx, e.admin, "import-"+name, extensions.ImportRequest{AssetID: r.AssetID, VersionID: r.VersionID}); err == nil {
			t.Fatal(name, "imported")
		}
	}
	e.pkgs.kind = "doc"
	r, _, _ := e.commit(exttest.Spec{Server: true, ID: "org.example.doc"})
	_, err = e.m.Import(ctx, e.admin, "import-doc", extensions.ImportRequest{AssetID: r.AssetID, VersionID: r.VersionID})
	mustCode(t, err, errcode.SchemaInvalid)
	e.pkgs.kind = ""
	// Replacing stored bytes after import is detected before any later use.
	if err = os.WriteFile(filepath.Join(dir, "LICENSE"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.pkgs.approve(ref)
	_, err = e.m.EnableHumanAction(ctx, e.enableRequest(pkg, "server", "instance", "", map[string]any{}, 1))
	if err == nil {
		t.Fatal("changed package bytes accepted")
	}
}

func TestEnableGatesReviewTrustProbeConfigAndAllowlist(t *testing.T) {
	e := newGovEnv(t)
	ctx := t.Context()
	ref, pkg, _ := e.commit(exttest.Spec{Server: true})
	if _, err := e.m.Import(ctx, e.admin, "import", extensions.ImportRequest{AssetID: ref.AssetID, VersionID: ref.VersionID}); err != nil {
		t.Fatal(err)
	}
	req := e.enableRequest(pkg, "server", "instance", "", map[string]any{"mode": "pass"}, 1)
	_, err := e.enable(req)
	mustCode(t, err, errcode.ChecksNotSatisfied)
	e.pkgs.approve(ref)
	bad := req
	bad.Trust = ""
	_, err = e.enable(bad)
	mustCode(t, err, errcode.ExtensionPointUnsupported)
	bad = req
	bad.Probe = false
	_, err = e.enable(bad)
	mustCode(t, err, errcode.PreconditionRequired)
	bad = req
	bad.Config = json.RawMessage(`{"unknown":true}`)
	_, err = e.enable(bad)
	mustCode(t, err, errcode.SchemaInvalid)
	bad = req
	bad.ConfigRevision = 2
	_, err = e.enable(bad)
	mustCode(t, err, errcode.PreconditionFailed)
	// A grant for one config cannot be spent on another request.
	a, err := e.m.EnableHumanAction(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	grant, child := e.human.grant(a)
	other := req
	other.Config = json.RawMessage(`{"mode":"fail"}`)
	_, err = e.m.Enable(ctx, e.admin, other, grant, child, e.human)
	mustCode(t, err, errcode.HumanGrantMismatch)
	out, err := e.m.Enable(ctx, e.admin, req, grant, child, e.human)
	if err != nil || out.Activation == nil || out.Activation.State != "ready" || out.Enablement.Generation != 1 {
		t.Fatalf("%+v %v", out, err)
	}
	processor := pkg.Manifest.ID + ".check"
	_, _, err = e.m.Snapshot(ctx, e.project, processor, 1)
	mustCode(t, err, errcode.Forbidden) // global enablement does not imply project use
	e.pol.allow(e.project, pkg.Manifest.ID)
	s, p, err := e.m.Snapshot(ctx, e.project, processor, 1)
	if err != nil || s.Activation.Generation != 1 || p.Source != "package" || p.ContributionID != processor {
		t.Fatal(s, p, err)
	}
	if err = e.reg.VerifyProducer(ctx, p); err != nil {
		t.Fatal("package producer identity", err)
	}
	forged := p
	forged.PackageDigest = digest.Of([]byte("other"))
	mustCode(t, e.reg.VerifyProducer(ctx, forged), errcode.ExtensionActivationStale)
	// A different core release is a different environment: the probe is stale.
	other2, err := extensions.New(extensions.Deps{DB: e.main, Gate: e.gate, ReleaseDigest: digest.Of([]byte("release two"))})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = e.manager(other2).Snapshot(ctx, e.project, processor, 1)
	mustCode(t, err, errcode.ExtensionActivationStale)
	// A probe that crashes leaves the new generation undispatchable.
	crash := e.enableRequest(pkg, "server", "instance", "", map[string]any{"mode": "probe_crash"}, 2)
	out, err = e.enable(crash)
	if errcode.CodeOf(err) != errcode.ExtensionActivationStale || out.Activation == nil || out.Activation.State != "probe_failed" {
		t.Fatalf("%+v %v", out, err)
	}
	_, _, err = e.m.Snapshot(ctx, e.project, processor, 1)
	mustCode(t, err, errcode.ExtensionActivationStale)
	views, err := e.m.Enablements(ctx, e.admin)
	if err != nil || len(views) != 1 || !slices.Contains(views[0].Reasons, "probe_failed") || views[0].Host == nil || views[0].Host.Sandbox {
		t.Fatalf("%+v %v", views, err)
	}
}

func TestEnableReplayRetainsGenerationAndProbe(t *testing.T) {
	e := newGovEnv(t)
	ctx := t.Context()
	ref, pkg, _ := e.commit(exttest.Spec{Server: true})
	if _, err := e.m.Import(ctx, e.admin, "import", extensions.ImportRequest{AssetID: ref.AssetID, VersionID: ref.VersionID}); err != nil {
		t.Fatal(err)
	}
	e.pkgs.approve(ref)
	req := e.enableRequest(pkg, "server", "instance", "", map[string]any{"mode": "pass"}, 1)
	a, err := e.m.EnableHumanAction(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	grant, child := e.human.grant(a)
	first, err := e.m.Enable(ctx, e.admin, req, grant, child, e.human)
	if err != nil || first.Activation == nil || first.Activation.State != "ready" {
		t.Fatal(first, err)
	}
	// Equivalent JSON formatting retains the request binding. Advancing time
	// also detects a probe re-run through its ID and UpdatedAt in the full reply.
	req.Config = json.RawMessage(`{ "mode" : "pass" }`)
	e.clk.Advance(time.Minute)
	replay, err := e.m.Enable(ctx, e.admin, req, grant, child, e.human)
	if err != nil || encodeJSON(first) != encodeJSON(replay) {
		t.Fatal("enable replay changed the result or probe", replay, err)
	}
	bad := req
	bad.Reason = "replaced after approval"
	_, err = e.m.Enable(ctx, e.admin, bad, grant, child, e.human)
	mustCode(t, err, errcode.HumanGrantMismatch)
	_, err = e.m.Enable(ctx, e.admin, req, grant, ids.New(), e.human)
	mustCode(t, err, errcode.HumanGrantMismatch)
	// Replaying generation 1 after disable/re-enable must not expose or mutate
	// generation 3, nor overwrite the retained generation-1 activation.
	e.disable(first.Enablement.ID, "revoke")
	current, err := e.enable(req)
	if err != nil || current.Enablement.Generation != 3 {
		t.Fatal(current, err)
	}
	replay, err = e.m.Enable(ctx, e.admin, req, grant, child, e.human)
	if err != nil || encodeJSON(first) != encodeJSON(replay) {
		t.Fatal("old enable replay returned the current generation", replay, err)
	}
	views, err := e.m.Enablements(ctx, e.admin)
	if err != nil || len(views) != 1 || views[0].Generation != 3 || views[0].Activation.ProbeID != current.Activation.ProbeID {
		t.Fatal("replay modified current activation", views, err)
	}
}

func TestDisableReplayRechecksProofAndRetainsResult(t *testing.T) {
	e := newGovEnv(t)
	ctx := t.Context()
	ref, pkg, _ := e.commit(exttest.Spec{Server: true})
	if _, err := e.m.Import(ctx, e.admin, "import", extensions.ImportRequest{AssetID: ref.AssetID, VersionID: ref.VersionID}); err != nil {
		t.Fatal(err)
	}
	e.pkgs.approve(ref)
	req := e.enableRequest(pkg, "server", "instance", "", map[string]any{"mode": "pass"}, 1)
	enabled, err := e.enable(req)
	if err != nil {
		t.Fatal(err)
	}
	in := extensions.DisableRequest{EnablementID: enabled.Enablement.ID, Mode: "revoke", Reason: "synthetic"}
	a, err := e.m.DisableHumanAction(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	grant, child := e.human.grant(a)
	first, err := e.m.Disable(ctx, e.admin, in, grant, child, e.human)
	if err != nil || first.Enablement == nil || first.Enablement.State != "disabled" {
		t.Fatal(first, err)
	}
	e.human.acceptErr = errcode.New(errcode.TokenRevoked, "")
	_, err = e.m.Disable(ctx, e.admin, in, grant, child, e.human)
	mustCode(t, err, errcode.TokenRevoked) // persisted receipt cannot bypass identity
	e.human.acceptErr = nil
	bad := in
	bad.Mode = "drain"
	_, err = e.m.Disable(ctx, e.admin, bad, grant, child, e.human)
	mustCode(t, err, errcode.HumanGrantMismatch)
	req.Config = json.RawMessage(`{"mode":"pass","sleep_ms":1000}`)
	req.ConfigRevision = 2
	current, err := e.enable(req)
	if err != nil {
		t.Fatal(err)
	}
	e.pol.allow(e.project, pkg.Manifest.ID)
	processor := pkg.Manifest.ID + ".check"
	snapshot, _, err := e.m.Snapshot(ctx, e.project, processor, 1)
	if err != nil {
		t.Fatal(err)
	}
	type jobResult struct {
		out extensions.InvocationResult
		id  ids.ID
		err error
	}
	done := make(chan jobResult, 1)
	go func() {
		out, id, err := e.runJob(processor, snapshot, nil)
		done <- jobResult{out, id, err}
	}()
	// Wait for actual admission of a later-generation host, then let Run track
	// its cancellation handle before replaying the old revoke operation.
	deadline := time.Now().Add(5 * time.Second)
	for {
		var n int
		if err := e.runtime.QueryRowContext(ctx, `SELECT count(*) FROM extensions_invocations WHERE generation=?`, current.Enablement.Generation).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("later-generation host was not admitted")
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	replay, err := e.m.Disable(ctx, e.admin, in, grant, child, e.human)
	if err != nil || encodeJSON(first) != encodeJSON(replay) {
		t.Fatal("disable replay changed the committed response", replay, err)
	}
	views, err := e.m.Enablements(ctx, e.admin)
	if err != nil || len(views) != 1 || views[0].State != "enabled" || views[0].Generation != current.Enablement.Generation {
		t.Fatal("old disable replay changed the new generation", views, err)
	}
	select {
	case result := <-done:
		if result.err != nil || execution.ClassifyInvocation(result.out.Observation) != execution.InvocationCompleted {
			t.Fatal("old revoke replay cancelled the new generation's host", result.err, result.out.Observation)
		}
		if err := e.m.CheckSnapshot(ctx, e.project, snapshot, result.id, 1); err != nil {
			t.Fatal("new-generation result was rejected", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("later-generation host did not finish")
	}
}

func encodeJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func (e *govEnv) runJob(processor string, s ae.ActivationSnapshot, input []byte) (extensions.InvocationResult, ids.ID, error) {
	e.t.Helper()
	p := storage.Producer{ExtensionID: s.Activation.ExtensionID, ExtensionVersion: s.Activation.ExtensionVersion, PackageDigest: s.Activation.PackageDigest, Source: "package", ContributionID: processor}
	attempt := ids.New()
	ref := ids.PermanentRef{InstanceID: instance, AssetID: ids.MustParse("01K00000000000000000000001"), VersionID: ids.MustParse("01K00000000000000000000002")}
	d := digest.Digest("sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc")
	in := extensions.ProcessorInput{Contract: "lantai.processor-input/v1", OperationID: ids.New(), Fence: ae.JobFence{JobID: ids.New(), JobAttemptID: attempt, LeaseFence: 1, RecoveryEpoch: 1}, InputRefs: []ids.PermanentRef{ref}, InputDigest: d, Deadline: clock.Format(time.Now().Add(20 * time.Second)), Activation: s, Producer: p}
	if input == nil {
		input = []byte(`{"instance_id":"01K00000000000000000000000","asset_id":"01K00000000000000000000001","version_id":"01K00000000000000000000002","manifest_digest":"sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"}`)
	}
	out, err := e.m.Run(e.t.Context(), e.project, in, input)
	return out, attempt, err
}

func TestDispatchDrainRevokeAndReviewWithdrawal(t *testing.T) {
	e := newGovEnv(t)
	ctx := t.Context()
	ref, pkg, _ := e.commit(exttest.Spec{Server: true})
	if _, err := e.m.Import(ctx, e.admin, "import", extensions.ImportRequest{AssetID: ref.AssetID, VersionID: ref.VersionID}); err != nil {
		t.Fatal(err)
	}
	e.pkgs.approve(ref)
	e.pol.allow(e.project, pkg.Manifest.ID)
	first, err := e.enable(e.enableRequest(pkg, "server", "project", e.project, map[string]any{"mode": "pass"}, 1))
	if err != nil {
		t.Fatal(err)
	}
	processor := pkg.Manifest.ID + ".check"
	s1, _, err := e.m.Snapshot(ctx, e.project, processor, 1)
	if err != nil {
		t.Fatal(err)
	}
	out, inv1, err := e.runJob(processor, s1, nil)
	if err != nil || execution.ClassifyInvocation(out.Observation) != execution.InvocationCompleted || out.Result == nil || out.Result.Checks[0].Verdict != execution.VerdictPass {
		t.Fatalf("%+v %v", out, err)
	}
	if err = e.m.CheckSnapshot(ctx, e.project, s1, inv1, 1); err != nil {
		t.Fatal(err)
	}
	// Normal config change drains generation 1: its admitted result is still
	// accepted until the frozen deadline, but it cannot start new calls.
	second, err := e.enable(e.enableRequest(pkg, "server", "project", e.project, map[string]any{"mode": "fail"}, 2))
	if err != nil || second.Enablement.Generation != 2 || second.Enablement.Retired == nil || second.Enablement.Retired.Mode != "drain" {
		t.Fatalf("%+v %v", second, err)
	}
	if err = e.m.CheckSnapshot(ctx, e.project, s1, inv1, 1); err != nil {
		t.Fatal("drained result rejected", err)
	}
	_, _, err = e.runJob(processor, s1, nil)
	mustCode(t, err, errcode.ExtensionActivationStale)
	s2, _, err := e.m.Snapshot(ctx, e.project, processor, 1)
	if err != nil {
		t.Fatal(err)
	}
	out, inv2, err := e.runJob(processor, s2, nil)
	if err != nil || out.Result == nil || out.Result.Checks[0].Verdict != execution.VerdictFail {
		t.Fatal("legal fail", out, err)
	}
	e.clk.Advance(time.Minute)
	mustCode(t, e.m.CheckSnapshot(ctx, e.project, s1, inv1, 1), errcode.ExtensionActivationStale)
	// Allowlist removal and review withdrawal revoke acceptance immediately.
	e.pol.allow(e.project)
	mustCode(t, e.m.CheckSnapshot(ctx, e.project, s2, inv2, 1), errcode.Forbidden)
	e.pol.allow(e.project, pkg.Manifest.ID)
	if err = e.m.CheckSnapshot(ctx, e.project, s2, inv2, 1); err != nil {
		t.Fatal(err)
	}
	e.pkgs.withdraw(ref)
	mustCode(t, e.m.CheckSnapshot(ctx, e.project, s2, inv2, 1), errcode.ExtensionActivationStale)
	_, _, err = e.m.Snapshot(ctx, e.project, processor, 1)
	mustCode(t, err, errcode.ExtensionActivationStale)
	e.pkgs.approve(ref)
	// A re-approval is a different review fact: re-enable before use.
	_, _, err = e.m.Snapshot(ctx, e.project, processor, 1)
	mustCode(t, err, errcode.ExtensionActivationStale)
	third, err := e.enable(e.enableRequest(pkg, "server", "project", e.project, map[string]any{"mode": "fail"}, 2))
	if err != nil || third.Enablement.Generation != 3 {
		t.Fatal(third, err)
	}
	s3, _, err := e.m.Snapshot(ctx, e.project, processor, 1)
	if err != nil {
		t.Fatal(err)
	}
	_, inv3, err := e.runJob(processor, s3, nil)
	if err != nil {
		t.Fatal(err)
	}
	e.disable(first.Enablement.ID, "revoke")
	mustCode(t, e.m.CheckSnapshot(ctx, e.project, s3, inv3, 1), errcode.ExtensionActivationStale)
	_, _, err = e.m.Snapshot(ctx, e.project, processor, 1)
	mustCode(t, err, errcode.UnsupportedCapability)
}

func TestCLICommandsFollowScopeAndReview(t *testing.T) {
	e := newGovEnv(t)
	ctx := t.Context()
	ref, pkg, _ := e.commit(exttest.Spec{CLI: true})
	if _, err := e.m.Import(ctx, e.admin, "import", extensions.ImportRequest{AssetID: ref.AssetID, VersionID: ref.VersionID}); err != nil {
		t.Fatal(err)
	}
	e.pkgs.approve(ref)
	user := authz.Context{PrincipalID: ids.New(), SessionID: ids.New(), InstanceID: instance}
	req := e.enableRequest(pkg, "cli", "user", user.PrincipalID, map[string]any{}, 1)
	if _, err := e.enable(req); err != nil {
		t.Fatal(err)
	}
	list, err := e.m.CLICommands(ctx, user)
	if err != nil || len(list) != 1 || list[0].Command != "copy" || list[0].PackageDigest != pkg.Digest {
		t.Fatal(list, err)
	}
	if list, err = e.m.CLICommands(ctx, authz.Context{PrincipalID: ids.New(), SessionID: ids.New()}); err != nil || len(list) != 0 {
		t.Fatal("other user sees a user-scoped command", list, err)
	}
	e.pkgs.withdraw(ref)
	if list, err = e.m.CLICommands(ctx, user); err != nil || len(list) != 0 {
		t.Fatal("withdrawn package still listed", list, err)
	}
	empty, _ := tc.SnapshotDigest(nil)
	if !empty.Valid() {
		t.Fatal("digest helper")
	}
}
