package integration

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/oujinhaoai/lantai/internal/application"
	"github.com/oujinhaoai/lantai/internal/catalog"
	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/cli"
	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	tc "github.com/oujinhaoai/lantai/internal/contract/tasks"
	"github.com/oujinhaoai/lantai/internal/extensions"
	"github.com/oujinhaoai/lantai/internal/extensions/exttest"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/jobs"
	"github.com/oujinhaoai/lantai/internal/ledger"
	"github.com/oujinhaoai/lantai/internal/mcpserver"
	"github.com/oujinhaoai/lantai/internal/node"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
	"github.com/oujinhaoai/lantai/internal/tasks"
	"github.com/oujinhaoai/lantai/internal/workflow"
)

// appFlow drives real flows on the assembled application: identity/TOTP,
// catalog/storage, ledger reviews, T05 tasks/flows, T06 jobs with the T09
// manager as host, and the real one-shot checker processes.
type appFlow struct {
	*flowEnv
	app    *application.App
	worker authz.Context
}

func newAppFlow(t *testing.T) *appFlow {
	t.Helper()
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	e.setRole(e.admin.Context.PrincipalID, identity.RoleOwner, true)
	a := reopenApplication(t, e, storage.Config{MinFreeBytes: 1 << 20})
	f := &flowEnv{env: e, owner: e.login().Context, tasks: a.Tasks, flows: a.Flows, reviews: a.Reviews, source: a.Evidence, log: a.Events}
	f.maker = e.taskAgent("maker@pc", identity.RoleContributor)
	f.checker = e.taskAgent("checker@pc", identity.RoleChecker)
	return &appFlow{flowEnv: f, app: a, worker: e.taskPrincipal("worker:check@local", identity.RoleChecker, authz.Worker)}
}

// profile commits an acceptance profile accepting producer for every check.
// It is a pre-approved fixture like the other flow tests; configuration
// initialization has its own test.
func (f *appFlow) profile(slug string, types []manifest.AssetType, producer storage.Producer) catalog.VersionResult {
	f.t.Helper()
	p := ledger.AcceptanceProfile{Contract: "lantai.acceptance-profile/v1", ID: slug, Revision: 1, AssetTypes: types, Purpose: authz.PurposeProduction, RequiredEvidence: []string{}, QARequired: true, DistinctActorRule: "maker_checker", WaivableChecks: []string{}, Defaults: ledger.ProfileDefaults{Publication: "auto"}}
	for _, key := range []string{"integrity", "schema", "license_evidence", "purpose"} {
		p.RequiredChecks = append(p.RequiredChecks, ledger.CheckRequirement{Key: key, SchemaVersion: 1, AcceptedProcessors: []storage.Producer{producer}, ConfigDigest: jobs.ConfigDigest(), Severity: "error"})
	}
	var doc any
	raw, _ := json.Marshal(p)
	_ = json.Unmarshal(raw, &doc)
	v := f.config(f.owner, slug, map[string]any{"acceptance_profile": doc})
	if _, err := f.inst.DB(ownership.Ledger).ExecContext(f.t.Context(), `UPDATE ledger_version_states SET state='approved',effective_review_id=? WHERE version_id=?`, ids.New(), v.VersionID); err != nil {
		f.t.Fatal(err)
	}
	return v
}

func (f *appFlow) definition(slug, processor string) catalog.VersionResult {
	f.t.Helper()
	def, err := workflow.BuiltinDefinition("create")
	if err != nil {
		f.t.Fatal(err)
	}
	for i := range def.Steps {
		if def.Steps[i].Job != nil {
			def.Steps[i].Job.Processor = processor
		}
	}
	var doc any
	raw, _ := json.Marshal(def)
	_ = json.Unmarshal(raw, &doc)
	return f.config(f.owner, slug, map[string]any{"flow_definition": doc})
}

// uploadFiles stages several files in one upload session.
func (f *appFlow) uploadFiles(who authz.Context, files map[string][]byte) (ids.ID, []manifest.InputFile) {
	f.t.Helper()
	ctx := f.t.Context()
	specs := []storage.FileSpec{}
	inputs := []manifest.InputFile{}
	seen := map[string]bool{}
	names := []string{}
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		b := files[name]
		role := "source"
		if name == "extension.yaml" {
			role = "primary"
		}
		inputs = append(inputs, manifest.InputFile{Path: name, Role: role, SHA256: shaOf(b), Size: int64(len(b))})
		if !seen[shaOf(b)] {
			seen[shaOf(b)] = true
			specs = append(specs, storage.FileSpec{SHA256: shaOf(b), Size: int64(len(b))})
		}
	}
	u, err := f.storage.CreateUpload(ctx, storage.CreateUploadRequest{Who: who, IdempotencyKey: f.key(), ProjectID: f.project.ProjectID, Files: specs})
	if err != nil {
		f.t.Fatal(err)
	}
	for _, s := range specs {
		var body []byte
		for _, b := range files {
			if shaOf(b) == s.SHA256 {
				body = b
			}
		}
		if _, err = f.storage.PutPart(ctx, storage.PartRequest{Who: who, UploadID: u.UploadID, SHA256: s.SHA256, PartNumber: 1, PartSHA256: s.SHA256, Size: s.Size, Body: bytes.NewReader(body)}); err != nil {
			f.t.Fatal(err)
		}
		if _, err = f.storage.CompleteFile(ctx, who, u.UploadID, s.SHA256); err != nil {
			f.t.Fatal(err)
		}
	}
	return u.UploadID, inputs
}

func readDir(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		b, err := os.ReadFile(p)
		out[filepath.ToSlash(rel)] = b
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// review runs one real create flow to an approved, published version. commit
// produces the candidate for the claimed production task.
func (f *appFlow) review(profile, definition catalog.VersionResult, capability, outputType string, commit func(task ids.ID, a tasks.Attempt) catalog.VersionResult) (ids.ID, jobs.Job) {
	f.t.Helper()
	ctx := f.t.Context()
	start, err := f.flows.Start(ctx, f.owner, f.key(), workflow.StartRequest{ProjectID: f.project.ProjectID, DefinitionRef: definition.Ref, ProfileRef: profile.Ref, Title: "synthetic", AcceptanceCriteria: []string{"valid"}, ExpectedOutputs: []tc.OutputRequirement{{Slug: "out", AssetType: outputType, CandidateCount: 1}}})
	if err != nil {
		f.t.Fatal(err)
	}
	f.dispatch()
	task := f.latest(f.flow(start.FlowID), "produce").StepRun.TaskIDs[0]
	a := f.claim(f.maker, task)
	v := commit(task, a)
	if _, err = f.tasks.Submit(ctx, f.maker, f.key(), tasks.SubmitRequest{AttemptRef: attemptRef(f.flowEnv, task, a), OutputRefs: []ids.PermanentRef{v.Ref}}); err != nil {
		f.t.Fatal(err)
	}
	f.sync()
	f.dispatch()
	list, err := f.app.Jobs.List(ctx, f.owner, f.project.ProjectID, "", 100)
	if err != nil || len(list) == 0 {
		f.t.Fatal(list, err)
	}
	j := list[len(list)-1]
	if _, err = f.app.Nodes.Observe(ctx, f.worker, f.key(), node.Observation{ProjectID: f.project.ProjectID, Capabilities: []string{capability}, Slots: 1}); err != nil {
		f.t.Fatal(err)
	}
	j, err = f.app.Jobs.Run(ctx, f.worker, f.key(), jobs.Control{JobID: j.ID, ExpectedRevision: j.Revision})
	if err != nil || j.State != "succeeded" {
		f.t.Fatalf("%+v %v", j, err)
	}
	for _, c := range j.Checks {
		if c.Check.Verdict != "pass" {
			f.t.Fatalf("check %s failed: %v", c.CheckKey, c.Check.Findings)
		}
	}
	if _, err = f.flows.SyncJobs(ctx, f.owner, 10); err != nil {
		f.t.Fatal(err)
	}
	f.dispatch()
	f.qa(start.FlowID, "pass")
	f.sync()
	f.dispatch()
	view := f.flow(start.FlowID)
	if view.Meta.ReviewTargetID == "" {
		f.t.Fatal("no review target", view)
	}
	f.decide(view.Meta.ReviewTargetID, "approve")
	f.sync()
	f.dispatch()
	f.sync()
	f.dispatch() // production completion follows the persisted publication event
	if view = f.flow(start.FlowID); view.Flow.State != "completed" {
		f.t.Fatal(view.Flow, view.Commands)
	}
	return start.FlowID, j
}

// grant runs one extension change through the real Challenge → TOTP → HumanGrant.
func (f *appFlow) grant(a identity.HumanAction) (authz.Context, ids.ID, ids.ID) {
	f.t.Helper()
	ctx := f.t.Context()
	who := f.login().Context
	ch, err := f.id.CreateDomainChallenge(ctx, who, []identity.HumanAction{a}, f.app)
	if err != nil {
		f.t.Fatal(err)
	}
	g, err := f.id.VerifyChallenge(ctx, who, ch.ChallengeID, f.fresh(), "192.0.2.10")
	if err != nil {
		f.t.Fatal(err)
	}
	items, err := f.id.DomainItems(ctx, who, g.GrantID)
	if err != nil {
		f.t.Fatal(err)
	}
	return who, g.GrantID, items[0].OperationID
}

func TestM2ExtensionPackageReviewEnableCheckAndRevoke(t *testing.T) {
	f := newAppFlow(t)
	ctx := t.Context()
	m := f.app.ExtensionManager
	builtin, err := f.app.Extensions.Producer(ctx, "org.lantai.corecheck.manifest")
	if err != nil {
		t.Fatal(err)
	}
	// 1. The package is an ordinary plugin asset version reviewed in ledger.
	dir := t.TempDir()
	pkg := exttest.Write(t, dir, exttest.Spec{Server: true, CLI: true})
	files := readDir(t, dir)
	profile := f.profile("profiles/base", []manifest.AssetType{manifest.TypeDoc, manifest.TypePlugin}, builtin)
	var plugin ids.PermanentRef
	_, _ = f.review(profile, f.definition("flows/builtin", "org.lantai.corecheck.manifest"), "org.lantai.corecheck.manifest", "plugin", func(task ids.ID, a tasks.Attempt) catalog.VersionResult {
		upload, inputs := f.uploadFiles(f.maker, files)
		v, err := f.catalog.CommitVersion(ctx, catalog.VersionRequest{Who: f.maker, IdempotencyKey: f.key(), UploadID: upload, Slug: "plugins/fixture", Task: bindTo(task, a), Content: catalog.ContentInput{AssetType: manifest.TypePlugin, Rights: rightsOwned(), Files: inputs, Metadata: map[string]any{"extension_id": pkg.Manifest.ID, "extension_version": pkg.Manifest.Version}}})
		if err != nil {
			t.Fatal(err)
		}
		plugin = v.Ref
		return v
	})
	// 2. Static import: only a registration; nothing runs.
	admin := f.login().Context
	rec, err := m.Import(ctx, admin, f.key(), extensions.ImportRequest{AssetID: plugin.AssetID, VersionID: plugin.VersionID})
	if err != nil || rec.PackageDigest != pkg.Digest {
		t.Fatal(rec, err)
	}
	// 3. Enable through a real HumanGrant; the same grant authorizes the probe.
	raw, _ := json.Marshal(map[string]any{"mode": "pass"})
	req := extensions.EnableRequest{ExtensionID: pkg.Manifest.ID, ExtensionVersion: pkg.Manifest.Version, PackageDigest: pkg.Digest, Target: "server", ScopeKind: "instance", Config: raw, ConfigRevision: 1, Trust: extensions.TrustUnenforced, Probe: true, Reason: "synthetic checker"}
	action, err := m.EnableHumanAction(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	who, grant, child := f.grant(action)
	enabled, err := m.Enable(ctx, who, req, grant, child, f.id)
	if err != nil || enabled.Activation == nil || enabled.Activation.State != "ready" {
		t.Fatalf("%+v %v", enabled, err)
	}
	processor := pkg.Manifest.ID + ".check"
	// 4. Global enablement does not imply project use; the allowlist is explicit.
	if _, _, err = m.Snapshot(ctx, f.project.ProjectID, processor, 1); errcode.CodeOf(err) != errcode.Forbidden {
		t.Fatal("instance enablement leaked into a project", err)
	}
	value, _ := json.Marshal([]string{pkg.Manifest.ID})
	f.sudo(&identity.SetPolicy{ProjectID: f.project.ProjectID, Key: "plugins.allowed", Value: value})
	// Reference retention: an enabled package version is a current use.
	preview, err := f.app.Lifecycle.PreviewTrash(ctx, f.login().Context, ledger.TrashSelector{ProjectID: f.project.ProjectID, AssetID: plugin.AssetID, WholeAsset: true, Reason: "cleanup"})
	if err != nil || !slices.ContainsFunc(preview.Uses, func(u ledger.LifecycleUse) bool { return u.Kind == "extension" }) {
		t.Fatal("enabled package version not retained", preview.Uses, err)
	}
	// 5. A second real flow uses the external checker for its job.
	external := storage.Producer{ExtensionID: pkg.Manifest.ID, ExtensionVersion: pkg.Manifest.Version, PackageDigest: pkg.Digest, Source: "package", ContributionID: processor}
	profile2 := f.profile("profiles/external", []manifest.AssetType{manifest.TypeDoc}, external)
	def2 := f.definition("flows/external", processor)
	_, job := f.review(profile2, def2, processor, "doc", func(task ids.ID, a tasks.Attempt) catalog.VersionResult {
		v, err := f.commitDoc(f.maker, "docs/checked", "", "", []byte("externally checked"), bindTo(task, a))
		if err != nil {
			t.Fatal(err)
		}
		return v
	})
	if job.Attempt == nil || job.Attempt.ActivationSnapshot.Activation.ExtensionID != pkg.Manifest.ID || len(job.Evidence) != 4 {
		t.Fatalf("external job evidence: %+v", job)
	}
	for _, c := range job.Checks {
		if c.Check.Producer != external || c.Check.Verdict != "pass" {
			t.Fatal("check not produced by the external package", c.Check)
		}
	}
	// 6. Revocation stops new dispatch immediately; history stays readable.
	views, err := m.Enablements(ctx, admin)
	if err != nil || len(views) != 1 {
		t.Fatal(views, err)
	}
	dreq := extensions.DisableRequest{EnablementID: views[0].ID, Mode: "revoke", Reason: "synthetic revoke"}
	daction, err := m.DisableHumanAction(ctx, dreq)
	if err != nil {
		t.Fatal(err)
	}
	who, grant, child = f.grant(daction)
	if _, err = m.Disable(ctx, who, dreq, grant, child, f.id); err != nil {
		t.Fatal(err)
	}
	if _, _, err = m.Snapshot(ctx, f.project.ProjectID, processor, 1); errcode.CodeOf(err) != errcode.UnsupportedCapability {
		t.Fatal("revoked processor still resolves", err)
	}
	epoch, err := f.inst.RecoveryEpoch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.CheckSnapshot(ctx, f.project.ProjectID, job.Attempt.ActivationSnapshot, job.Attempt.ID, epoch); errcode.CodeOf(err) != errcode.ExtensionActivationStale {
		t.Fatal("revoked activation still accepted", err)
	}
	// The producer identity of historical evidence stays verifiable.
	if err = f.app.Extensions.VerifyProducer(ctx, external); err != nil {
		t.Fatal(err)
	}
}

func TestRemoteExtensionCLIInstallRunMCPAndRevoke(t *testing.T) {
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	e.setRole(e.admin.Context.PrincipalID, identity.RoleOwner, true)
	app := reopenApplication(t, e, storage.Config{MinFreeBytes: 1 << 20})
	f := &appFlow{flowEnv: &flowEnv{env: e, owner: e.login().Context}, app: app}
	ctx := t.Context()
	dir := t.TempDir()
	pkg := exttest.Write(t, dir, exttest.Spec{CLI: true})
	upload, inputs := f.uploadFiles(f.owner, readDir(t, dir))
	v, err := f.catalog.CommitVersion(ctx, catalog.VersionRequest{Who: f.owner, IdempotencyKey: f.key(), UploadID: upload, Slug: "plugins/cli", Content: catalog.ContentInput{AssetType: manifest.TypePlugin, Rights: rightsOwned(), Files: inputs, Metadata: map[string]any{"extension_id": pkg.Manifest.ID, "extension_version": pkg.Manifest.Version}}})
	if err != nil {
		t.Fatal(err)
	}
	// Pre-approved package review fixture; the real review path is covered by
	// TestM2ExtensionPackageReviewEnableCheckAndRevoke.
	if _, err = f.inst.DB(ownership.Ledger).ExecContext(ctx, `UPDATE ledger_version_states SET state='approved',effective_review_id=? WHERE version_id=?`, ids.New(), v.VersionID); err != nil {
		t.Fatal(err)
	}
	servers := remoteServers(t, app, true)
	login := e.login()
	origin := "http://" + servers.Addresses.API
	work := t.TempDir()
	session := filepath.Join(work, "session.json")
	writeJSON(t, session, map[string]string{"schema": "lantai.client-session/v1", "origin": origin, "token": login.Token})
	registry := filepath.Join(work, "extensions.json")
	lantai := func(args ...string) (int, map[string]any) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		code := cli.Run(ctx, append(args, "--server", origin, "--allow-http", "--session-file", session), &stdout, &stderr)
		out := map[string]any{}
		raw := stdout.Bytes()
		if code != 0 {
			raw = stderr.Bytes()
		}
		_ = json.Unmarshal(raw, &out)
		return code, out
	}
	code, out := lantai("plugin", "import", "--input", writeTemp(t, map[string]any{"asset_id": v.AssetID, "version_id": v.VersionID}), "--idempotency-key", "import-cli")
	if code != 0 || out["package_digest"] != string(pkg.Digest) {
		t.Fatal(code, out)
	}
	// Server enablement is not local trust: install is refused until enabled.
	code, out = lantai("ext", "install", "--package", dir, "--registry", registry)
	if code == 0 {
		t.Fatal("installed a package that is not enabled", out)
	}
	m := app.ExtensionManager
	raw, _ := json.Marshal(map[string]any{})
	req := extensions.EnableRequest{ExtensionID: pkg.Manifest.ID, ExtensionVersion: pkg.Manifest.Version, PackageDigest: pkg.Digest, Target: "cli", ScopeKind: "user", ScopeID: login.Context.PrincipalID, Config: raw, ConfigRevision: 1, Trust: extensions.TrustUnenforced, Reason: "local tool"}
	action, err := m.EnableHumanAction(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	who, grant, child := f.grant(action)
	enabled, err := m.Enable(ctx, who, req, grant, child, f.id)
	if err != nil {
		t.Fatal(err)
	}
	if code, out = lantai("ext", "install", "--package", dir, "--registry", registry); code != 0 {
		t.Fatal(code, out)
	}
	// A same-named program on PATH is never consulted.
	bin := t.TempDir()
	exttest.CopyFile(t, exttest.Fixture(t), filepath.Join(bin, "copy"))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("LANTAI_SESSION_TOKEN", login.Token)
	if code, out = lantai("ext", "run", "org.example.other", "copy", "--registry", registry, "--output", t.TempDir()); code == 0 {
		t.Fatal("ran an uninstalled command", out)
	}
	input := filepath.Join(work, "note.txt")
	if err = os.WriteFile(input, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	output := t.TempDir()
	// Mode "env" makes the child fail if PATH, HOME or any token leaked in.
	code, out = lantai("ext", "run", pkg.Manifest.ID, "copy", "--registry", registry, "--input-file", input, "--arg", "env", "--output", output)
	if code != 0 || out["outcome"] != "completed" {
		t.Fatal(code, out)
	}
	if b, err := os.ReadFile(filepath.Join(output, "copies", "note.txt")); err != nil || string(b) != "HELLO" {
		t.Fatal(string(b), err)
	}
	// The same command through the controlled MCP projection.
	c, err := client.New(client.Config{BaseURL: origin, SessionToken: login.Token, AllowHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err = os.WriteFile(filepath.Join(work, "mcp.txt"), []byte("mcp"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(filepath.Join(work, "mcp-out"), 0o700); err != nil {
		t.Fatal(err)
	}
	server, err := mcpserver.New(ctx, mcpserver.Config{Client: c, Workspace: work, ExtensionsRegistry: registry})
	if err != nil {
		t.Fatal(err)
	}
	st, ct := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "integration", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	name := "ext_" + strings.ReplaceAll(pkg.Manifest.ID+".copy", ".", "_")
	tools, err := cs.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	var tool *mcp.Tool
	for _, x := range tools.Tools {
		if x.Name == name {
			tool = x
		}
	}
	if tool == nil || tool.Annotations == nil || tool.Annotations.ReadOnlyHint || tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint {
		t.Fatal("extension tool missing or mis-annotated", tool)
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: map[string]any{"inputs": []string{"mcp.txt"}, "args": []string{}, "output": "mcp-out"}})
	if err != nil || res.IsError {
		t.Fatal(res, err)
	}
	if b, err := os.ReadFile(filepath.Join(work, "mcp-out", "copies", "mcp.txt")); err != nil || string(b) != "MCP" {
		t.Fatal(string(b), err)
	}
	if res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: map[string]any{"inputs": []string{"../escape.txt"}, "args": []string{}, "output": "mcp-out"}}); err != nil || !res.IsError {
		t.Fatal("workspace escape accepted", res, err)
	}
	// A replaced entry is refused before anything runs.
	entry := filepath.Join(dir, filepath.FromSlash(exttest.EntryPath()))
	original, err := os.ReadFile(entry)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(entry, append(slices.Clone(original), 0), 0o755); err != nil {
		t.Fatal(err)
	}
	if code, out = lantai("ext", "run", pkg.Manifest.ID, "copy", "--registry", registry, "--output", t.TempDir()); code == 0 {
		t.Fatal("replaced entry ran", out)
	}
	if err = os.WriteFile(entry, original, 0o755); err != nil {
		t.Fatal(err)
	}
	// Revocation on the server is honored by the next local run.
	dreq := extensions.DisableRequest{EnablementID: enabled.Enablement.ID, Mode: "revoke", Reason: "synthetic"}
	daction, err := m.DisableHumanAction(ctx, dreq)
	if err != nil {
		t.Fatal(err)
	}
	who, grant, child = f.grant(daction)
	if _, err = m.Disable(ctx, who, dreq, grant, child, f.id); err != nil {
		t.Fatal(err)
	}
	code, out = lantai("ext", "run", pkg.Manifest.ID, "copy", "--registry", registry, "--output", t.TempDir())
	if code == 0 || !strings.Contains(string(mustJSON(out)), string(errcode.ExtensionActivationStale)) {
		t.Fatal("revoked command ran", code, out)
	}
}

func writeTemp(t *testing.T, v any) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "input.json")
	writeJSON(t, p, v)
	return p
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
