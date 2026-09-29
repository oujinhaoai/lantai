package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"testing"

	"github.com/oujinhaoai/lantai/internal/catalog"
	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	tc "github.com/oujinhaoai/lantai/internal/contract/tasks"
	"github.com/oujinhaoai/lantai/internal/events"
	"github.com/oujinhaoai/lantai/internal/extensions"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/ledger"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
	"github.com/oujinhaoai/lantai/internal/tasks"
	"github.com/oujinhaoai/lantai/internal/workflow"
)

// T06 remains an explicit fixture: it runs the built-in manifest validator and
// appends its results through the real ledger evidence path as a worker
// principal. Identity/TOTP, storage, catalog, ledger reviews and publication,
// provenance, events, T05 tasks/workflow and SQLite are real modules.
type jobFixture struct {
	e        *env
	registry *extensions.Registry
	source   *ledger.FileReviewSources
	checks   []string
	config   digest.Digest
	worker   authz.Context
	byOp     map[ids.ID]ids.ID
	results  map[ids.ID]workflow.JobResult
	inputs   map[string]ledger.ReviewEvidenceInput
	fail     bool
}

func (j *jobFixture) StartJob(ctx context.Context, _ authz.Context, req workflow.JobRequest) (workflow.JobRef, error) {
	if id, ok := j.byOp[req.OperationID]; ok {
		return workflow.JobRef{JobID: id}, nil
	}
	v, err := j.e.ledger.Version(ctx, req.Target.AssetID, req.Target.VersionID)
	if err != nil {
		return workflow.JobRef{}, err
	}
	raw, err := j.e.storage.ReadManifest(ctx, v)
	if err != nil {
		return workflow.JobRef{}, err
	}
	checked, err := j.registry.ValidateManifest(ctx, raw, req.Target, v.ManifestDigest)
	if err != nil {
		return workflow.JobRef{}, err
	}
	if j.fail {
		checked.Verdict, checked.Findings = "fail", []string{"synthetic_structure_error"}
	}
	res := workflow.JobResult{State: "succeeded"}
	for _, key := range j.checks {
		input := ledger.ReviewEvidenceInput{ProjectID: v.ProjectID, Ref: req.Target, ManifestDigest: v.ManifestDigest, Flow: req.Flow, Kind: "check_result", CheckKey: key, SchemaVersion: 1, ConfigDigest: j.config, Check: &checked}
		j.inputs[string(v.VersionID)+":"+key] = input
		accepted, err := j.source.AppendEvidence(ctx, j.worker, "job-"+string(req.OperationID)+"-"+key, input)
		if err != nil {
			return workflow.JobRef{}, err
		}
		var op ids.ID
		if err = j.e.inst.DB(ownership.Ledger).QueryRowContext(ctx, `SELECT operation_id FROM ledger_check_records WHERE evidence_id=?`, accepted.ID).Scan(&op); err != nil {
			return workflow.JobRef{}, err
		}
		res.Evidence = append(res.Evidence, workflow.JobEvidence{EvidenceID: accepted.ID, OperationID: op, Verdict: string(checked.Verdict)})
	}
	id := ids.New()
	j.byOp[req.OperationID], j.results[id] = id, res
	return workflow.JobRef{JobID: id}, nil
}
func (j *jobFixture) JobResult(_ context.Context, id ids.ID) (workflow.JobResult, error) {
	r, ok := j.results[id]
	if !ok {
		return r, errcode.New(errcode.NotFound, "")
	}
	return r, nil
}
func (j *jobFixture) VerifyCheck(_ context.Context, _ authz.Context, _ commit.Committed, actor ids.ID, in ledger.ReviewEvidenceInput, _ bool) error {
	a, _ := json.Marshal(in)
	b, _ := json.Marshal(j.inputs[string(in.Ref.VersionID)+":"+in.CheckKey])
	if actor != j.worker.PrincipalID || string(a) != string(b) {
		return errcode.New(errcode.PreconditionFailed, "job completion mismatch")
	}
	return nil
}

type flowEnv struct {
	*env
	tasks      *tasks.Service
	flows      *workflow.Service
	reviews    *ledger.Reviews
	source     *ledger.FileReviewSources
	log        *events.Store
	jobs       *jobFixture
	owner      authz.Context
	maker      authz.Context
	checker    authz.Context
	profile    catalog.VersionResult
	definition catalog.VersionResult
}

// taskAgent 登记带 task 范围的 Agent 主体并换取会话。
func (e *env) taskAgent(name string, role identity.Role) authz.Context {
	e.t.Helper()
	ctx := e.t.Context()
	e.sudo(&identity.RegisterPrincipal{Kind: authz.Agent, Name: name})
	principals, err := e.id.ListPrincipals(ctx, e.login().Context)
	if err != nil {
		e.t.Fatal(err)
	}
	var p identity.Principal
	for _, x := range principals {
		if x.Name == name {
			p = x
		}
	}
	res := e.sudo(&identity.IssueCredential{PrincipalID: p.ID, ExpectedRevision: p.Revision, Scopes: []identity.Scope{identity.ScopeRead, identity.ScopeIngest, identity.ScopeOrganize, identity.ScopeTask}})
	e.setRole(p.ID, role, true)
	s, err := e.id.ExchangeToken(ctx, res.Secret, identity.SessionRequest{Channel: identity.ChannelCLI})
	if err != nil {
		e.t.Fatal(err)
	}
	return s.Context
}

func (e *env) upload(who authz.Context, data []byte) ids.ID {
	e.t.Helper()
	ctx := e.t.Context()
	u, err := e.storage.CreateUpload(ctx, storage.CreateUploadRequest{Who: who, IdempotencyKey: e.key(), ProjectID: e.project.ProjectID, Files: []storage.FileSpec{{SHA256: shaOf(data), Size: int64(len(data))}}})
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err = e.storage.PutPart(ctx, storage.PartRequest{Who: who, UploadID: u.UploadID, SHA256: shaOf(data), PartNumber: 1, PartSHA256: shaOf(data), Size: int64(len(data)), Body: bytes.NewReader(data)}); err != nil {
		e.t.Fatal(err)
	}
	if _, err = e.storage.CompleteFile(ctx, who, u.UploadID, shaOf(data)); err != nil {
		e.t.Fatal(err)
	}
	return u.UploadID
}

func (e *env) config(who authz.Context, slug string, metadata map[string]any) catalog.VersionResult {
	e.t.Helper()
	data, _ := json.Marshal(metadata)
	v, err := e.catalog.CommitVersion(e.t.Context(), catalog.VersionRequest{Who: who, IdempotencyKey: e.key(), UploadID: e.upload(who, data), Slug: slug,
		Content: catalog.ContentInput{AssetType: manifest.TypeConfig, Rights: rightsOwned(), Files: []manifest.InputFile{{Path: "config.json", Role: "primary", SHA256: shaOf(data), Size: int64(len(data))}}, Metadata: metadata}})
	if err != nil {
		e.t.Fatal(err)
	}
	return v
}

// commitDoc 新建（asset 为空）或追加文档版本；task 非空时绑定当前执行轮次。
func (e *env) commitDoc(who authz.Context, slug string, asset, base ids.ID, data []byte, task *catalog.TaskBinding) (catalog.VersionResult, error) {
	req := catalog.VersionRequest{Who: who, IdempotencyKey: e.key(), UploadID: e.upload(who, data), AssetID: asset, BaseVersionID: base, Task: task,
		Content: catalog.ContentInput{AssetType: manifest.TypeDoc, Files: []manifest.InputFile{{Path: "content.txt", Role: "primary", SHA256: shaOf(data), Size: int64(len(data))}}}}
	if asset == "" {
		req.Slug, req.Content.Rights = slug, rightsOwned()
	}
	return e.catalog.CommitVersion(e.t.Context(), req)
}

func newFlowEnv(t *testing.T) *flowEnv {
	t.Helper()
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	ctx := t.Context()
	e.setRole(e.admin.Context.PrincipalID, identity.RoleOwner, true)
	f := &flowEnv{env: e, owner: e.login().Context}
	f.maker = e.taskAgent("maker@pc", identity.RoleContributor)
	f.checker = e.taskAgent("checker@pc", identity.RoleChecker)
	worker := e.taskAgent("worker@pc", identity.RoleChecker)
	registry, err := extensions.New(extensions.Deps{DB: e.inst.DB(ownership.Main), Gate: e.inst.Gate(), ReleaseDigest: digest.Of([]byte("flow test release"))})
	if err != nil {
		t.Fatal(err)
	}
	mctx, h, err := e.inst.Gate().Maintain(ctx, commands.ReasonStarting)
	if err != nil {
		t.Fatal(err)
	}
	err = registry.RegisterBuiltins(mctx)
	h.Release()
	e.inst.Gate().Open()
	if err != nil {
		t.Fatal(err)
	}
	producer, err := registry.Producer(ctx, "org.lantai.corecheck.manifest")
	if err != nil {
		t.Fatal(err)
	}
	config := digest.Of([]byte("flow checker configuration"))
	profile := ledger.AcceptanceProfile{Contract: "lantai.acceptance-profile/v1", ID: "flow-base", Revision: 1, AssetTypes: []manifest.AssetType{manifest.TypeDoc}, Purpose: authz.PurposeProduction, RequiredEvidence: []string{}, QARequired: true, DistinctActorRule: "maker_checker", WaivableChecks: []string{}, Defaults: ledger.ProfileDefaults{Publication: "auto"}}
	checks := []string{"integrity", "schema", "license_evidence", "purpose"}
	for _, key := range checks {
		profile.RequiredChecks = append(profile.RequiredChecks, ledger.CheckRequirement{Key: key, SchemaVersion: 1, AcceptedProcessors: []storage.Producer{producer}, ConfigDigest: config, Severity: "error"})
	}
	var metadata any
	raw, _ := json.Marshal(profile)
	_ = json.Unmarshal(raw, &metadata)
	f.profile = e.config(f.owner, "flow-profile", map[string]any{"acceptance_profile": metadata})
	// 预先批准的验收配置夹具；配置初始化路径另见 m2_profile_test。
	if _, err = e.inst.DB(ownership.Ledger).ExecContext(ctx, `UPDATE ledger_version_states SET state='approved',effective_review_id=? WHERE version_id=?`, ids.New(), f.profile.VersionID); err != nil {
		t.Fatal(err)
	}
	def, err := workflow.BuiltinDefinition("create")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(def)
	var defDoc any
	_ = json.Unmarshal(raw, &defDoc)
	f.definition = e.config(f.owner, "flows/create", map[string]any{"flow_definition": defDoc})

	gen := &ids.Generator{Clock: e.clk, Rand: rand.Reader}
	auth := authority{Service: e.id, epochs: e.inst}
	f.tasks, err = tasks.New(tasks.Deps{DB: e.inst.DB(ownership.Runtime), Gate: e.inst.Gate(), Authority: auth, Policies: e.id, Members: e.id, Resources: e.ledger, Discussions: e.ledger, Clock: e.clk, IDs: gen, InstanceID: e.inst.InstanceID()})
	if err != nil {
		t.Fatal(err)
	}
	f.log = events.New(e.inst.DB(ownership.Events), e.clk, e.inst.Gate())
	f.jobs = &jobFixture{e: e, registry: registry, checks: checks, config: config, worker: worker, byOp: map[ids.ID]ids.ID{}, results: map[ids.ID]workflow.JobResult{}, inputs: map[string]ledger.ReviewEvidenceInput{}}
	f.flows, err = workflow.New(workflow.Deps{DB: e.inst.DB(ownership.Runtime), Gate: e.inst.Gate(), Authority: auth, Tasks: f.tasks, Ledger: e.ledger, Manifests: e.storage, Events: f.log, Jobs: f.jobs, Checks: f.jobs, Clock: e.clk, IDs: gen, InstanceID: e.inst.InstanceID()})
	if err != nil {
		t.Fatal(err)
	}
	f.source, err = e.ledger.NewFileReviewSources(e.storage, e.rights, f.flows.ReviewExecution(), registry, e.rights)
	if err != nil {
		t.Fatal(err)
	}
	f.jobs.source = f.source
	f.reviews, err = e.ledger.NewReviews(f.source, e.id)
	if err != nil {
		t.Fatal(err)
	}
	f.flows.SetReviews(f.reviews)
	f.tasks.SetAuthorities(tasks.LedgerAuthorities{Reviews: f.reviews, Evidence: f.source})
	e.ledger.SetCheckoutGuard(f.tasks)
	return f
}

// sync 收录各库 outbox 并推进流程消费者；dispatch 以协调者身份派发待发命令。
func (f *flowEnv) sync() {
	f.t.Helper()
	f.relay(f.log)
	if _, err := f.flows.CatchUp(f.t.Context(), 1000); err != nil {
		f.t.Fatal(err)
	}
}
func (f *flowEnv) dispatch() {
	f.t.Helper()
	if _, err := f.flows.Dispatch(f.t.Context(), f.owner, 100); err != nil {
		f.t.Fatal(err)
	}
	f.sync()
}
func (f *flowEnv) flow(id ids.ID) workflow.FlowView {
	f.t.Helper()
	v, err := f.flows.Flow(f.t.Context(), f.owner, id)
	if err != nil {
		f.t.Fatal(err)
	}
	return v
}
func (f *flowEnv) latest(v workflow.FlowView, key string) workflow.StepView {
	f.t.Helper()
	var out *workflow.StepView
	for i := range v.Steps {
		if v.Steps[i].StepRun.StepKey == key && (out == nil || v.Steps[i].StepRun.Round > out.StepRun.Round) {
			out = &v.Steps[i]
		}
	}
	if out == nil {
		f.t.Fatalf("no %s step in %+v", key, v.Steps)
	}
	return *out
}
func (f *flowEnv) task(id ids.ID) tasks.View {
	f.t.Helper()
	v, err := f.tasks.Task(f.t.Context(), f.owner, id)
	if err != nil {
		f.t.Fatal(err)
	}
	return v
}
func (f *flowEnv) claim(who authz.Context, id ids.ID) tasks.Attempt {
	f.t.Helper()
	v := f.task(id)
	r, err := f.tasks.Claim(f.t.Context(), who, f.key(), tasks.ClaimRequest{ProjectID: f.project.ProjectID, TaskID: id, SeatID: v.Seat.ID, ExpectedRevision: v.Seat.Revision})
	if err != nil {
		f.t.Fatal(err)
	}
	return *r.Attempt
}
func attemptRef(f *flowEnv, id ids.ID, a tasks.Attempt) tasks.AttemptRef {
	return tasks.AttemptRef{ProjectID: f.project.ProjectID, TaskID: id, AttemptID: a.ID, ExpectedRevision: a.Revision, Fence: a.Fence}
}

// qa 以独立质检者完成一轮质检：领取、登记报告证据、交付并按证据完成任务。
func (f *flowEnv) qa(flow ids.ID, verdict string) {
	f.t.Helper()
	ctx := f.t.Context()
	v := f.flow(flow)
	qaTask := f.latest(v, "qa").StepRun.TaskIDs[0]
	a := f.claim(f.checker, qaTask)
	subject := v.Meta.Subject
	in := ledger.ReviewEvidenceInput{ProjectID: f.project.ProjectID, Ref: subject.Ref, ManifestDigest: f.versionOf(subject.Ref).ManifestDigest, Flow: subject.Flow, Kind: "qa_report", QA: &ledger.QAReport{Tool: "synthetic-viewer", ToolVersion: "1.0", Observations: "silhouette and naming inspected", Verdict: verdict}}
	evidence, err := f.source.AppendEvidence(ctx, f.checker, f.key(), in)
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err = f.tasks.Submit(ctx, f.checker, f.key(), tasks.SubmitRequest{AttemptRef: attemptRef(f, qaTask, a)}); err != nil {
		f.t.Fatal(err)
	}
	var op ids.ID
	if err = f.inst.DB(ownership.Ledger).QueryRowContext(ctx, `SELECT operation_id FROM ledger_check_records WHERE evidence_id=?`, evidence.ID).Scan(&op); err != nil {
		f.t.Fatal(err)
	}
	t := f.task(qaTask)
	if _, err = f.tasks.Complete(ctx, f.checker, f.key(), tasks.CompleteRequest{TaskRef: tasks.TaskRef{ProjectID: f.project.ProjectID, TaskID: qaTask, ExpectedRevision: t.Task.Revision}, AuthorityOperationID: op}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *flowEnv) versionOf(r ids.PermanentRef) commit.Committed {
	f.t.Helper()
	v, err := f.ledger.Version(f.t.Context(), r.AssetID, r.VersionID)
	if err != nil {
		f.t.Fatal(err)
	}
	return v
}

// decide 由人经 Challenge → TOTP → HumanGrant 记录审定结论。
func (f *flowEnv) decide(target ids.ID, verdict string) {
	f.t.Helper()
	ctx := f.t.Context()
	who := f.login().Context
	t, err := f.reviews.Target(ctx, who, target)
	if err != nil {
		f.t.Fatal(err)
	}
	decision := ledger.ReviewDecision{Action: identity.ActRecordReview, TargetID: t.ID, ExpectedRevision: t.Revision, Verdict: verdict, Reason: "synthetic human decision: " + verdict, Waivers: []ledger.ReviewWaiver{}}
	action, err := f.reviews.HumanAction(ctx, decision)
	if err != nil {
		f.t.Fatal(err)
	}
	ch, err := f.id.CreateDomainChallenge(ctx, who, []identity.HumanAction{action}, f.reviews)
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
	if _, err = f.reviews.Record(ctx, who, decision, g.GrantID, items[0].OperationID, f.id); err != nil {
		f.t.Fatal(err)
	}
}

func TestM2FlowCreateReworkApprovePublish(t *testing.T) {
	f := newFlowEnv(t)
	ctx := t.Context()
	started, err := f.flows.Start(ctx, f.owner, f.key(), workflow.StartRequest{ProjectID: f.project.ProjectID, DefinitionRef: f.definition.Ref, ProfileRef: f.profile.Ref, Title: "Fourth sister hero sheet",
		AcceptanceCriteria: []string{"front view", "matches palette"}, ExpectedOutputs: []tc.OutputRequirement{{Slug: "hero", AssetType: "doc", CandidateCount: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	v := f.flow(started.FlowID)
	if v.Flow.Definition.Ref != f.definition.Ref || len(v.Commands) != 1 || v.Commands[0].CommandType != "tasks.create" || v.Flow.State != "running" {
		t.Fatalf("flow must pin its definition and persist the first command: %+v", v)
	}
	// 重复派发不重复建任务。
	f.dispatch()
	f.dispatch()
	v = f.flow(started.FlowID)
	produce := f.latest(v, "produce")
	if produce.StepRun.State != "running" || len(produce.StepRun.TaskIDs) != 1 || v.Flow.State != "waiting" {
		t.Fatalf("%+v", produce)
	}
	taskID := produce.StepRun.TaskIDs[0]
	if list, _ := f.tasks.List(ctx, f.owner, f.project.ProjectID, "", 50, true); len(list) != 1 {
		t.Fatal("exactly one production task", list)
	}
	a1 := f.claim(f.maker, taskID)
	v1, err := f.commitDoc(f.maker, "hero", "", "", []byte("hero sheet round one"), &catalog.TaskBinding{TaskID: taskID, AttemptID: a1.ID, LeaseFence: a1.Fence.LeaseFence})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.tasks.Submit(ctx, f.maker, f.key(), tasks.SubmitRequest{AttemptRef: attemptRef(f, taskID, a1), OutputRefs: []ids.PermanentRef{v1.Ref}}); err != nil {
		t.Fatal(err)
	}
	f.sync()
	v = f.flow(started.FlowID)
	if v.Meta.Subject == nil || v.Meta.Subject.Ref != v1.Ref || f.latest(v, "produce").StepRun.State != "waiting" || f.latest(v, "check").StepRun.State != "ready" {
		t.Fatalf("delivery starts the check step: %+v", v.Meta)
	}
	f.dispatch()
	if n, err := f.flows.SyncJobs(ctx, f.owner, 10); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	f.dispatch()
	v = f.flow(started.FlowID)
	if f.latest(v, "check").StepRun.State != "completed" || len(v.Meta.CheckEvidence) != 4 || f.latest(v, "qa").StepRun.State != "running" {
		t.Fatalf("%+v", v.Steps)
	}
	// 制作者即使兼任质检角色也不能质检自己的交付。
	qaTask := f.latest(v, "qa").StepRun.TaskIDs[0]
	if qv := f.task(qaTask); qv.Meta.Subject == nil || qv.Meta.Subject.Flow.AttemptID != a1.ID {
		t.Fatal(qv.Meta)
	}
	f.qa(started.FlowID, "pass")
	f.sync()
	f.dispatch()
	v = f.flow(started.FlowID)
	review := f.latest(v, "review")
	if review.StepRun.State != "waiting" || v.Meta.ReviewTargetID == "" {
		t.Fatalf("flow submits the exact delivery for review: %+v", review)
	}
	firstTarget := v.Meta.ReviewTargetID
	f.decide(firstTarget, "return")
	f.sync()
	f.dispatch()
	v = f.flow(started.FlowID)
	if v.Meta.ProductionRound != 2 || v.Meta.Reworks != 1 || len(v.Meta.Rounds) != 1 || v.Meta.Rounds[0].Outcome != "review_returned" {
		t.Fatalf("return opens a new round and keeps history: %+v", v.Meta)
	}
	rounds := 0
	for _, s := range v.Steps {
		if s.StepRun.StepKey == "produce" {
			rounds++
		}
	}
	if rounds != 2 || f.task(taskID).Task.State != "rework" {
		t.Fatal("rework keeps the old produce round", rounds)
	}
	// 旧轮次的交付不能再被审定；旧 Attempt 不能再提交版本。
	if err = f.flows.ReviewExecution().Task(ctx, f.owner, f.versionOf(v1.Ref), ledger.ReviewFlow{TaskID: taskID, AttemptID: a1.ID, Round: 1, Fence: a1.Fence.LeaseFence}, "review"); errcode.CodeOf(err) != errcode.ReviewTargetStale {
		t.Fatal("stale review target accepted", err)
	}
	a2 := f.claim(f.maker, taskID)
	if tv := f.task(taskID); len(tv.Checkouts) != 1 || tv.Checkouts[0].State != "active" || tv.Checkouts[0].Mode != "exclusive" || tv.Checkouts[0].BaseVersionID != v1.VersionID {
		t.Fatal("rework round checks out the delivered asset", tv.Checkouts)
	}
	if _, err = f.commitDoc(f.owner, "", v1.AssetID, v1.VersionID, []byte("unbound edit"), nil); errcode.CodeOf(err) != errcode.AssetCheckedOut {
		t.Fatal("exclusive checkout must reject other writers at the ledger", err)
	}
	if _, err = f.commitDoc(f.maker, "", v1.AssetID, v1.VersionID, []byte("stale attempt edit"), &catalog.TaskBinding{TaskID: taskID, AttemptID: a1.ID, LeaseFence: a1.Fence.LeaseFence}); errcode.CodeOf(err) != errcode.LeaseStale {
		t.Fatal("old attempt wrote a version", err)
	}
	v2, err := f.commitDoc(f.maker, "", v1.AssetID, v1.VersionID, []byte("hero sheet round two"), &catalog.TaskBinding{TaskID: taskID, AttemptID: a2.ID, LeaseFence: a2.Fence.LeaseFence})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.tasks.Submit(ctx, f.maker, f.key(), tasks.SubmitRequest{AttemptRef: attemptRef(f, taskID, a2), OutputRefs: []ids.PermanentRef{v2.Ref}}); err != nil {
		t.Fatal(err)
	}
	f.sync()
	f.dispatch()
	if _, err = f.flows.SyncJobs(ctx, f.owner, 10); err != nil {
		t.Fatal(err)
	}
	f.dispatch()
	f.qa(started.FlowID, "pass")
	f.sync()
	f.dispatch()
	v = f.flow(started.FlowID)
	if v.Meta.ReviewTargetID == firstTarget || f.latest(v, "review").Meta.ProductionRound != 2 {
		t.Fatal("new round needs a new review target", v.Meta)
	}
	f.decide(v.Meta.ReviewTargetID, "approve")
	// 仅 approved 时旧发布指针不变；发布由流程持久请求执行。
	if a, err := f.ledger.AssetControl(ctx, v1.AssetID); err != nil || a.PublicationState == "published" {
		t.Fatal("approval is not publication", a, err)
	}
	f.sync()
	f.dispatch()
	f.sync()
	v = f.flow(started.FlowID)
	if v.Flow.State != "completed" || f.latest(v, "publish").StepRun.State != "completed" || f.latest(v, "produce").StepRun.State != "completed" {
		t.Fatalf("flow completes only after publication: %+v %+v", v.Flow, v.Steps)
	}
	a, err := f.ledger.AssetControl(ctx, v1.AssetID)
	if err != nil || a.PublicationState != "published" || a.PublishedVersionID != v2.VersionID {
		t.Fatal(a, err)
	}
	tv := f.task(taskID)
	if tv.Task.State != "done" || tv.Meta.Round != 2 || len(tv.Meta.History) != 2 || tv.Checkouts[0].State != "released" {
		t.Fatalf("%+v", tv.Meta)
	}
	attempts, _ := f.tasks.Attempts(ctx, f.owner, taskID)
	if len(attempts) != 2 || attempts[0].State != "submitted" || attempts[1].Fence.LeaseFence != 2 {
		t.Fatal(attempts)
	}
	// 再次推进和派发不改变已完成的流程。
	before := v.Flow.Revision
	f.dispatch()
	if again := f.flow(started.FlowID); again.Flow.Revision != before {
		t.Fatal("completed flow changed on replay", again.Flow.Revision, before)
	}
	// 归档需要 T05 空闲：流程结束且任务完成后才放行。
	if err = f.flows.RequireIdle(ctx, f.project.ProjectID, []ids.ID{v1.AssetID}); err != nil {
		t.Fatal(err)
	}
}
