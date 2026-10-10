package integration

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	ax "github.com/oujinhaoai/lantai/internal/agent_execution"
	"github.com/oujinhaoai/lantai/internal/application"
	"github.com/oujinhaoai/lantai/internal/catalog"
	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/commands"
	ae "github.com/oujinhaoai/lantai/internal/contract/agentexec"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/event"
	"github.com/oujinhaoai/lantai/internal/contract/execution"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	tc "github.com/oujinhaoai/lantai/internal/contract/tasks"
	"github.com/oujinhaoai/lantai/internal/events"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/identity/masterkey"
	"github.com/oujinhaoai/lantai/internal/identity/totp"
	"github.com/oujinhaoai/lantai/internal/jobs"
	"github.com/oujinhaoai/lantai/internal/ledger"
	"github.com/oujinhaoai/lantai/internal/node"
	"github.com/oujinhaoai/lantai/internal/operations"
	"github.com/oujinhaoai/lantai/internal/platform/sqlite"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/tasks"
	"github.com/oujinhaoai/lantai/internal/workflow"
)

// HTTP/CLI business commands run against a separate, unmodified lantai serve.
// Legal bootstrap and event injection use assembled domain owners while serve
// is stopped. SQL is observation only. Reordered semantic copies deliberately
// get new event IDs: this tests the workflow's live-fact checks as well as exact
// event-ID deduplication. This is a process crash rehearsal, not a power-loss test.
type businessRecovery struct {
	*reviewRemote
	home        string
	process     *exec.Cmd
	done        chan error
	starts      int
	records     []commands.OutboxRecord
	c           identity.IssuedSession
	old         catalog.VersionResult
	injected    map[ids.ID]bool
	cAttempt    tasks.Attempt
	cRun        ax.Record
	evidenceDir string
	lastProof   int64
}

func newBusinessRecovery(t *testing.T) *businessRecovery {
	f := newAppFlow(t)
	bin := reviewAcceptanceBinary(t)
	dir := os.Getenv("LANTAI_BUSINESS_RECOVERY_EVIDENCE_DIR")
	if dir == "" {
		dir = t.TempDir()
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	log, err := os.OpenFile(filepath.Join(dir, filepath.Base(t.Name())+".jsonl"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	r := &reviewRemote{f: f, bin: bin, log: log}
	b := &businessRecovery{reviewRemote: r, home: f.inst.Layout().Home, injected: map[ids.ID]bool{}, evidenceDir: dir}
	t.Cleanup(func() {
		b.stop()
		if err := log.Close(); err != nil {
			t.Error(err)
		}
	})
	testExe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	r.record(map[string]any{"test_binary_sha256": reviewBinaryDigest(t, testExe), "kind": "fixture", "binary_sha256": reviewBinaryDigest(t, bin), "bootstrap": "legal domain APIs; no SQL writes", "business": "REST/CLI to independent lantai serve", "events": "internal Store.Collect, exact duplicates and reverse-order semantic copies", "data": "temporary synthetic instance", "clock": "bootstrap fake; serve system wall clock"})
	producer, err := f.app.Extensions.Producer(t.Context(), "org.lantai.corecheck.manifest")
	if err != nil {
		t.Fatal(err)
	}
	base := f.approvedProfile("recovery/bootstrap", profileDocument("bootstrap", producer), ids.PermanentRef{})
	p := profileDocument("recovery-qa", producer)
	p.QARequired = true
	serveProducer := producer
	serveProducer.CoreReleaseDigest = reviewBinaryDigest(t, bin)
	for i := range p.RequiredChecks {
		p.RequiredChecks[i].AcceptedProcessors = append(p.RequiredChecks[i].AcceptedProcessors, serveProducer)
	}
	r.profile = f.approvedProfile("recovery/profile", p, base.Ref)
	r.definition = f.definition("recovery/flow", "org.lantai.corecheck.manifest")
	// Also retain legal external package/enablement/probe history for restore.
	pkg, rec := f.approvedPackage(base, "recovery/plugin", "0.1.0")
	enabled := f.enablePackage(pkg, `{"mode":"pass"}`, 1)
	f.sudo(&identity.SetPolicy{ProjectID: f.project.ProjectID, Key: "plugins.allowed", Value: json.RawMessage(`["org.example.business"]`)})
	r.record(map[string]any{"kind": "prerequisites", "profile": r.profile, "definition": r.definition, "package": rec, "enablement": enabled})
	// Old release on the same asset proves approval cannot move its pointer.
	old, flow := f.candidate("recovery/output", manifest.TypeDoc, map[string][]byte{"content.txt": []byte("synthetic old published bytes")}, nil)
	job := f.checkCandidate(old, flow, "org.lantai.corecheck.manifest")
	target := f.submitChecked(old, flow, base.Ref, job, false)
	f.decide(target.ID, "approve")
	state, err := f.ledger.VersionControl(t.Context(), old.VersionID)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := f.reviews.Publish(t.Context(), f.login().Context, f.key(), ledger.PublishRequest{ProjectID: f.project.ProjectID, VersionID: old.VersionID, ReviewID: state.EffectiveReviewID, Action: "publish", Reason: "synthetic baseline release"})
	if err != nil {
		t.Fatal(err)
	}
	r.record(map[string]any{"kind": "old-publication", "version": old, "publication": pub})
	b.old = old // private helper slot stores the old business release
	// Fresh ordinary credentials are minted near wall time through HumanGrant.
	// This avoids resurrecting the deliberately old bootstrap sessions at serve.
	f.clk.Set(time.Now().Add(-time.Hour))
	f.setRole(f.maker.PrincipalID, identity.RoleChecker, true)
	mint := func(id ids.ID, scopes []identity.Scope) identity.IssuedSession {
		p, e := f.id.GetPrincipal(t.Context(), f.login().Context, id)
		if e != nil {
			t.Fatal(e)
		}
		cred := f.sudo(&identity.IssueCredential{PrincipalID: id, ExpectedRevision: p.Revision, Scopes: scopes})
		s, e := f.id.ExchangeToken(t.Context(), cred.Secret, identity.SessionRequest{Channel: identity.ChannelCLI, Purpose: "production"})
		if e != nil {
			t.Fatal(e)
		}
		return s
	}
	scopes := []identity.Scope{identity.ScopeRead, identity.ScopeIngest, identity.ScopeOrganize, identity.ScopeTask}
	r.maker = mint(f.maker.PrincipalID, scopes)
	r.checker = mint(f.checker.PrincipalID, scopes)
	r.worker = mint(f.worker.PrincipalID, []identity.Scope{identity.ScopeWorker})
	c := f.taskAgent("downstream-c@synthetic", identity.RoleContributor)
	b.c = mint(c.PrincipalID, scopes)
	r.owner = f.login()
	f.owner = r.owner.Context
	r.record(map[string]any{"kind": "actors", "A": r.maker.Context, "B": r.checker.Context, "C": b.c.Context, "human": r.owner.Context})
	if err = f.app.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	b.startServe()
	return b
}

func (b *businessRecovery) startServe() {
	t := b.f.t
	b.starts++
	cmd := exec.CommandContext(t.Context(), b.bin, "serve", "-home", b.home, "-merged", "-api-listen", "127.0.0.1:0", "-operations-listen", "127.0.0.1:0")
	cmd.Env = consistencyEnv()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := os.OpenFile(filepath.Join(b.evidenceDir, fmt.Sprintf("serve-%02d.jsonl", b.starts)), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = stderr
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	b.process = cmd
	b.done = make(chan error, 1)
	go func() { b.done <- cmd.Wait() }()
	ready := make(chan []byte, 1)
	go func() {
		s := bufio.NewScanner(stdout)
		if s.Scan() {
			ready <- append([]byte(nil), s.Bytes()...)
		} else {
			ready <- nil
		}
	}()
	select {
	case line := <-ready:
		var v struct {
			Ready     bool `json:"ready"`
			Listeners struct {
				API string `json:"api"`
			} `json:"listeners"`
		}
		if err = json.Unmarshal(line, &v); err != nil || !v.Ready {
			data, _ := os.ReadFile(stderr.Name())
			t.Fatalf("serve readiness: %s %v %s", line, err, data)
		}
		b.origin = "http://" + v.Listeners.API
		b.record(map[string]any{"kind": "serve-start", "number": b.starts, "pid": cmd.Process.Pid, "ready": json.RawMessage(line)})
	case <-time.After(30 * time.Second):
		t.Fatal("serve readiness deadline")
	}
	t.Cleanup(func() { _ = stderr.Close() })
}
func (b *businessRecovery) stop() {
	if b.process == nil {
		return
	}
	pid := b.process.Process.Pid
	if err := b.process.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		b.f.t.Error(err)
	}
	select {
	case err := <-b.done:
		b.record(map[string]any{"kind": "serve-exit", "pid": pid, "error": fmt.Sprint(err), "signal": "kill"})
	case <-time.After(10 * time.Second):
		b.f.t.Error("own serve did not exit")
	}
	b.process = nil
}
func (b *businessRecovery) open() *application.App {
	a, err := application.Open(b.f.t.Context(), application.Options{Instance: operations.Options{Home: b.home}, Identity: identity.Config{Password: fastPassword}, Storage: storage.Config{MinFreeBytes: 1 << 20}})
	if err != nil {
		b.f.t.Fatal(err)
	}
	f := b.f
	f.app = a
	f.inst = a.Instance
	f.id = a.Identity
	f.ledger = a.Ledger
	f.catalog = a.Catalog
	f.storage = a.Storage
	f.rights = a.Rights
	f.flows = a.Flows
	f.tasks = a.Tasks
	f.reviews = a.Reviews
	f.log = a.Events
	f.source = a.Evidence
	return a
}
func businessRows(t *testing.T, db *sql.DB, q string, args ...any) [][]any {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), q, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	out := [][]any{}
	for rows.Next() {
		values := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range ptrs {
			ptrs[i] = &values[i]
		}
		if err = rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		for i, v := range values {
			if x, ok := v.([]byte); ok {
				values[i] = string(x)
			}
		}
		out = append(out, values)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
func (b *businessRecovery) facts(a *application.App) map[string][][]any {
	t := b.f.t
	out := map[string][][]any{}
	tables := map[ownership.Database][]string{
		ownership.Ledger:  {"ledger_reviews", "ledger_review_targets", "ledger_check_records", "ledger_publications", "ledger_version_states", "ledger_asset_controls", "ledger_messages", "command_receipts"},
		ownership.Runtime: {"workflow_flows", "workflow_step_runs", "workflow_commands", "tasks_tasks", "tasks_attempts", "jobs_jobs", "jobs_attempts", "agent_execution_runs", "agent_execution_history", "agent_execution_admissions", "extensions_activations", "extensions_invocations", "extensions_breakers", "extensions_breaker_faults"},
		ownership.Main:    {"extensions_packages", "extensions_enablements", "extensions_enablement_history"},
	}
	for db, ts := range tables {
		for _, table := range ts {
			out[string(db)+"."+table] = businessRows(t, a.Instance.DB(db), "SELECT * FROM "+table+" ORDER BY 1,2")
		}
	}
	return out
}

// point is a deterministic barrier: mutation already returned; no flow dispatch
// is automatic. Read-only SQL proves pending intent before killing this PID.
func (b *businessRecovery) point(label string, flow ids.ID, requirePending bool) {
	t := b.f.t
	db, err := sqlite.Open(t.Context(), filepath.Join(b.home, "db", "runtime.db"), sqlite.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	pending := businessRows(t, db, "SELECT operation_id,command_type,payload,status,attempts FROM workflow_commands WHERE flow_id=? AND status='pending' ORDER BY operation_id", flow)
	_ = db.Close()
	if requirePending && len(pending) == 0 {
		t.Fatal("no durable pending intent at crash barrier")
	}
	b.record(map[string]any{"kind": "crash-barrier", "point": label, "flow": flow, "pid": b.process.Process.Pid, "pending": pending, "observation": "read-only connection before kill"})
	b.stop()
	a := b.open()
	if err = a.Sync(t.Context()); err != nil {
		t.Fatal(err)
	}
	b.catch(a)
	before := b.facts(a)
	page := events.Page{}
	for cursor := int64(0); ; {
		next, e := a.Events.Read(t.Context(), cursor, 1000)
		if e != nil {
			t.Fatal(e)
		}
		page.Entries = append(page.Entries, next.Entries...)
		if len(next.Entries) < 1000 {
			break
		}
		cursor = next.HighWater
	}
	records := []commands.OutboxRecord{}
	for i := len(page.Entries) - 1; i >= 0; i-- {
		e := page.Entries[i]
		if !b.injected[e.Envelope.EventID] && (strings.HasPrefix(e.Envelope.EventType, "task.") || strings.HasPrefix(e.Envelope.EventType, "review.") || e.Envelope.EventType == "publication.changed") {
			records = append(records, commands.OutboxRecord{EventID: e.Envelope.EventID, OperationID: e.Envelope.OperationID, Envelope: e.Canonical})
		}
	}
	n, err := a.Events.Collect(t.Context(), records)
	if err != nil || n != 0 {
		t.Fatalf("duplicate collection %d %v", n, err)
	}
	clones := []commands.OutboxRecord{}
	for _, r := range records {
		e, err := eventCopy(r)
		if err != nil {
			t.Fatal(err)
		}
		clones = append(clones, e)
		b.injected[e.EventID] = true
	}
	n, err = a.Events.Collect(t.Context(), clones)
	if err != nil || n != len(clones) {
		t.Fatal(n, err)
	}
	b.catch(a)
	after := b.facts(a)
	if !reflect.DeepEqual(before, after) {
		b.record(map[string]any{"kind": "replay-mismatch", "point": label, "before": before, "after": after})
		t.Fatal("late reverse-order facts changed business state at " + label)
	}
	// Exact-ID duplicates after acceptance must still be a no-op.
	if n, err = a.Events.Collect(t.Context(), clones); err != nil || n != 0 {
		t.Fatal(n, err)
	}
	b.records = append(b.records, records...)
	var view workflow.FlowView
	view, err = a.Flows.Flow(t.Context(), b.owner.Context, flow)
	if err != nil {
		t.Fatal(err)
	}
	b.record(map[string]any{"kind": "replay-point", "point": label, "exact_duplicates": len(records), "reverse_semantic_copies": clones, "flow": view, "facts": after, "result": "pass"})
	if err = a.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	b.startServe()
}
func eventCopy(r commands.OutboxRecord) (commands.OutboxRecord, error) {
	e, err := event.Parse(r.Envelope)
	if err != nil {
		return commands.OutboxRecord{}, err
	}
	original := e.EventID
	e.EventID = ids.New()
	e.CausationID = original
	raw, err := e.Canonical()
	return commands.OutboxRecord{EventID: e.EventID, OperationID: e.OperationID, Envelope: raw}, err
}
func (b *businessRecovery) catch(a *application.App) {
	t := b.f.t
	for i := 0; i < 30; i++ {
		n, err := a.Flows.CatchUp(t.Context(), 1000)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
	}
	if _, err := a.Flows.SyncJobs(t.Context(), b.owner.Context, 100); err != nil {
		t.Fatal(err)
	}
}
func (b *businessRecovery) pump(flow ids.ID, predicate func(workflow.FlowView) bool) workflow.FlowView {
	t := b.f.t
	deadline := time.Now().Add(20 * time.Second)
	for {
		b.ok(b.owner, "POST", "flows/dispatch", map[string]int{"limit": 100}, nil)
		v := b.view(flow)
		if predicate(v) {
			return v
		}
		if time.Now().After(deadline) {
			t.Fatalf("flow did not reach required state: %+v", v)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
func (b *businessRecovery) human(d ledger.ReviewDecision) (commands.Receipt, map[string]any) {
	t := b.f.t
	// Real serve enforces TOTP replay. Wait for a new counter, never disable proof.
	for totp.Counter(time.Now()) <= b.lastProof {
		select {
		case <-t.Context().Done():
			t.Fatal("proof wait cancelled")
		case <-time.After(100 * time.Millisecond):
		}
	}
	p := b.prepare([]ledger.ReviewDecision{d})
	counter := totp.Counter(time.Now())
	var g identity.Grant
	b.ok(b.owner, "POST", "identity/challenges/"+string(p.Challenge.ChallengeID)+"/verify", map[string]string{"code": totp.Code(b.f.secret, counter)}, &g)
	b.lastProof = counter
	var items []identity.HumanGrantItem
	b.ok(b.owner, "GET", "human/grants/"+string(g.GrantID), nil, &items)
	body := executeBody(g, items[0])
	var receipt commands.Receipt
	b.cli(b.owner, "human", "execute", body, &receipt, 0)
	var replay commands.Receipt
	b.ok(b.owner, "POST", "human/execute", body, &replay)
	if !reflect.DeepEqual(receipt, replay) {
		t.Fatal("human receipt changed on replay")
	}
	return receipt, body
}

func (b *businessRecovery) round(flow ids.ID, previous catalog.VersionResult, round int) (catalog.VersionResult, ledger.ReviewTarget, tasks.Attempt, []byte) {
	t := b.f.t
	view := b.view(flow)
	task := b.f.latest(view, "produce").StepRun.TaskIDs[0]
	attempt := b.claim(b.maker, task)
	run := b.manual(b.maker, attempt)
	dir := t.TempDir()
	payload := []byte(fmt.Sprintf("synthetic business recovery round %d; exact bytes", round))
	if err := os.WriteFile(filepath.Join(dir, "content.txt"), payload, 0600); err != nil {
		t.Fatal(err)
	}
	bind, _ := json.Marshal(bindTo(task, attempt))
	push := client.PushInput{ProjectID: string(b.f.project.ProjectID), AssetID: string(previous.AssetID), BaseVersionID: string(previous.VersionID), Task: bind, Content: client.ContentInput{AssetType: "doc", Files: []client.InputFile{{Path: "content.txt", Role: "source"}}, Rights: json.RawMessage(`{"usage":"production","license":"LicenseRef-Owned","sensitivity":"normal"}`)}}
	input := filepath.Join(dir, "push.json")
	writeJSON(t, input, push)
	session := filepath.Join(dir, "session.json")
	writeJSON(t, session, map[string]string{"schema": "lantai.client-session/v1", "origin": b.origin, "token": b.maker.Token})
	args := []string{"push", "--server", b.origin, "--allow-http", "--session-file", session, "--input", input, "--directory", dir, "--state", filepath.Join(dir, "push-state.json")}
	runPush := func() catalog.VersionResult {
		cmd := exec.CommandContext(t.Context(), b.bin, args...)
		cmd.Env = consistencyEnv()
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		b.record(map[string]any{"kind": "cli-push", "round": round, "request": push, "payload_sha256": digest.Of(payload), "stdout": stdout.String(), "stderr": stderr.String(), "error": fmt.Sprint(err)})
		if err != nil {
			t.Fatal(err, stderr.String())
		}
		var v catalog.VersionResult
		if err = json.Unmarshal(stdout.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	version := runPush()
	if replay := runPush(); !reflect.DeepEqual(version, replay) {
		t.Fatal("push replay changed version")
	}
	candidate := ae.ArtifactCandidate{Contract: "lantai.artifact-candidate/v1", ID: ids.New(), TaskRunID: run.Run.ID, AttemptID: attempt.ID, ManifestDigest: version.ManifestDigest, Files: []ae.ArtifactFile{{Path: "content.txt", SHA256: shaOf(payload), Size: int64(len(payload))}}, Purpose: "synthetic business recovery", ProvenanceRefs: []ids.ID{}, LicenseEvidenceRefs: []ids.ID{}, ValidationState: "pending"}
	b.ok(b.maker, "POST", "task-runs/candidate", ax.CandidateRequest{Mutation: mut(run, attempt.Fence), Candidate: candidate, Ref: version.Ref}, &run)
	b.ok(b.maker, "POST", "task-runs/seal", ax.SealRequest{Mutation: mut(run, attempt.Fence), Outcome: execution.RunExecutionSucceeded, Stopped: true, CandidateIDs: []ids.ID{candidate.ID}, Limitations: []string{"synthetic manual execution; human review remains separate"}}, &run)
	b.ok(b.maker, "POST", "tasks/submit", tasks.SubmitRequest{AttemptRef: attemptRef(b.f.flowEnv, task, attempt), OutputRefs: []ids.PermanentRef{version.Ref}}, nil)
	b.point(fmt.Sprintf("round-%d-production-delivery", round), flow, false)
	b.pump(flow, func(v workflow.FlowView) bool { return len(b.f.latest(v, "check").StepRun.JobIDs) > 0 })
	var list struct {
		Items []jobs.Job `json:"items"`
	}
	b.ok(b.owner, "GET", "jobs?project_id="+string(b.f.project.ProjectID)+"&limit=100", nil, &list)
	var job jobs.Job
	for _, j := range list.Items {
		if j.Request.Target == version.Ref {
			job = j
		}
	}
	if !job.ID.Valid() {
		t.Fatal("real check job absent")
	}
	b.ok(b.worker, "POST", "nodes/observe", node.Observation{ProjectID: b.f.project.ProjectID, Capabilities: []string{"org.lantai.corecheck.manifest"}, Slots: 1}, nil)
	b.ok(b.worker, "POST", "jobs/run", jobs.Control{JobID: job.ID, ExpectedRevision: job.Revision}, &job)
	if job.State != "succeeded" || len(job.Checks) != 4 || job.Attempt == nil {
		t.Fatal("real corecheck did not finish", job)
	}
	for _, c := range job.Checks {
		if c.Check.Verdict != "pass" {
			t.Fatal("core check failed", c)
		}
	}
	b.point(fmt.Sprintf("round-%d-corecheck", round), flow, false)
	view = b.pump(flow, func(v workflow.FlowView) bool { return len(b.f.latest(v, "qa").StepRun.TaskIDs) > 0 })
	qa := b.f.latest(view, "qa").StepRun.TaskIDs[0]
	var qv tasks.View
	b.ok(b.checker, "GET", "tasks/"+string(qa), nil, &qv)
	b.deny(b.wire(b.maker, "POST", "tasks/claim", tasks.ClaimRequest{ProjectID: b.f.project.ProjectID, TaskID: qa, SeatID: qv.Seat.ID, ExpectedRevision: qv.Seat.Revision}), errcode.SelfReviewForbidden)
	qaAttempt := b.claim(b.checker, qa)
	// B actually reads the candidate bytes before its independent QA report.
	b.pull(b.checker, version, payload, "archive_review")
	var evidence ledger.AcceptedEvidence
	b.ok(b.checker, "POST", "evidence", ledger.ReviewEvidenceInput{ProjectID: b.f.project.ProjectID, Ref: version.Ref, ManifestDigest: version.ManifestDigest, Flow: view.Meta.Subject.Flow, Kind: "qa_report", QA: &ledger.QAReport{Tool: "synthetic-byte-reader", ToolVersion: "1.0", Observations: string(digest.Of(payload)), Verdict: "pass"}}, &evidence)
	b.ok(b.checker, "POST", "tasks/submit", tasks.SubmitRequest{AttemptRef: attemptRef(b.f.flowEnv, qa, qaAttempt)}, nil)
	db, err := sqlite.Open(t.Context(), filepath.Join(b.home, "db", "ledger.db"), sqlite.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	var op ids.ID
	err = db.QueryRowContext(t.Context(), "SELECT operation_id FROM ledger_check_records WHERE evidence_id=?", evidence.ID).Scan(&op)
	_ = db.Close()
	if err != nil {
		t.Fatal(err)
	}
	b.ok(b.checker, "GET", "tasks/"+string(qa), nil, &qv)
	b.ok(b.checker, "POST", "tasks/complete", tasks.CompleteRequest{TaskRef: tasks.TaskRef{ProjectID: b.f.project.ProjectID, TaskID: qa, ExpectedRevision: qv.Task.Revision}, AuthorityOperationID: op}, nil)
	b.point(fmt.Sprintf("round-%d-independent-qa", round), flow, false)
	view = b.pump(flow, func(v workflow.FlowView) bool { return v.Meta.ReviewTargetID.Valid() })
	var target ledger.ReviewTarget
	b.ok(b.owner, "GET", "review-targets/"+string(view.Meta.ReviewTargetID), nil, &target)
	if target.MakerID != b.maker.Context.PrincipalID || target.Flow.AttemptID != attempt.ID || target.ManifestDigest != version.ManifestDigest {
		t.Fatal("review target did not freeze current round", target)
	}
	b.point(fmt.Sprintf("round-%d-review-target", round), flow, false)
	return version, target, attempt, payload
}
func (b *businessRecovery) pull(s identity.IssuedSession, v catalog.VersionResult, want []byte, purpose string) {
	t := b.f.t
	dir := t.TempDir()
	session := filepath.Join(t.TempDir(), "session.json")
	writeJSON(t, session, map[string]string{"schema": "lantai.client-session/v1", "origin": b.origin, "token": s.Token})
	cmd := exec.CommandContext(t.Context(), b.bin, "pull", "--server", b.origin, "--allow-http", "--session-file", session, "--asset", string(v.AssetID), "--version", string(v.VersionID), "--directory", dir, "--purpose", purpose)
	cmd.Env = consistencyEnv()
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		t.Fatal(err, stderr.String())
	}
	got, err := os.ReadFile(filepath.Join(dir, "content.txt"))
	if err != nil || !bytes.Equal(got, want) {
		t.Fatal("exact bytes differ", err)
	}
	b.record(map[string]any{"kind": "cli-pull", "actor": s.Context.PrincipalID, "session": s.Context.SessionID, "purpose": purpose, "version": v.Ref, "sha256": digest.Of(got), "bytes": len(got), "stdout": out.String(), "result": "pass"})
}
func (b *businessRecovery) pointer(v catalog.VersionResult) ledger.AssetControl {
	t := b.f.t
	db, err := sqlite.Open(t.Context(), filepath.Join(b.home, "db", "ledger.db"), sqlite.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var out ledger.AssetControl
	err = db.QueryRowContext(t.Context(), "SELECT asset_id,revision,lifecycle,publication_state,published_version_id,publication_revision,pending_operation_id FROM ledger_asset_controls WHERE asset_id=?", v.AssetID).Scan(&out.AssetID, &out.Revision, &out.Lifecycle, &out.PublicationState, &out.PublishedVersionID, &out.PublicationRevision, &out.PendingOperationID)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func TestM2BusinessRecoveryAcceptance(t *testing.T) {
	b := newBusinessRecovery(t)
	old := b.old
	var started workflow.Result
	b.ok(b.owner, "POST", "flows", workflow.StartRequest{ProjectID: b.f.project.ProjectID, DefinitionRef: b.definition.Ref, ProfileRef: b.profile.Ref, Title: "synthetic full business recovery", AcceptanceCriteria: []string{"independent QA; exact release"}, ExpectedOutputs: []tc.OutputRequirement{{Slug: "out", AssetType: "doc", CandidateCount: 1}}}, &started)
	flow := started.FlowID
	b.point("flow-start-durable-command", flow, true)
	view := b.pump(flow, func(v workflow.FlowView) bool { return len(b.f.latest(v, "produce").StepRun.TaskIDs) > 0 })
	task := b.f.latest(view, "produce").StepRun.TaskIDs[0]
	var downstream tasks.Result
	b.ok(b.owner, "POST", "tasks", tasks.CreateRequest{ProjectID: b.f.project.ProjectID, Type: "produce", Title: "C consumes published bytes", AssigneeID: b.c.Context.PrincipalID, DependencyIDs: []ids.ID{task}, AcceptanceCriteria: []string{"published upstream version"}, ExpectedOutputs: []tc.OutputRequirement{{Slug: "c-out", AssetType: "doc", CandidateCount: 1}}}, &downstream)
	v1, target1, a1, _ := b.round(flow, old, 1)
	var discussion ledger.Message
	b.ok(b.maker, "POST", "messages", ledger.MessageInput{Target: ledger.DiscussionTarget{ProjectID: b.f.project.ProjectID, Kind: "version", ID: v1.VersionID}, Kind: "question", Text: "synthetic human review rework discussion", Anchors: []ledger.Anchor{}, Mentions: []ledger.Mention{}}, &discussion)
	b.ok(b.owner, "POST", "messages", ledger.MessageInput{Target: discussion.Target, Kind: "answer", Text: "synthetic revise and submit another exact version", ReplyTo: discussion.ID, Anchors: []ledger.Anchor{}, Mentions: []ledger.Mention{}}, nil)
	b.human(decision(target1, "return"))
	b.point("human-return", flow, false)
	b.pump(flow, func(v workflow.FlowView) bool {
		return v.Meta.ProductionRound == 2 && b.f.latest(v, "produce").StepRun.State == "running"
	})
	v2, target2, a2, payload := b.round(flow, v1, 2)
	if a1.ID == a2.ID || target1.ID == target2.ID {
		t.Fatal("rework reused old authority")
	}
	approval, _ := b.human(decision(target2, "approve"))
	b.point("human-approved-unpublished", flow, false)
	if p := b.pointer(v2); p.PublishedVersionID != old.VersionID {
		t.Fatal("approval moved old publication pointer", p)
	}
	// Dispatch approval commands while publication remains manual. C must still wait.
	b.pump(flow, func(v workflow.FlowView) bool { return b.f.latest(v, "publish").StepRun.State == "waiting" })
	var cv tasks.View
	b.ok(b.c, "GET", "tasks/"+string(downstream.TaskID), nil, &cv)
	if cv.Task.State != "waiting" {
		t.Fatalf("C dependency released before publication: %s", cv.Task.State)
	}
	b.record(map[string]any{"kind": "approved-unpublished", "pointer": b.pointer(v2), "C": cv, "approval": approval})
	var review ledger.Review
	if err := json.Unmarshal(approval.ResponseSummary, &review); err != nil {
		t.Fatal(err)
	}
	var pub ledger.Publication
	b.cli(b.owner, "review", "publish", ledger.PublishRequest{ProjectID: b.f.project.ProjectID, VersionID: v2.VersionID, ReviewID: review.ID, ExpectedRevision: b.pointer(v2).PublicationRevision, Action: "publish", Reason: "synthetic exact v2 release"}, &pub, 0)
	b.point("published-v2", flow, false)
	b.pump(flow, func(v workflow.FlowView) bool { return v.Flow.State == "completed" })
	b.ok(b.c, "GET", "tasks/"+string(downstream.TaskID), nil, &cv)
	if cv.Task.State != "todo" {
		t.Fatal("C dependency not released after publication", cv)
	}
	b.cAttempt = b.claim(b.c, downstream.TaskID)
	b.cRun = b.manual(b.c, b.cAttempt)
	b.pull(b.c, v2, payload, "production")
	b.restore(flow, v2, target1, a2, payload)
}

func (b *businessRecovery) restore(flow ids.ID, v catalog.VersionResult, oldTarget ledger.ReviewTarget, oldAttempt tasks.Attempt, payload []byte) {
	t := b.f.t
	ctx := t.Context()
	oldC := b.c
	oldMaker := b.maker
	oldOwner := b.owner
	b.stop()
	a := b.open()
	if err := a.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	// Accepted business facts, completed tasks/jobs and frozen dedup identities.
	before := b.facts(a)
	oldEpoch, err := a.Instance.RecoveryEpoch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, _, err := a.ExtensionManager.Snapshot(ctx, b.f.project.ProjectID, "org.lantai.corecheck.manifest", oldEpoch)
	if err != nil {
		t.Fatal(err)
	}
	oldKey, err := masterkey.Load(a.Instance.Config().SecretsDir)
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "complete-backup")
	backup, err := a.Backup(ctx, operations.BackupOptions{Destination: archive})
	if err != nil {
		t.Fatal(err)
	}
	verified, err := operations.VerifyBackup(ctx, archive)
	if err != nil || !reflect.DeepEqual(verified, backup) {
		t.Fatal("common backup verification", err)
	}
	b.record(map[string]any{"kind": "backup", "manifest": backup, "facts": before, "old_activation": snapshot})
	if err = a.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	restoredHome := filepath.Join(t.TempDir(), "empty-restore")
	marker, err := application.Restore(ctx, operations.RestoreOptions{Home: restoredHome, BackupDir: archive}, oldKey, identity.Config{Password: fastPassword})
	if err != nil {
		t.Fatal(err)
	}
	if marker.Restore == nil || marker.RecoveryEpoch <= oldEpoch {
		t.Fatal("restore epoch/latch missing")
	}
	opts := application.Options{Instance: operations.Options{Home: restoredHome}, Identity: identity.Config{Password: fastPassword}, Storage: storage.Config{MinFreeBytes: 1 << 20}}
	if opened, e := application.Open(ctx, opts); e == nil {
		_ = opened.Close(context.Background())
		t.Fatal("restore served before reconciliation")
	}
	offline, err := application.OpenOffline(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	const password = "synthetic restored administrator correct horse"
	var secret []byte
	err = offline.Maintenance(ctx, func(c context.Context) error {
		reset, e := offline.Identity.BeginOfflineReset(c, "ada", password)
		if e != nil {
			return e
		}
		secret, e = base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(reset.Enrollment.Secret)
		if e != nil {
			return e
		}
		_, e = reset.Confirm(c, totp.Code(secret, totp.Counter(time.Now())), "synthetic restore enrollment")
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	report := []byte("Synthetic software rehearsal: frozen backup and all retained business facts verified; no later deletions; revoke all copied credentials; old attempts and activation must not authorize new acceptance.")
	rr := operations.RecoveryReview{Contract: "lantai.recovery-review/v1", RunID: marker.Restore.RunID, BackupID: backup.BackupID, ManifestDigest: marker.Restore.ManifestDigest, RecoveryEpoch: marker.RecoveryEpoch, Administrator: string(oldOwner.Context.PrincipalID), RevocationsReconciled: true, DeletionsReconciled: true, OpenOperationsReconciled: true, EvidenceDigest: digest.Of(report), Note: "synthetic empty-directory business restore"}
	if err = offline.CompleteRestore(ctx, rr, report); err != nil {
		t.Fatal(err)
	}
	if err = offline.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	b.home = restoredHome
	a = b.open()
	after := b.facts(a)
	// Restore preserves historical records; epoch guards revoke acceptance
	// without rewriting archived tasks, attempts or manual execution facts.
	for table, rows := range before {
		if !reflect.DeepEqual(rows, after[table]) {
			b.record(map[string]any{"kind": "restore-mismatch", "table": table, "before": rows, "after": after[table]})
			t.Fatal("restore altered retained facts: " + table)
		}
	}
	for i := 0; i < len(b.records); i += 1000 {
		end := min(i+1000, len(b.records))
		if n, e := a.Events.Collect(ctx, b.records[i:end]); e != nil || n != 0 {
			t.Fatal("restore lost dedup identities", n, e)
		}
	}
	if err = a.ExtensionManager.CheckSnapshot(ctx, b.f.project.ProjectID, snapshot, ids.New(), marker.RecoveryEpoch); errcode.CodeOf(err) != errcode.ExtensionActivationStale {
		t.Fatal("old activation revived", err)
	}
	b.record(map[string]any{"kind": "old-activation-denied", "code": errcode.ExtensionActivationStale, "old_epoch": oldEpoch, "new_epoch": marker.RecoveryEpoch})
	oldFence := commands.Context{TaskID: b.cAttempt.TaskID, AttemptID: b.cAttempt.ID, LeaseFence: b.cAttempt.Fence.LeaseFence, RecoveryEpoch: b.cAttempt.Fence.RecoveryEpoch, SessionID: oldC.Context.SessionID}
	if e := a.Tasks.CheckVersionWrite(ctx, oldC.Context, oldFence, b.f.project.ProjectID, ""); errcode.CodeOf(e) != errcode.LeaseStale {
		t.Fatal("archived active C fence survived restore", e)
	}
	b.record(map[string]any{"kind": "old-active-fence-denied", "fence": oldFence, "code": errcode.LeaseStale, "layer": "internal final version-write guard; original archived session binding"})

	// Native domain bootstrap with fake proof time only for new credential setup.
	// Fresh C credential/session purpose remains production and is used by CLI.
	if err = a.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	b.f.clk.Set(time.Now().Add(time.Minute))
	a, err = application.Open(ctx, application.Options{Instance: operations.Options{Home: b.home, Clock: b.f.clk}, Identity: identity.Config{Password: fastPassword}, Storage: storage.Config{MinFreeBytes: 1 << 20}})
	if err != nil {
		t.Fatal(err)
	}
	f := b.f
	f.app = a
	f.inst = a.Instance
	f.id = a.Identity
	f.ledger = a.Ledger
	f.catalog = a.Catalog
	f.storage = a.Storage
	f.flows = a.Flows
	f.tasks = a.Tasks
	f.reviews = a.Reviews
	f.secret = secret
	login, err := f.id.Login(ctx, identity.LoginRequest{Name: "ada", Password: password, Code: f.fresh(), Channel: identity.ChannelCLI, Source: "192.0.2.10"})
	if err != nil {
		t.Fatal(err)
	}
	// Challenge/verify/execute use the fresh administrator; e.sudo has the old
	// password, so keep this restore setup explicit.
	sensitive := func(cmd identity.Command) identity.Result {
		ch, e := f.id.CreateChallenge(ctx, login.Context, cmd)
		if e != nil {
			t.Fatal(e)
		}
		g, e := f.id.VerifyChallenge(ctx, login.Context, ch.ChallengeID, f.fresh(), "192.0.2.10")
		if e != nil {
			t.Fatal(e)
		}
		res, e := f.id.Execute(ctx, login.Context, g.GrantID, f.key(), cmd)
		if e != nil {
			t.Fatal(e)
		}
		return res
	}
	mint := func(id ids.ID) identity.IssuedSession {
		p, e := f.id.GetPrincipal(ctx, login.Context, id)
		if e != nil {
			t.Fatal(e)
		}
		cred := sensitive(&identity.IssueCredential{PrincipalID: id, ExpectedRevision: p.Revision, Scopes: []identity.Scope{identity.ScopeRead, identity.ScopeIngest, identity.ScopeTask}})
		s, e := f.id.ExchangeToken(ctx, cred.Secret, identity.SessionRequest{Channel: identity.ChannelCLI, Purpose: "production"})
		if e != nil {
			t.Fatal(e)
		}
		return s
	}
	b.c = mint(oldC.Context.PrincipalID)
	b.maker = mint(oldMaker.Context.PrincipalID)
	b.owner = login
	f.owner = login.Context
	if err = a.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	b.startServe()
	b.deny(b.wire(oldC, "GET", "flows/"+string(flow), nil), errcode.TokenRevoked)
	b.deny(b.wire(oldOwner, "GET", "flows/"+string(flow), nil), errcode.TokenRevoked)
	b.deny(b.wire(b.maker, "POST", "tasks/renew", tasks.AttemptRef{ProjectID: f.project.ProjectID, TaskID: oldAttempt.TaskID, AttemptID: oldAttempt.ID, ExpectedRevision: oldAttempt.Revision, Fence: oldAttempt.Fence}), errcode.LeaseStale)
	b.lastProof = totp.Counter(f.clk.Now()) // new admin proof consumed during setup
	// Old target final acceptance is checked through legal fresh proof in the
	// assembled owner layer, to avoid waiting for fake setup time at real serve.
	b.stop()
	a = b.open()
	f.clk.Advance(time.Minute)
	d := decision(oldTarget, "approve")
	action, e := a.Reviews.HumanAction(ctx, d)
	if e != nil {
		t.Fatal(e)
	}
	// Reopen with the explicit proof clock; domain grants use the same native API.
	_ = action
	if err = a.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	a, err = application.Open(ctx, application.Options{Instance: operations.Options{Home: b.home, Clock: f.clk}, Identity: identity.Config{Password: fastPassword}, Storage: storage.Config{MinFreeBytes: 1 << 20}})
	if err != nil {
		t.Fatal(err)
	}
	ch, e := a.Identity.CreateDomainChallenge(ctx, b.owner.Context, []identity.HumanAction{action}, a)
	if e != nil {
		t.Fatal(e)
	}
	g, e := a.Identity.VerifyChallenge(ctx, b.owner.Context, ch.ChallengeID, f.fresh(), "192.0.2.10")
	if e != nil {
		t.Fatal(e)
	}
	items, e := a.Identity.DomainItems(ctx, b.owner.Context, g.GrantID)
	if e != nil {
		t.Fatal(e)
	}
	_, e = a.Reviews.Record(ctx, b.owner.Context, d, g.GrantID, items[0].OperationID, a.Identity)
	if errcode.CodeOf(e) != errcode.ReviewTargetStale {
		t.Fatal("historical target accepted", e)
	}
	b.record(map[string]any{"kind": "old-target-denied", "target": oldTarget, "operation": items[0].OperationID, "code": errcode.ReviewTargetStale, "layer": "assembled legal domain API with fresh HumanGrant"})
	if err = a.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	b.startServe()
	b.deny(b.wire(b.c, "POST", "task-runs/seal", ax.SealRequest{Mutation: mut(b.cRun, b.cAttempt.Fence), Outcome: execution.RunExecutionSucceeded, Stopped: true, CandidateIDs: []ids.ID{}, Limitations: []string{}}), errcode.LeaseStale)
	var newTask tasks.Result
	b.ok(b.owner, "POST", "tasks", tasks.CreateRequest{ProjectID: f.project.ProjectID, Type: "produce", Title: "restored C production session", AssigneeID: b.c.Context.PrincipalID, InputRefs: []ids.PermanentRef{v.Ref}, AcceptanceCriteria: []string{"consume exact published input"}, ExpectedOutputs: []tc.OutputRequirement{{Slug: "restored-c", AssetType: "doc", CandidateCount: 1}}}, &newTask)
	newAttempt := b.claim(b.c, newTask.TaskID)
	newRun := b.manual(b.c, newAttempt)
	if newRun.Run.ID == b.cRun.Run.ID || newAttempt.Fence.RecoveryEpoch != marker.RecoveryEpoch {
		t.Fatal("restored C did not register fresh production execution")
	}
	b.record(map[string]any{"kind": "fresh-C-production-run", "old": b.cRun, "new": newRun, "attempt": newAttempt})
	b.pull(b.c, v, payload, "production")
	final := b.view(flow)
	if final.Flow.State != "completed" {
		t.Fatal("completed flow lost")
	}
	b.record(map[string]any{"kind": "result", "result": "pass", "backup": backup.BackupID, "restored_epoch": marker.RecoveryEpoch, "flow": final, "fresh_C": b.c.Context, "serve_starts": b.starts, "records_replayed_after_restore": len(b.records), "limitations": "internal event injection; synthetic bootstrap clock; software process kill only; no device/RPO/soak/GATE"})
	if _, err = operations.VerifyBackup(ctx, archive); err != nil {
		t.Fatal("restore modified archive", err)
	}
}

// manual registers the current claimed task through the public T06 API.
func (b *businessRecovery) manual(actor identity.IssuedSession, attempt tasks.Attempt) ax.Record {
	t := b.f.t
	raw, err := os.ReadFile("../../schemas/examples/agent-execution/v1/agent-request/start.json")
	if err != nil {
		t.Fatal(err)
	}
	var wrap struct {
		Document ae.StartRequest `json:"document"`
	}
	if err = json.Unmarshal(raw, &wrap); err != nil {
		t.Fatal(err)
	}
	in := wrap.Document
	in.Fence = attempt.Fence
	in.OperationID = ids.New()
	in.TaskRunID = ids.New()
	in.BudgetID = ids.New()
	in.ExecutionKey, err = ae.ExecutionKey(in.TaskRunID, attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	var task tasks.View
	b.ok(actor, "GET", "tasks/"+string(attempt.TaskID), nil, &task)
	in.Input = task.Task.Input
	in.Activation = nil
	in.Profile.ID = ids.New()
	in.Profile.ActivationSnapshot = ae.ActivationSnapshot{}
	in.Profile.PlaybookRefs = []ids.PermanentRef{}
	in.Profile.SkillDigests = []digest.Digest{}
	in.Profile.RequiredCapabilities = []string{"manual_session"}
	in.Profile.AllowedTools = []string{"catalog.read", "catalog.create_version"}
	in.RequestHash, err = ae.RequestHash(in)
	if err != nil {
		t.Fatal(err)
	}
	var first, replay ax.Record
	b.ok(actor, "POST", "task-runs", in, &first)
	b.ok(actor, "POST", "task-runs", in, &replay)
	if !reflect.DeepEqual(first, replay) {
		t.Fatal("manual registration replay duplicated admission")
	}
	b.record(map[string]any{"kind": "manual-registration", "actor": actor.Context.PrincipalID, "session": actor.Context.SessionID, "request": in, "run": first, "replay": "same admission"})
	return first
}
