package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/catalog"
	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	tc "github.com/oujinhaoai/lantai/internal/contract/tasks"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/jobs"
	"github.com/oujinhaoai/lantai/internal/ledger"
	"github.com/oujinhaoai/lantai/internal/node"
	"github.com/oujinhaoai/lantai/internal/tasks"
	"github.com/oujinhaoai/lantai/internal/workflow"
)

// This suite exercises live HTTP listeners and a separate CLI process. Only
// identity/bootstrap configuration uses the existing legal domain helpers;
// no approved, grant, evidence, task, or receipt row is written by test SQL.
// Fake time makes proof expiry deterministic; this is not a wall-clock soak.
type reviewRemote struct {
	f                                            *appFlow
	bin, origin                                  string
	owner, other, maker, maker2, checker, worker identity.IssuedSession
	profile, definition                          catalog.VersionResult
	mu                                           sync.Mutex
	log                                          *os.File
}

type reviewWire struct {
	Status int
	Body   json.RawMessage
}

func newReviewRemote(t *testing.T, bin string) *reviewRemote {
	t.Helper()
	f := newAppFlow(t)
	producer, err := f.app.Extensions.Producer(t.Context(), "org.lantai.corecheck.manifest")
	if err != nil {
		t.Fatal(err)
	}
	base := f.approvedProfile("review-acceptance/bootstrap", profileDocument("bootstrap", producer), ids.PermanentRef{})
	p := profileDocument("independent-qa", producer)
	p.QARequired = true
	profile := f.approvedProfile("review-acceptance/qa-profile", p, base.Ref)
	r := &reviewRemote{f: f, bin: bin, profile: profile, definition: f.definition("review-acceptance/flow", "org.lantai.corecheck.manifest")}
	// Grant checker capability as well, so maker rejection tests principal
	// separation instead of merely failing due to a missing role.
	f.setRole(f.maker.PrincipalID, identity.RoleChecker, true)
	mint := func(id ids.ID, scopes []identity.Scope) identity.IssuedSession {
		p, e := f.id.GetPrincipal(t.Context(), f.login().Context, id)
		if e != nil {
			t.Fatal(e)
		}
		cred := f.sudo(&identity.IssueCredential{PrincipalID: id, ExpectedRevision: p.Revision, Scopes: scopes})
		s, e := f.id.ExchangeToken(t.Context(), cred.Secret, identity.SessionRequest{Channel: identity.ChannelCLI})
		if e != nil {
			t.Fatal(e)
		}
		return s
	}
	scopes := []identity.Scope{identity.ScopeRead, identity.ScopeIngest, identity.ScopeOrganize, identity.ScopeTask}
	r.maker = mint(f.maker.PrincipalID, scopes)
	r.maker2 = mint(f.maker.PrincipalID, scopes)
	r.checker = mint(f.checker.PrincipalID, scopes)
	r.worker = mint(f.worker.PrincipalID, []identity.Scope{identity.ScopeWorker})
	r.owner = f.login()
	r.other = f.login()
	if err = f.app.Sync(t.Context()); err != nil {
		t.Fatal(err)
	}
	servers := remoteServers(t, f.app, true)
	r.origin = "http://" + servers.Addresses.API
	dir := os.Getenv("LANTAI_REVIEW_EVIDENCE_DIR")
	if dir == "" {
		dir = t.TempDir()
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	r.log, err = os.OpenFile(filepath.Join(dir, filepath.Base(t.Name())+".jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.log.Close(); err != nil {
			t.Error(err)
		}
	})
	r.record(map[string]any{"kind": "fixture", "test": t.Name(), "profile": profile, "definition": r.definition, "owner": r.owner.Context.PrincipalID, "maker": r.maker.Context.PrincipalID, "maker_session": r.maker.Context.SessionID, "maker_other_session": r.maker2.Context.SessionID, "checker": r.checker.Context.PrincipalID, "clock": "fake; real network and processes", "bootstrap": "legal domain APIs and corecheck processes; no SQL writes", "cli_sha256": reviewBinaryDigest(t, bin)})
	return r
}
func (r *reviewRemote) record(v any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := json.NewEncoder(r.log).Encode(v); err != nil {
		r.f.t.Error(err)
	}
	if err := r.log.Sync(); err != nil {
		r.f.t.Error(err)
	}
}
func (r *reviewRemote) wire(s identity.IssuedSession, method, path string, in any) reviewWire {
	raw, _ := json.Marshal(in)
	var body io.Reader
	if in != nil {
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(r.f.t.Context(), method, r.origin+"/api/v1/"+path, body)
	if err != nil {
		r.f.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+s.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", string(ids.New()))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.f.t.Fatal(err)
	}
	out, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		r.f.t.Fatal(err)
	}
	logged := in
	if strings.HasSuffix(path, "/verify") {
		logged = map[string]string{"code": "[redacted synthetic TOTP]"}
	}
	r.record(map[string]any{"kind": "http", "method": method, "path": path, "actor": s.Context.PrincipalID, "session": s.Context.SessionID, "request": logged, "status": resp.StatusCode, "response": json.RawMessage(out), "response_raw": string(out), "response_sha256": digest.Of(out), "wall_utc": time.Now().UTC(), "clock": r.f.clk.Now()})
	return reviewWire{resp.StatusCode, out}
}
func (r *reviewRemote) ok(s identity.IssuedSession, method, path string, in, out any) {
	r.f.t.Helper()
	w := r.wire(s, method, path, in)
	if w.Status < 200 || w.Status >= 300 {
		r.f.t.Fatalf("%s %s: %d %s", method, path, w.Status, w.Body)
	}
	if out != nil {
		if err := json.Unmarshal(w.Body, out); err != nil {
			r.f.t.Fatal(err)
		}
	}
}
func (r *reviewRemote) deny(w reviewWire, code errcode.Code) {
	r.f.t.Helper()
	var e errcode.Envelope
	if err := json.Unmarshal(w.Body, &e); err != nil {
		r.f.t.Fatal(err)
	}
	if w.Status < 400 || e.Error.Code != code {
		r.f.t.Fatalf("expected %s, got %d %s", code, w.Status, w.Body)
	}
}
func (r *reviewRemote) cli(s identity.IssuedSession, command, action string, in any, out any, expected int) []byte {
	r.f.t.Helper()
	dir := r.f.t.TempDir()
	session := filepath.Join(dir, "session.json")
	input := filepath.Join(dir, "input.json")
	writeJSON(r.f.t, session, map[string]string{"schema": "lantai.client-session/v1", "origin": r.origin, "token": s.Token})
	writeJSON(r.f.t, input, in)
	args := []string{command, action, "--server", r.origin, "--allow-http", "--session-file", session, "--input", input, "--idempotency-key", string(ids.New())}
	cmd := exec.CommandContext(r.f.t.Context(), r.bin, args...)
	cmd.Env = consistencyEnv()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		if e, ok := err.(*exec.ExitError); ok {
			code = e.ExitCode()
		} else {
			r.f.t.Fatal(err)
		}
	}
	r.record(map[string]any{"kind": "cli", "command": command + " " + action, "actor": s.Context.PrincipalID, "request": in, "exit": code, "stdout": stdout.String(), "stderr": stderr.String()})
	if code != expected {
		r.f.t.Fatalf("CLI %s %s expected %d got %d: %s %s", command, action, expected, code, stdout.String(), stderr.String())
	}
	if out != nil {
		data := stdout.Bytes()
		if expected != 0 {
			data = stderr.Bytes()
		}
		if err := json.Unmarshal(data, out); err != nil {
			r.f.t.Fatalf("CLI JSON: %v: %s", err, data)
		}
	}
	return append([]byte(nil), stdout.Bytes()...)
}
func (r *reviewRemote) cliDeny(s identity.IssuedSession, in any, code errcode.Code, exit int) {
	r.f.t.Helper()
	var e errcode.Envelope
	r.cli(s, "human", "execute", in, &e, exit)
	if e.Error.Code != code {
		r.f.t.Fatalf("CLI expected %s got %s", code, e.Error.Code)
	}
}

func (r *reviewRemote) dispatch() {
	r.f.t.Helper()
	if err := r.f.app.Sync(r.f.t.Context()); err != nil {
		r.f.t.Fatal(err)
	}
	r.ok(r.owner, "POST", "flows/dispatch", map[string]int{"limit": 100}, nil)
}
func (r *reviewRemote) view(flow ids.ID) workflow.FlowView {
	var v workflow.FlowView
	r.ok(r.owner, "GET", "flows/"+string(flow), nil, &v)
	return v
}
func (r *reviewRemote) claim(s identity.IssuedSession, id ids.ID) tasks.Attempt {
	var v tasks.View
	r.ok(s, "GET", "tasks/"+string(id), nil, &v)
	var result tasks.Result
	r.ok(s, "POST", "tasks/claim", tasks.ClaimRequest{ProjectID: r.f.project.ProjectID, TaskID: id, SeatID: v.Seat.ID, ExpectedRevision: v.Seat.Revision}, &result)
	if result.Attempt == nil {
		r.f.t.Fatal("missing attempt")
	}
	return *result.Attempt
}
func (r *reviewRemote) start(title string) ids.ID {
	var s workflow.Result
	r.ok(r.owner, "POST", "flows", workflow.StartRequest{ProjectID: r.f.project.ProjectID, DefinitionRef: r.definition.Ref, ProfileRef: r.profile.Ref, Title: title, AcceptanceCriteria: []string{"independent QA and exact human approval"}, ExpectedOutputs: []tc.OutputRequirement{{Slug: "out", AssetType: "doc", CandidateCount: 1}}}, &s)
	r.dispatch()
	return s.FlowID
}
func decision(t ledger.ReviewTarget, verdict string) ledger.ReviewDecision {
	return ledger.ReviewDecision{Action: identity.ActRecordReview, TargetID: t.ID, ExpectedRevision: t.Revision, Verdict: verdict, Reason: "synthetic acceptance " + verdict, Waivers: []ledger.ReviewWaiver{}}
}
func intent(d ledger.ReviewDecision) map[string]any {
	return map[string]any{"kind": "review", "request": d}
}

type preparedReview struct {
	Challenge identity.Challenge     `json:"challenge"`
	Items     []identity.HumanAction `json:"items"`
}

func (r *reviewRemote) prepare(ds []ledger.ReviewDecision) preparedReview {
	items := []any{}
	for _, d := range ds {
		items = append(items, intent(d))
	}
	var p preparedReview
	r.ok(r.owner, "POST", "human/prepare", map[string]any{"items": items}, &p)
	return p
}
func (r *reviewRemote) verify(p preparedReview) (identity.Grant, []identity.HumanGrantItem) {
	var g identity.Grant
	r.ok(r.owner, "POST", "identity/challenges/"+string(p.Challenge.ChallengeID)+"/verify", map[string]string{"code": r.f.fresh()}, &g)
	var items []identity.HumanGrantItem
	r.ok(r.owner, "GET", "human/grants/"+string(g.GrantID), nil, &items)
	return g, items
}
func executeBody(g identity.Grant, i identity.HumanGrantItem) map[string]any {
	return map[string]any{"grant_id": g.GrantID, "operation_id": i.OperationID}
}
func (r *reviewRemote) grant(ds ...ledger.ReviewDecision) (identity.Grant, []identity.HumanGrantItem) {
	return r.verify(r.prepare(ds))
}

// round uploads with the actual CLI and uses real Task/Job/QA/Review HTTP APIs.
func (r *reviewRemote) round(flow ids.ID, slug string, previous *catalog.VersionResult, probeQA bool) (catalog.VersionResult, ledger.ReviewTarget) {
	t, f := r.f.t, r.f
	v := r.view(flow)
	task := f.latest(v, "produce").StepRun.TaskIDs[0]
	a := r.claim(r.maker, task)
	dir := t.TempDir()
	content := []byte("synthetic review payload " + slug + " " + string(task))
	if err := os.WriteFile(filepath.Join(dir, "content.txt"), content, 0600); err != nil {
		t.Fatal(err)
	}
	bind, _ := json.Marshal(bindTo(task, a))
	push := client.PushInput{ProjectID: string(f.project.ProjectID), Slug: slug, Task: bind, Content: client.ContentInput{AssetType: "doc", Files: []client.InputFile{{Path: "content.txt", Role: "source"}}, Rights: json.RawMessage(`{"usage":"production","license":"LicenseRef-Owned","sensitivity":"normal"}`)}}
	if previous != nil {
		push.Slug = ""
		push.AssetID = string(previous.AssetID)
		push.BaseVersionID = string(previous.VersionID)
	}
	input := filepath.Join(dir, "push.json")
	writeJSON(t, input, push)
	session := filepath.Join(dir, "session.json")
	writeJSON(t, session, map[string]string{"schema": "lantai.client-session/v1", "origin": r.origin, "token": r.maker.Token})
	cmd := exec.CommandContext(t.Context(), r.bin, "push", "--server", r.origin, "--allow-http", "--session-file", session, "--input", input, "--directory", dir, "--state", filepath.Join(dir, "state.json"))
	cmd.Env = consistencyEnv()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	r.record(map[string]any{"kind": "cli-push", "request": push, "input_sha256": digest.Of(content), "stdout": stdout.String(), "stderr": stderr.String(), "error": fmt.Sprint(err)})
	if err != nil {
		t.Fatal(err, stderr.String())
	}
	var version catalog.VersionResult
	if err = json.Unmarshal(stdout.Bytes(), &version); err != nil {
		t.Fatal(err)
	}
	r.ok(r.maker, "POST", "tasks/submit", tasks.SubmitRequest{AttemptRef: attemptRef(f.flowEnv, task, a), OutputRefs: []ids.PermanentRef{version.Ref}}, nil)
	r.dispatch()
	var list struct {
		Items []jobs.Job `json:"items"`
	}
	r.ok(r.owner, "GET", "jobs?project_id="+string(f.project.ProjectID)+"&limit=100", nil, &list)
	var job jobs.Job
	for _, j := range list.Items {
		if j.Request.Target == version.Ref {
			job = j
		}
	}
	if !job.ID.Valid() {
		t.Fatal("no real corecheck job")
	}
	r.ok(r.worker, "POST", "nodes/observe", node.Observation{ProjectID: f.project.ProjectID, Capabilities: []string{"org.lantai.corecheck.manifest"}, Slots: 1}, nil)
	r.ok(r.worker, "POST", "jobs/run", jobs.Control{JobID: job.ID, ExpectedRevision: job.Revision}, &job)
	if job.State != "succeeded" || len(job.Checks) != 4 {
		t.Fatalf("real corecheck: %s checks=%d", job.State, len(job.Checks))
	}
	r.dispatch()
	r.dispatch()
	v = r.view(flow)
	qa := f.latest(v, "qa").StepRun.TaskIDs[0]
	evidenceInput := ledger.ReviewEvidenceInput{ProjectID: f.project.ProjectID, Ref: version.Ref, ManifestDigest: version.ManifestDigest, Flow: v.Meta.Subject.Flow, Kind: "qa_report", QA: &ledger.QAReport{Tool: "synthetic-reader", ToolVersion: "1.0", Observations: "read synthetic bytes", Verdict: "pass"}}
	if probeQA {
		before := r.snapshot()
		var qaView tasks.View
		r.ok(r.maker2, "GET", "tasks/"+string(qa), nil, &qaView)
		r.deny(r.wire(r.maker2, "POST", "tasks/claim", tasks.ClaimRequest{ProjectID: f.project.ProjectID, TaskID: qa, SeatID: qaView.Seat.ID, ExpectedRevision: qaView.Seat.Revision}), errcode.SelfReviewForbidden)
		r.deny(r.wire(r.maker2, "POST", "evidence", evidenceInput), errcode.SelfReviewForbidden)
		r.unchanged(before)
	}
	qaAttempt := r.claim(r.checker, qa)
	var evidence ledger.AcceptedEvidence
	r.ok(r.checker, "POST", "evidence", evidenceInput, &evidence)
	r.ok(r.checker, "POST", "tasks/submit", tasks.SubmitRequest{AttemptRef: attemptRef(f.flowEnv, qa, qaAttempt)}, nil)
	var op ids.ID
	if err = f.inst.DB(ownership.Ledger).QueryRowContext(t.Context(), "SELECT operation_id FROM ledger_check_records WHERE evidence_id=?", evidence.ID).Scan(&op); err != nil {
		t.Fatal(err)
	}
	var qaView tasks.View
	r.ok(r.checker, "GET", "tasks/"+string(qa), nil, &qaView)
	r.ok(r.checker, "POST", "tasks/complete", tasks.CompleteRequest{TaskRef: tasks.TaskRef{ProjectID: f.project.ProjectID, TaskID: qa, ExpectedRevision: qaView.Task.Revision}, AuthorityOperationID: op}, nil)
	r.dispatch()
	r.dispatch()
	v = r.view(flow)
	var target ledger.ReviewTarget
	r.ok(r.owner, "GET", "review-targets/"+string(v.Meta.ReviewTargetID), nil, &target)
	if target.MakerID != r.maker.Context.PrincipalID || target.ManifestDigest != version.ManifestDigest || target.Flow.Round != int64(v.Meta.ProductionRound) {
		t.Fatal("wrong frozen target", target)
	}
	return version, target
}
func (r *reviewRemote) snapshot() []byte {
	t := r.f.t
	out := map[string]any{}
	for _, table := range []string{"ledger_reviews", "ledger_review_targets", "ledger_check_records", "command_receipts", "ledger_publications"} {
		var n int
		err := r.f.inst.DB(ownership.Ledger).QueryRowContext(t.Context(), "SELECT count(*) FROM "+table).Scan(&n)
		if err != nil {
			t.Fatal(err)
		}
		out[table] = n
	}
	rows, err := r.f.inst.DB(ownership.Ledger).QueryContext(t.Context(), "SELECT json_object('id',version_id,'revision',revision,'state',state,'availability',availability,'lifecycle',lifecycle,'review',effective_review_id,'target',review_target_id) FROM ledger_version_states ORDER BY version_id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	states := []string{}
	for rows.Next() {
		var s string
		if err = rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		states = append(states, s)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	out["states"] = states
	b, _ := json.Marshal(out)
	r.record(map[string]any{"kind": "ledger-observation", "snapshot": json.RawMessage(b)})
	return b
}
func (r *reviewRemote) unchanged(before []byte) {
	r.f.t.Helper()
	if !bytes.Equal(before, r.snapshot()) {
		r.f.t.Fatal("denial/replay changed authoritative facts, receipts or version states")
	}
}
func (r *reviewRemote) assertState(v catalog.VersionResult, want string) {
	s, e := r.f.ledger.VersionControl(r.f.t.Context(), v.VersionID)
	if e != nil || s.ReviewState != want {
		r.f.t.Fatalf("review state want %s got %+v %v", want, s, e)
	}
	a, e := r.f.ledger.AssetControl(r.f.t.Context(), v.AssetID)
	if e != nil || a.PublicationState == "published" {
		r.f.t.Fatal("unexpected publication", a, e)
	}
	r.record(map[string]any{"kind": "state", "version": s, "asset": a})
}

func TestRemoteReviewFrozenRework(t *testing.T) {
	r := newReviewRemote(t, reviewAcceptanceBinary(t))
	flow := r.start("rework")
	v1, target := r.round(flow, "review-acceptance/rework", nil, true)
	d := decision(target, "approve")
	p := r.prepare([]ledger.ReviewDecision{d})
	before := r.snapshot()
	// Verify takes only the code; replacing any approved binding is rejected at
	// the public schema boundary, rather than trusting a client-supplied digest.
	for _, field := range []string{"manifest_digest", "target_id", "expected_revision", "action", "operation_id"} {
		r.deny(r.wire(r.owner, "POST", "identity/challenges/"+string(p.Challenge.ChallengeID)+"/verify", map[string]any{"code": "000000", field: string(ids.New())}), errcode.SchemaInvalid)
	}
	r.deny(r.wire(r.other, "POST", "identity/challenges/"+string(p.Challenge.ChallengeID)+"/verify", map[string]string{"code": r.f.fresh()}), errcode.HumanGrantMismatch)
	g, items := r.verify(p)
	r.deny(r.wire(r.owner, "POST", "identity/challenges/"+string(p.Challenge.ChallengeID)+"/verify", map[string]string{"code": r.f.fresh()}), errcode.HumanGrantMismatch)
	body := executeBody(g, items[0])
	for _, field := range []string{"manifest_digest", "target_id", "expected_revision", "action", "request"} {
		mut := map[string]any{"grant_id": g.GrantID, "operation_id": items[0].OperationID, field: "replacement"}
		r.deny(r.wire(r.owner, "POST", "human/execute", mut), errcode.SchemaInvalid)
	}
	r.deny(r.wire(r.owner, "POST", "human/execute", map[string]any{"grant_id": g.GrantID, "operation_id": ids.New()}), errcode.HumanGrantMismatch)
	r.deny(r.wire(r.other, "POST", "human/execute", body), errcode.HumanGrantMismatch)
	r.deny(r.wire(r.maker, "POST", "human/execute", body), errcode.Forbidden)
	r.deny(r.wire(r.worker, "POST", "human/execute", body), errcode.Forbidden)
	r.unchanged(before)
	// Expire both a challenge and an unconsumed grant using explicit fake time.
	exp := r.prepare([]ledger.ReviewDecision{d})
	r.f.clk.Advance(6 * time.Minute)
	r.deny(r.wire(r.owner, "POST", "identity/challenges/"+string(exp.Challenge.ChallengeID)+"/verify", map[string]string{"code": r.f.fresh()}), errcode.HumanProofRequired)
	r.deny(r.wire(r.owner, "POST", "human/execute", body), errcode.HumanProofRequired)
	r.unchanged(before)
	g, items = r.grant(d)
	body = executeBody(g, items[0]) // held approval of old target
	// Human return is a real CLI request; the pending approve must become stale.
	ret, ri := r.grant(decision(target, "return"))
	r.cli(r.owner, "human", "execute", executeBody(ret, ri[0]), nil, 0)
	r.assertState(v1, "changes_requested")
	r.dispatch()
	r.dispatch()
	v2, newTarget := r.round(flow, "review-acceptance/rework", &v1, false)
	if newTarget.ID == target.ID || newTarget.Flow.Round <= target.Flow.Round || v1.ManifestDigest == v2.ManifestDigest {
		t.Fatal("rework did not freeze a new version/round")
	}
	before = r.snapshot()
	r.deny(r.wire(r.owner, "POST", "human/execute", body), errcode.ReviewTargetStale)
	// A fresh proof for the historical card still cannot accept a new round.
	old, oi := r.grant(d)
	r.cliDeny(r.owner, executeBody(old, oi[0]), errcode.ReviewTargetStale, 4)
	r.unchanged(before)
	// Ordinary commentary including an answer is not a review fact.
	r.ok(r.maker, "POST", "messages", ledger.MessageInput{Target: ledger.DiscussionTarget{ProjectID: r.f.project.ProjectID, Kind: "version", ID: v2.VersionID}, Kind: "note", Text: "synthetic model says approved", Anchors: []ledger.Anchor{}, Mentions: []ledger.Mention{}}, nil)
	var question ledger.Message
	r.ok(r.maker, "POST", "messages", ledger.MessageInput{Target: ledger.DiscussionTarget{ProjectID: r.f.project.ProjectID, Kind: "version", ID: v2.VersionID}, Kind: "question", Text: "synthetic: may this be approved?", Anchors: []ledger.Anchor{}, Mentions: []ledger.Mention{}}, &question)
	r.ok(r.owner, "POST", "messages", ledger.MessageInput{Target: question.Target, Kind: "answer", Text: "synthetic answer is not approval", ReplyTo: question.ID, Anchors: []ledger.Anchor{}, Mentions: []ledger.Mention{}}, nil)
	r.assertState(v2, "submitted")
	// A forged future revision can be frozen for historical recovery, but final
	// acceptance must still compare the live revision.
	future := decision(newTarget, "approve")
	future.ExpectedRevision++
	fg, fi := r.grant(future)
	before = r.snapshot()
	r.deny(r.wire(r.owner, "POST", "human/execute", executeBody(fg, fi[0])), errcode.ReviewTargetStale)
	r.unchanged(before)
	// The exact new target succeeds, duplicate REST/CLI returns its receipt.
	ng, ni := r.grant(decision(newTarget, "approve"))
	var first commands.Receipt
	r.ok(r.owner, "POST", "human/execute", executeBody(ng, ni[0]), &first)
	r.assertState(v2, "approved")
	r.assertReviewReceipt(first, newTarget, "approve")
	before = r.snapshot()
	r.f.clk.Advance(6 * time.Minute) // completed receipts survive proof expiry
	var replay commands.Receipt
	r.cli(r.owner, "human", "execute", executeBody(ng, ni[0]), &replay, 0)
	if !reflect.DeepEqual(first, replay) {
		t.Fatal("CLI replay differs from original receipt")
	}
	r.unchanged(before)
	r.record(map[string]any{"kind": "result", "case": "frozen-rework", "old_target": target, "new_target": newTarget, "receipt": first, "result": "pass"})
}

func TestRemoteReviewBatchRevalidation(t *testing.T) {
	bin := reviewAcceptanceBinary(t)
	for _, mode := range []string{"state", "permission"} {
		t.Run(mode, func(t *testing.T) {
			r := newReviewRemote(t, bin)
			fa := r.start("batch-a")
			va, ta := r.round(fa, "review-acceptance/a", nil, false)
			fb := r.start("batch-b")
			vb, tb := r.round(fb, "review-acceptance/b", nil, false)
			ds := []ledger.ReviewDecision{decision(ta, "approve"), decision(tb, "approve")}
			// Prepare the batch with the CLI, then redeem the same persisted challenge.
			var p preparedReview
			r.cli(r.owner, "human", "prepare", map[string]any{"items": []any{intent(ds[0]), intent(ds[1])}}, &p, 0)
			g, items := r.verify(p)
			if len(items) != 2 || items[0].OperationID == items[1].OperationID {
				t.Fatal("batch did not freeze two stable children")
			}
			var first commands.Receipt
			r.ok(r.owner, "POST", "human/execute", executeBody(g, items[0]), &first)
			r.assertState(va, "approved")
			r.assertReviewReceipt(first, ta, "approve")
			var next commands.Receipt
			if mode == "state" {
				ret, ri := r.grant(decision(tb, "return"))
				r.ok(r.owner, "POST", "human/execute", executeBody(ret, ri[0]), nil)
				r.dispatch()
				r.dispatch()
				_, fresh := r.round(fb, "review-acceptance/b", &vb, false)
				if fresh.ID == tb.ID {
					t.Fatal("same target after state change")
				}
				before := r.snapshot()
				r.cli(r.owner, "human", "execute", executeBody(g, items[0]), &next, 0)
				r.cliDeny(r.owner, executeBody(g, items[1]), errcode.ReviewTargetStale, 4)
				r.unchanged(before)
				r.assertState(vb, "changes_requested")
			} else {
				// Hold the second incoming HTTP request at a deterministic proxy barrier.
				// Revoke using another real human session, then release to final acceptance.
				origin, _ := url.Parse(r.origin)
				proxy := httputil.NewSingleHostReverseProxy(origin)
				arrived := make(chan struct{})
				release := make(chan struct{})
				held := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
					close(arrived)
					select {
					case <-release:
						proxy.ServeHTTP(w, q)
					case <-q.Context().Done():
					}
				}))
				defer held.Close()
				original := r.origin
				r.origin = held.URL
				done := make(chan reviewWire, 1)
				go func() { done <- r.wire(r.owner, "POST", "human/execute", executeBody(g, items[1])) }()
				select {
				case <-arrived:
				case <-time.After(10 * time.Second):
					t.Fatal("request never reached barrier")
				}
				r.origin = original
				r.changeOwnerRole(false)
				before := r.snapshot()
				close(release)
				r.deny(<-done, errcode.NotFound)
				// Completed receipts also require current permission; revoked callers do
				// not receive a historical result until authority is restored.
				r.cliDeny(r.owner, executeBody(g, items[0]), errcode.NotFound, 1)
				r.cliDeny(r.owner, executeBody(g, items[1]), errcode.NotFound, 1)
				r.unchanged(before)
				r.changeOwnerRole(true)
				before = r.snapshot()
				r.cli(r.owner, "human", "execute", executeBody(g, items[0]), &next, 0)
				r.unchanged(before)
				var second commands.Receipt
				r.cli(r.owner, "human", "execute", executeBody(g, items[1]), &second, 0)
				r.assertReviewReceipt(second, tb, "approve")
				r.assertState(vb, "approved")
			}
			if !reflect.DeepEqual(first, next) {
				t.Fatal("successful child did not return original receipt")
			}
			r.record(map[string]any{"kind": "batch-summary", "mode": mode, "parent_operation": g.OperationID, "children": items, "first_receipt": first, "replayed_receipt": next, "result": "pass", "server_aggregate_endpoint": false, "final_snapshot": json.RawMessage(r.snapshot())})
		})
	}
}
func (r *reviewRemote) changeOwnerRole(grant bool) {
	t := r.f.t
	var members struct {
		Revision int64 `json:"revision"`
	}
	// The project membership revision is public, not inferred from SQL.
	w := r.wire(r.other, "GET", "identity/projects/"+string(r.f.project.ProjectID)+"/members", nil)
	if w.Status != 200 {
		t.Fatal(string(w.Body))
	}
	if err := json.Unmarshal(w.Body, &members); err != nil {
		t.Fatal(err)
	}
	cmd := identity.SetProjectRole{ProjectID: r.f.project.ProjectID, PrincipalID: r.owner.Context.PrincipalID, Role: identity.RoleOwner, Grant: grant, ExpectedRevision: members.Revision}
	action := identity.ActRevokeProjectRole
	if grant {
		action = identity.ActGrantProjectRole
	}
	request := map[string]any{"action": action, "command": cmd}
	var ch identity.Challenge
	r.ok(r.other, "POST", "identity/challenges", request, &ch)
	var g identity.Grant
	r.ok(r.other, "POST", "identity/challenges/"+string(ch.ChallengeID)+"/verify", map[string]string{"code": r.f.fresh()}, &g)
	request["grant_id"] = g.GrantID
	r.ok(r.other, "POST", "identity/commands", request, nil)
}

// Optional fixed executable lets an acceptance run archive the exact CLI bytes.
func reviewAcceptanceBinary(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("LANTAI_REVIEW_BIN"); p != "" {
		if !filepath.IsAbs(p) {
			t.Fatal("LANTAI_REVIEW_BIN must be absolute")
		}
		if _, err := os.Stat(p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	return buildConsistencyCLI(t)
}
func reviewBinaryDigest(t *testing.T, p string) digest.Digest {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return digest.Of(b)
}

func (r *reviewRemote) assertReviewReceipt(receipt commands.Receipt, target ledger.ReviewTarget, verdict string) {
	r.f.t.Helper()
	var review ledger.Review
	if err := json.Unmarshal(receipt.ResponseSummary, &review); err != nil {
		r.f.t.Fatal(err)
	}
	if review.TargetID != target.ID || review.VersionID != target.VersionID || review.ManifestDigest != target.ManifestDigest || review.Verdict != verdict || review.OperationID != receipt.OperationID {
		r.f.t.Fatal("receipt does not identify the exact accepted target")
	}
	for _, table := range []string{"command_receipts", "ledger_reviews"} {
		var n int
		if err := r.f.inst.DB(ownership.Ledger).QueryRowContext(r.f.t.Context(), "SELECT count(*) FROM "+table+" WHERE operation_id=?", receipt.OperationID).Scan(&n); err != nil {
			r.f.t.Fatal(err)
		}
		if n != 1 {
			r.f.t.Fatalf("%s contains %d facts for accepted operation", table, n)
		}
	}
	r.record(map[string]any{"kind": "accepted-fact", "operation_id": receipt.OperationID, "target": target.ID, "version": target.VersionID, "manifest_digest": target.ManifestDigest, "receipt_count": 1, "review_count": 1})
}
