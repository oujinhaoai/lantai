package integration

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/application"
	"github.com/oujinhaoai/lantai/internal/catalog"
	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/events"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/jobs"
	"github.com/oujinhaoai/lantai/internal/ledger"
	"github.com/oujinhaoai/lantai/internal/operations"
	"github.com/oujinhaoai/lantai/internal/query"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
	"github.com/oujinhaoai/lantai/internal/tasks"
)

// m207 uses the assembled application and real domain owners. Synchronization
// points are between client requests; no task/provider/authorization substitutes
// or SQL cursor timestamps are installed.
type m207 struct {
	*env
	app           *application.App
	origin        string
	owner, reader *client.Client
	readerSession identity.IssuedSession
	credential    string
	trace         []any
}

func newM207(t *testing.T) *m207 {
	t.Helper()
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	e.setRole(e.admin.Context.PrincipalID, identity.RoleOwner, true)
	a := reopenApplication(t, e, storage.Config{MinFreeBytes: 1 << 20})
	servers := remoteServers(t, a, true)
	f := &m207{env: e, app: a, origin: "http://" + servers.Addresses.API}
	p, _ := e.agent("resync-reader@synthetic", identity.RoleContributor)
	p, err := e.id.GetPrincipal(t.Context(), e.login().Context, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	r := e.sudo(&identity.IssueCredential{PrincipalID: p.ID, ExpectedRevision: p.Revision, Scopes: []identity.Scope{identity.ScopeRead, identity.ScopeIngest, identity.ScopeOrganize, identity.ScopeTask}})
	f.credential = r.Secret
	f.refresh()
	return f
}
func (f *m207) connect(s identity.IssuedSession) *client.Client {
	f.t.Helper()
	c, err := client.New(client.Config{BaseURL: f.origin, SessionToken: s.Token, AllowHTTP: true})
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(c.Close)
	return c
}
func (f *m207) refresh() {
	f.t.Helper()
	f.owner = f.connect(f.login())
	s, err := f.id.ExchangeToken(f.t.Context(), f.credential, identity.SessionRequest{Channel: identity.ChannelCLI})
	if err != nil {
		f.t.Fatal(err)
	}
	f.readerSession = s
	f.reader = f.connect(s)
}
func (f *m207) call(c *client.Client, method, path, key string, in, out any) error {
	f.t.Helper()
	r, err := c.Do(f.t.Context(), method, "/api/v1/"+path, in, client.Options{IdempotencyKey: key})
	// Human factor verification and credentials are never placed in evidence.
	if !strings.HasPrefix(path, "human/") && !strings.HasPrefix(path, "identity/") {
		entry := map[string]any{"method": method, "path": path, "status": r.Status, "response": json.RawMessage(r.Body)}
		if err != nil {
			entry["error"] = err.Error()
		}
		f.trace = append(f.trace, entry)
	}
	if err == nil && strings.Contains(path, "/milestones") {
		model := "MilestonePage"
		if method == http.MethodPost {
			model = "MilestoneResult"
		} else if strings.HasSuffix(path, "/progress") {
			model = "MilestoneProgress"
		}
		validatePublic(f.t, model, r.Body)
	}
	if err == nil && out != nil {
		err = r.Decode(out)
	}
	return err
}
func (f *m207) must(c *client.Client, method, path string, in, out any) {
	f.t.Helper()
	if err := f.call(c, method, path, f.key(), in, out); err != nil {
		f.t.Fatal(method, path, err)
	}
}
func (f *m207) task(c *client.Client, id ids.ID) tasks.View {
	f.t.Helper()
	var v tasks.View
	f.must(c, http.MethodGet, "tasks/"+string(id), nil, &v)
	return v
}
func (f *m207) create(project ids.ID, title string) tasks.View {
	f.t.Helper()
	var r tasks.Result
	f.must(f.owner, http.MethodPost, "tasks", tasks.CreateRequest{ProjectID: project, Type: "question", Title: title, AcceptanceCriteria: []string{"synthetic answer"}, Role: identity.RoleContributor}, &r)
	return f.task(f.owner, r.TaskID)
}
func (f *m207) sync() {
	f.t.Helper()
	if err := f.app.Sync(f.t.Context()); err != nil {
		f.t.Fatal(err)
	}
}
func (f *m207) evidence(value any) {
	f.t.Helper()
	raw, err := json.MarshalIndent(map[string]any{"observations": value, "requests": f.trace}, "", "  ")
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Logf("M2-07 observations: %s", mustJSON(value))
	if root := os.Getenv("LANTAI_M207_EVIDENCE"); root != "" {
		if err = os.MkdirAll(root, 0700); err != nil {
			f.t.Fatal(err)
		}
		path := filepath.Join(root, f.t.Name()+".json")
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			f.t.Fatal(err)
		}
		_, werr := file.Write(raw)
		cerr := file.Close()
		if werr != nil || cerr != nil {
			f.t.Fatal(werr, cerr)
		}
	}
}

// This initially exposed the missing T07 routes on the delivery baseline.
func TestM2ResyncAcceptanceMilestoneREST(t *testing.T) {
	f := newM207(t)
	v := f.create(f.project.ProjectID, "milestone task")
	base := "projects/" + string(f.project.ProjectID) + "/milestones"
	in := map[string]any{"expected_revision": 0, "milestone": identity.Milestone{ProjectID: f.project.ProjectID, Name: "synthetic delivery", OwnerID: f.admin.Context.PrincipalID, TaskIDs: []ids.ID{v.Task.ID}}}
	if err := f.call(f.owner, http.MethodPost, base, f.key(), map[string]any{"milestone": in["milestone"]}, nil); m207Code(err) != errcode.SchemaInvalid {
		t.Fatal("missing conditional revision accepted", err)
	}
	missingTasks := in["milestone"].(identity.Milestone)
	missingTasks.TaskIDs = nil
	if err := f.call(f.owner, http.MethodPost, base, f.key(), map[string]any{"expected_revision": 0, "milestone": missingTasks}, nil); m207Code(err) != errcode.SchemaInvalid {
		t.Fatal("null task set accepted", err)
	}
	invalidOwner := in["milestone"].(identity.Milestone)
	invalidOwner.OwnerID = f.readerSession.Context.PrincipalID
	if err := f.call(f.owner, http.MethodPost, base, f.key(), map[string]any{"expected_revision": 0, "milestone": invalidOwner}, nil); m207Code(err) != errcode.SchemaInvalid {
		t.Fatal("non-owner designated as milestone owner", err)
	}
	var meta struct {
		Capabilities []string `json:"capabilities"`
	}
	f.must(f.owner, http.MethodGet, "meta", nil, &meta)
	if !slices.Contains(meta.Capabilities, "milestones") {
		t.Fatal("milestone capability missing", meta)
	}
	key := f.key()
	var r identity.Result
	f.must(f.owner, http.MethodGet, base, nil, &struct {
		Items []identity.Milestone `json:"items"`
	}{})
	if err := f.call(f.owner, http.MethodPost, base, key, in, &r); err != nil {
		t.Fatal(err)
	}
	var m identity.Milestone
	if err := json.Unmarshal(r.Summary, &m); err != nil {
		t.Fatal(err)
	}
	var replay identity.Result
	if err := f.call(f.owner, http.MethodPost, base, key, in, &replay); err != nil || !replay.Replayed || replay.OperationID != r.OperationID {
		t.Fatal("milestone replay", replay, err)
	}
	changedPayload := in["milestone"].(identity.Milestone)
	changedPayload.Name = "conflicting delivery"
	if err := f.call(f.owner, http.MethodPost, base, key, map[string]any{"expected_revision": 0, "milestone": changedPayload}, nil); m207Code(err) != errcode.IdempotencyConflict {
		t.Fatal("milestone key reused for changed payload", err)
	}
	progress := func(c *client.Client, project ids.ID) (identity.MilestoneProgress, error) {
		var p identity.MilestoneProgress
		err := f.call(c, http.MethodGet, "projects/"+string(project)+"/milestones/"+string(m.ID)+"/progress", "", nil, &p)
		return p, err
	}
	p, err := progress(f.reader, f.project.ProjectID)
	if err != nil || p.Completed != 0 || p.Total != 1 {
		t.Fatal(p, err)
	}
	denied := f.call(f.reader, http.MethodPost, base, f.key(), in, nil)
	if m207Code(denied) != errcode.Forbidden {
		t.Fatal("non-owner changed milestone", denied)
	}
	var claimed tasks.Result
	f.must(f.reader, http.MethodPost, "tasks/claim", tasks.ClaimRequest{ProjectID: f.project.ProjectID, TaskID: v.Task.ID, SeatID: v.Seat.ID, ExpectedRevision: v.Seat.Revision}, &claimed)
	a := claimed.Attempt
	var submitted tasks.Result
	f.must(f.reader, http.MethodPost, "tasks/submit", tasks.SubmitRequest{AttemptRef: tasks.AttemptRef{ProjectID: f.project.ProjectID, TaskID: v.Task.ID, AttemptID: a.ID, ExpectedRevision: a.Revision, Fence: a.Fence}}, &submitted)
	v = f.task(f.owner, v.Task.ID)
	f.must(f.owner, http.MethodPost, "tasks/complete", tasks.CompleteRequest{TaskRef: tasks.TaskRef{ProjectID: f.project.ProjectID, TaskID: v.Task.ID, ExpectedRevision: v.Task.Revision}, AuthorityOperationID: v.Meta.SubmitOperationID}, nil)
	p, err = progress(f.reader, f.project.ProjectID)
	if err != nil || p.Completed != 1 || p.Total != 1 {
		t.Fatal(p, err)
	}
	other, err := f.catalog.CreateProject(t.Context(), catalog.ProjectRequest{Who: f.login().Context, IdempotencyKey: f.key(), Key: "resync-other", Name: "other synthetic project"})
	if err != nil {
		t.Fatal(err)
	}
	saved := f.project
	f.project = other
	f.setRole(f.admin.Context.PrincipalID, identity.RoleOwner, true)
	f.project = saved
	foreign := f.create(other.ProjectID, "foreign task")
	m.TaskIDs = []ids.ID{v.Task.ID, foreign.Task.ID}
	if err = f.call(f.owner, http.MethodPost, base, f.key(), map[string]any{"expected_revision": m.Revision, "milestone": m}, nil); m207Code(err) != errcode.NotFound {
		t.Fatal("foreign task accepted", err)
	}
	if _, err = progress(f.owner, other.ProjectID); m207Code(err) != errcode.NotFound {
		t.Fatal("foreign milestone readable", err)
	}
	if _, err = progress(f.reader, other.ProjectID); m207Code(err) != errcode.NotFound {
		t.Fatal("foreign project readable", err)
	}
	m.TaskIDs = []ids.ID{v.Task.ID}
	m.Name = "updated delivery"
	f.must(f.owner, http.MethodPost, base, map[string]any{"expected_revision": m.Revision, "milestone": m}, &r)
	stale := f.call(f.owner, http.MethodPost, base, f.key(), map[string]any{"expected_revision": m.Revision, "milestone": m}, nil)
	if m207Code(stale) != errcode.PreconditionFailed {
		t.Fatal("stale milestone revision accepted", stale)
	}
	wrongProject := m
	wrongProject.ProjectID = other.ProjectID
	if err := f.call(f.owner, http.MethodPost, base, f.key(), map[string]any{"expected_revision": m.Revision, "milestone": wrongProject}, nil); m207Code(err) != errcode.SchemaInvalid {
		t.Fatal("route project mismatch accepted", err)
	}
	var listed struct {
		Items []identity.Milestone `json:"items"`
	}
	f.must(f.reader, http.MethodGet, base, nil, &listed)
	if len(listed.Items) != 1 || listed.Items[0].ID != m.ID || listed.Items[0].Revision != 2 || listed.Items[0].Name != m.Name {
		t.Fatal(listed)
	}
	f.evidence(map[string]any{"milestone": listed.Items[0], "progress": p, "replay_operation": replay.OperationID, "foreign_task_rejected": foreign.Task.ID, "stale_revision_rejected": true})
}

func (f *m207) postMessage(c *client.Client, key string, in ledger.MessageInput) ledger.Message {
	f.t.Helper()
	var m ledger.Message
	if err := f.call(c, http.MethodPost, "messages", key, in, &m); err != nil {
		f.t.Fatal(err)
	}
	return m
}
func (f *m207) inbox(c *client.Client) query.InboxPage {
	f.t.Helper()
	var p query.InboxPage
	f.must(c, http.MethodGet, "inbox?project_id="+string(f.project.ProjectID), nil, &p)
	return p
}
func m207InboxHas(p query.InboxPage, id ids.ID) bool {
	return slices.ContainsFunc(p.Items, func(i query.InboxItem) bool { return i.Object.Ref.ID == id })
}

func TestM2ResyncAcceptanceDiscussionsAndInbox(t *testing.T) {
	f := newM207(t)
	v := f.create(f.project.ProjectID, "blocked discussion")
	target := ledger.DiscussionTarget{ProjectID: f.project.ProjectID, Kind: "task", ID: v.Task.ID}
	messages := []ledger.Message{}
	for _, kind := range []string{"question", "answer", "handoff", "decision_proposal"} {
		in := ledger.MessageInput{Target: target, Kind: kind, Text: "synthetic " + kind}
		if kind == "answer" {
			in.ReplyTo = messages[0].ID
		}
		actor := f.reader
		if kind == "answer" {
			actor = f.owner
		}
		key := f.key()
		m := f.postMessage(actor, key, in)
		again := f.postMessage(actor, key, in)
		if m.ID != again.ID || m.Sequence != again.Sequence {
			t.Fatal("duplicate discussion", m, again)
		}
		in.Text = "changed payload"
		if err := f.call(actor, http.MethodPost, "messages", key, in, nil); m207Code(err) != errcode.IdempotencyConflict {
			t.Fatal("message key conflict", err)
		}
		messages = append(messages, m)
	}
	var list struct {
		Items []ledger.Message `json:"items"`
	}
	path := "messages?project_id=" + string(f.project.ProjectID) + "&kind=task&id=" + string(v.Task.ID)
	f.must(f.reader, http.MethodGet, path, nil, &list)
	if len(list.Items) != 4 {
		t.Fatal("discussion replay added records", list)
	}
	other := f.create(f.project.ProjectID, "different object")
	if err := f.call(f.reader, http.MethodPost, "messages", f.key(), ledger.MessageInput{Target: ledger.DiscussionTarget{ProjectID: f.project.ProjectID, Kind: "task", ID: other.Task.ID}, Kind: "answer", Text: "wrong target reply", ReplyTo: messages[0].ID}, nil); m207Code(err) != errcode.NotFound {
		t.Fatal("cross-object reply accepted", err)
	}
	f.sync()
	before := f.task(f.owner, v.Task.ID)
	inbox := f.inbox(f.reader)
	if !m207InboxHas(inbox, v.Task.ID) {
		t.Fatal("role pool task missing", inbox)
	}
	f.must(f.reader, http.MethodPost, "inbox/read", map[string]any{"project_id": f.project.ProjectID, "through": inbox.Through}, nil)
	read := f.inbox(f.reader)
	if !m207InboxHas(read, v.Task.ID) || slices.ContainsFunc(read.Items, func(i query.InboxItem) bool { return !i.Read }) {
		t.Fatal("read position missing", read)
	}
	if after := f.task(f.owner, v.Task.ID); !reflect.DeepEqual(before, after) {
		t.Fatal("mark-read changed task", before, after)
	}
	var claim tasks.Result
	f.must(f.reader, http.MethodPost, "tasks/claim", tasks.ClaimRequest{ProjectID: f.project.ProjectID, TaskID: v.Task.ID, SeatID: v.Seat.ID, ExpectedRevision: v.Seat.Revision}, &claim)
	a := claim.Attempt
	ar := tasks.AttemptRef{ProjectID: f.project.ProjectID, TaskID: v.Task.ID, AttemptID: a.ID, ExpectedRevision: a.Revision, Fence: a.Fence}
	f.must(f.reader, http.MethodPost, "tasks/block", tasks.BlockRequest{AttemptRef: ar, QuestionMessageID: messages[0].ID}, nil)
	v = f.task(f.owner, v.Task.ID)
	answer := tasks.AnswerRequest{TaskRef: tasks.TaskRef{ProjectID: f.project.ProjectID, TaskID: v.Task.ID, ExpectedRevision: v.Task.Revision}, BlockID: v.Blocks[0].ID, AnswerMessageID: messages[1].ID}
	key := f.key()
	var accepted, replay tasks.Result
	if err := f.call(f.owner, http.MethodPost, "tasks/answer", key, answer, &accepted); err != nil {
		t.Fatal(err)
	}
	if err := f.call(f.owner, http.MethodPost, "tasks/answer", key, answer, &replay); err != nil || !reflect.DeepEqual(accepted, replay) {
		t.Fatal("answer replay", replay, err)
	}
	v = f.task(f.owner, v.Task.ID)
	var attempts []tasks.Attempt
	f.must(f.owner, http.MethodGet, "tasks/"+string(v.Task.ID)+"/attempts", nil, &attempts)
	if v.Task.State != "reconciling" || v.Meta.Completion != nil || len(attempts) != 1 || v.Attempt.ID != a.ID || v.Attempt.State == "active" {
		t.Fatal("answer approved or created a lease", v, attempts)
	}
	ar.ExpectedRevision = v.Attempt.Revision
	if err := f.call(f.reader, http.MethodPost, "tasks/renew", f.key(), ar, nil); m207Code(err) != errcode.LeaseStale {
		t.Fatal("answer revived lease", err)
	}
	f.must(f.owner, http.MethodPost, "tasks/reconcile", tasks.ReconcileRequest{ProjectID: f.project.ProjectID, TaskID: v.Task.ID, AttemptID: a.ID, ExpectedRevision: v.Attempt.Revision, TerminationConfirmed: true, Reason: "synthetic stopped session"}, nil)
	v = f.task(f.owner, v.Task.ID)
	// Reassignment affects pending relevance, not the project-wide task read role.
	f.must(f.owner, http.MethodPost, "tasks/assign", tasks.AssignRequest{TaskRef: tasks.TaskRef{ProjectID: f.project.ProjectID, TaskID: v.Task.ID, ExpectedRevision: v.Task.Revision}, AssigneeID: f.admin.Context.PrincipalID}, nil)
	old := f.inbox(f.reader) // no explicit Sync: the stale inbox candidate must recheck authority
	if m207InboxHas(old, v.Task.ID) {
		t.Fatal("old read item bypassed current assignment", old)
	}
	v = f.task(f.reader, v.Task.ID)
	if err := f.call(f.reader, http.MethodPost, "tasks/claim", f.key(), tasks.ClaimRequest{ProjectID: f.project.ProjectID, TaskID: v.Task.ID, SeatID: v.Seat.ID, ExpectedRevision: v.Seat.Revision}, nil); m207Code(err) != errcode.Forbidden {
		t.Fatal("old item authorized claim", err)
	}
	f.setRole(f.readerSession.Context.PrincipalID, identity.RoleContributor, false)
	if err := f.call(f.reader, http.MethodGet, path, "", nil, nil); m207Code(err) != errcode.NotFound {
		t.Fatal("old discussion bypassed revocation", err)
	}
	if err := f.call(f.reader, http.MethodGet, "tasks/"+string(v.Task.ID), "", nil, nil); m207Code(err) != errcode.NotFound {
		t.Fatal("old task bypassed revocation", err)
	}
	if err := f.call(f.reader, http.MethodGet, "inbox?project_id="+string(f.project.ProjectID), "", nil, nil); m207Code(err) != errcode.NotFound {
		t.Fatal("old inbox bypassed revocation", err)
	}
	f.sync()
	f.sync()
	f.evidence(map[string]any{"message_ids": messages, "read_through": read.ReadThrough, "post_reassignment_inbox": old, "attempts_after_answer": attempts, "post_answer_state": accepted.State, "reassignment_claim_rejected": true, "revoked_discussion_task_inbox_rejected": true})
}

func m207Code(err error) errcode.Code {
	var api *client.APIError
	if errors.As(err, &api) {
		return api.Body.Code
	}
	return errcode.CodeOf(err)
}

func (f *m207) resync(cursor string, limit int) query.ResyncPage {
	f.t.Helper()
	var p query.ResyncPage
	f.must(f.reader, http.MethodGet, fmt.Sprintf("resync?project_id=%s&limit=%d&cursor=%s", f.project.ProjectID, limit, url.QueryEscape(cursor)), nil, &p)
	return p
}
func (f *m207) eventPage(after int64, limit int) query.EventPage {
	f.t.Helper()
	var p query.EventPage
	f.must(f.reader, http.MethodGet, fmt.Sprintf("events?project_id=%s&after=%d&limit=%d", f.project.ProjectID, after, limit), nil, &p)
	return p
}

type m207Set map[query.ObjectRef]query.ObjectState

func (s m207Set) upsert(o query.ObjectState) {
	if old, ok := s[o.Ref]; !ok || o.Revision >= old.Revision {
		s[o.Ref] = o
	}
}
func (s m207Set) sorted() []query.ObjectState {
	out := []query.ObjectState{}
	for _, o := range s {
		out = append(out, o)
	}
	slices.SortFunc(out, func(a, b query.ObjectState) int {
		return strings.Compare(a.Ref.Kind+string(a.Ref.ID), b.Ref.Kind+string(b.Ref.ID))
	})
	return out
}
func (f *m207) snapshot(hook func(query.ResyncPage)) (m207Set, int64) {
	f.t.Helper()
	set := m207Set{}
	cursor := ""
	seen := map[string]bool{}
	var start int64
	for page := 0; page < 100; page++ {
		p := f.resync(cursor, 1)
		if !p.Replace {
			f.t.Fatal("resync omitted replacement", p)
		}
		if page == 0 {
			start = p.ReplayAfter
		} else if start != p.ReplayAfter {
			f.t.Fatal("snapshot start changed", start, p)
		}
		for _, o := range p.Objects {
			if _, ok := set[o.Ref]; ok {
				f.t.Fatal("duplicate snapshot locator", o.Ref)
			}
			set[o.Ref] = o
		}
		if hook != nil {
			hook(p)
		}
		if p.NextCursor == "" {
			return set, start
		}
		if seen[p.NextCursor] {
			f.t.Fatal("snapshot cursor stalled")
		}
		seen[p.NextCursor] = true
		cursor = p.NextCursor
	}
	f.t.Fatal("snapshot did not finish")
	return nil, 0
}
func (f *m207) boundary(name string, mutate func()) {
	f.t.Helper()
	// The client is suspended after receiving a real page. A separate goroutine
	// commits through the live domain/REST interfaces before continuation/replay.
	done := make(chan struct{})
	go func() { defer close(done); mutate() }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		f.t.Fatal("concurrent mutation stalled", name)
	}
	if f.t.Failed() {
		f.t.FailNow()
	}
	f.trace = append(f.trace, map[string]any{"client_boundary": name, "mutation_completed": true})
}
func TestM2ResyncAcceptanceRetainedConcurrentReplace(t *testing.T) {
	f := newM207(t)
	ctx := t.Context()
	bin := buildConsistencyCLI(t)
	cliBytes, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	cliClient := &consistencyClient{bin: bin, origin: f.origin, token: f.readerSession.Token, session: filepath.Join(t.TempDir(), "session.json")}
	saveSession := func() {
		cliClient.token = f.readerSession.Token
		writeJSON(t, cliClient.session, map[string]string{"schema": "lantai.client-session/v1", "origin": f.origin, "token": cliClient.token})
	}
	saveSession()
	readCLI := func(command string, after string, out any) {
		t.Helper()
		args := []string{command, "list", "--project", string(f.project.ProjectID), "--limit", "1"}
		if after != "" {
			args = append(args, "--cursor", after)
		}
		value, code := cliClient.cli(t, args...)
		if code != 0 {
			t.Fatal(command, code, value)
		}
		if err := json.Unmarshal(mustJSON(value), out); err != nil {
			t.Fatal(err)
		}
	}
	who := f.login().Context
	readers, _ := json.Marshal([]ids.ID{who.PrincipalID, f.readerSession.Context.PrincipalID})
	f.sudo(&identity.SetPolicy{ProjectID: f.project.ProjectID, Key: "personal.readers", ExpectedRevision: 0, Value: readers})
	privateRights := *rightsOwned()
	privateRights.Sensitivity = "personal"
	private := f.ingest(who, "resync-private", []byte("synthetic private document"), privateRights)
	keep := f.ingest(who, "resync-kept", []byte("synthetic retained document"), *rightsOwned())
	comment := f.postMessage(f.owner, f.key(), ledger.MessageInput{Target: ledger.DiscussionTarget{ProjectID: f.project.ProjectID, Kind: "version", ID: private.VersionID}, Kind: "question", Text: "synthetic private question"})
	assigned := f.create(f.project.ProjectID, "assigned before snapshot")
	updated := f.create(f.project.ProjectID, "updated during replay")
	f.sync()
	cache, oldAfter := f.snapshot(nil)
	privateRef := query.ObjectRef{ProjectID: f.project.ProjectID, Kind: "version", ID: private.VersionID}
	if _, ok := cache[privateRef]; !ok {
		t.Fatal("private reader lacked initial version")
	}
	// All newly collected events are irrelevant login events. The external REST
	// and independent CLI still return the scanned high-water and no replacement.
	f.login()
	f.sync()
	filtered := f.eventPage(oldAfter, 100)
	if len(filtered.Events) != 0 || filtered.LastSeq <= oldAfter || filtered.ReplaceRequired {
		t.Fatal("filtered watermark did not advance", filtered)
	}
	var filteredCLI query.EventPage
	value, exit := cliClient.cli(t, "events", "list", "--project", string(f.project.ProjectID), "--cursor", strconv.FormatInt(oldAfter, 10), "--limit", "100")
	if exit != 0 {
		t.Fatal(value, exit)
	}
	if err := json.Unmarshal(mustJSON(value), &filteredCLI); err != nil || !reflect.DeepEqual(filtered, filteredCLI) {
		t.Fatal("CLI filtered page differs", filteredCLI, err)
	}
	// Real backup confirmation, consumer commits and audit export bound pruning.
	f.create(f.project.ProjectID, "before backup")
	f.sync()
	if _, err := f.app.Flows.CatchUp(ctx, 1000); err != nil {
		t.Fatal(err)
	}
	backupDir := filepath.Join(t.TempDir(), "backup")
	backup, err := f.app.Backup(ctx, operations.BackupOptions{Destination: backupDir})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = operations.VerifyBackup(ctx, backupDir); err != nil {
		t.Fatal(err)
	}
	if p, err := f.app.Events.Prune(ctx); err != nil || p != 0 {
		t.Fatal("hot records were pruned", p, err)
	}
	f.clk.Advance(events.HotWindow + time.Second)
	pruned, err := f.app.Events.Prune(ctx)
	if err != nil || pruned != backup.Watermarks["events"] || pruned <= oldAfter {
		t.Fatal("formal retention did not expire cursor", pruned, backup.Watermarks, err)
	}
	f.refresh()
	saveSession()
	f.sync()
	if err = f.call(f.reader, http.MethodGet, fmt.Sprintf("events?project_id=%s&after=%d", f.project.ProjectID, oldAfter), "", nil, nil); m207Code(err) != errcode.CursorExpired {
		t.Fatal("REST accepted expired cursor", err)
	}
	expired, exit := cliClient.cli(t, "events", "list", "--project", string(f.project.ProjectID), "--cursor", strconv.FormatInt(oldAfter, 10))
	if exit == 0 || expired["error"].(map[string]any)["code"] != string(errcode.CursorExpired) {
		t.Fatal("CLI accepted expired cursor", exit, expired)
	}
	var firstCLI query.ResyncPage
	readCLI("resync", "", &firstCLI)
	if firstCLI.NextCursor == "" || !firstCLI.Replace {
		t.Fatal("CLI snapshot did not paginate", firstCLI)
	}
	firstREST := f.resync("", 1)
	if !reflect.DeepEqual(firstCLI.Objects, firstREST.Objects) || firstCLI.ReplayAfter != firstREST.ReplayAfter {
		t.Fatal("CLI and REST snapshot scope differ")
	}
	// Cursors are scoped to the authenticated identity/session and project.
	if err = f.call(f.owner, http.MethodGet, "resync?project_id="+string(f.project.ProjectID)+"&cursor="+url.QueryEscape(firstCLI.NextCursor), "", nil, nil); m207Code(err) != errcode.CursorExpired {
		t.Fatal("snapshot cursor crossed identity", err)
	}
	var continued query.ResyncPage
	f.must(f.reader, http.MethodGet, "resync?project_id="+string(f.project.ProjectID)+"&limit=1&cursor="+url.QueryEscape(firstCLI.NextCursor), nil, &continued)
	if continued.ReplayAfter != firstCLI.ReplayAfter {
		t.Fatal("CLI cursor could not continue in REST")
	}
	var addedSnapshot, addedReplay tasks.View
	snapshotMutation := false
	replacement, start := f.snapshot(func(p query.ResyncPage) {
		if snapshotMutation || !slices.ContainsFunc(p.Objects, func(o query.ObjectState) bool { return o.Ref == privateRef }) {
			return
		}
		snapshotMutation = true
		f.boundary("after private version snapshot page", func() {
			v := f.task(f.owner, assigned.Task.ID)
			f.must(f.owner, http.MethodPost, "tasks/assign", tasks.AssignRequest{TaskRef: tasks.TaskRef{ProjectID: f.project.ProjectID, TaskID: v.Task.ID, ExpectedRevision: v.Task.Revision}, AssigneeID: f.admin.Context.PrincipalID}, nil)
			f.sync() // place a visible update before the later scope-change event
			readers, _ := json.Marshal([]ids.ID{f.admin.Context.PrincipalID})
			f.sudo(&identity.SetPolicy{ProjectID: f.project.ProjectID, Key: "personal.readers", ExpectedRevision: 1, Value: readers})
			addedSnapshot = f.create(f.project.ProjectID, "created during snapshot")
			f.sync()
		})
	})
	if !snapshotMutation {
		t.Fatal("snapshot mutation boundary was not reached")
	}
	if _, ok := replacement[privateRef]; !ok {
		t.Fatal("test did not retain the deliberately stale page")
	}
	cache = replacement // whole scope replacement, never a merge into old cached entries
	after := start
	seenEvents := map[ids.ID]bool{}
	duplicates := 0
	replacements := 0
	replayMutation := false
	for page := 0; page < 200; page++ {
		before := after
		p := f.eventPage(after, 2)
		if p.LastSeq < after {
			t.Fatal("scan watermark regressed", p)
		}
		// At-least-once network delivery repeats the same scan. Apply each twice to
		// the model; object revision and event ID dedup prevent repeated effects.
		repeated := f.eventPage(after, 2)
		if !reflect.DeepEqual(p, repeated) {
			t.Fatal("same event window changed without writes")
		}
		if !replayMutation && p.LastSeq > after {
			replayMutation = true
			f.boundary("after first replay page before client apply", func() {
				v := f.task(f.owner, updated.Task.ID)
				f.must(f.owner, http.MethodPost, "tasks/cancel", tasks.CancelRequest{TaskRef: tasks.TaskRef{ProjectID: f.project.ProjectID, TaskID: v.Task.ID, ExpectedRevision: v.Task.Revision}, Reason: "synthetic replay concurrency"}, nil)
				addedReplay = f.create(f.project.ProjectID, "created during replay")
				f.sync()
			})
		}
		for i := 0; i < 2; i++ {
			for _, ev := range p.Events {
				if seenEvents[ev.EventID] {
					duplicates++
					continue
				}
				seenEvents[ev.EventID] = true
				cache.upsert(ev.Object)
			}
		}
		after = p.LastSeq
		if p.ReplaceRequired {
			replacements++
			cache, after = f.snapshot(nil)
			// Snapshot starts after every already observed mutation; earlier watermark
			// is not retained and partial pages are never merged into the replacement.
		}
		high, err := f.app.Events.HighWater(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if after == high && before == after && !p.ReplaceRequired {
			break
		}
		if page == 199 {
			t.Fatal("client did not converge")
		}
	}
	if !replayMutation || replacements == 0 || duplicates == 0 {
		t.Fatal("replay/replace/dedup boundary missing", replayMutation, replacements, duplicates)
	}
	authoritative, finalStart := f.snapshot(nil)
	if !reflect.DeepEqual(cache, authoritative) || after != finalStart {
		t.Fatal("final client set differs", cache.sorted(), authoritative.sorted(), after, finalStart)
	}
	for _, id := range []ids.ID{private.AssetID, private.VersionID, comment.ID} {
		if slices.ContainsFunc(cache.sorted(), func(o query.ObjectState) bool { return o.Ref.ID == id }) {
			t.Fatal("revoked object remained in client set", id)
		}
	}
	for _, ref := range []query.ObjectRef{{ProjectID: f.project.ProjectID, Kind: "version", ID: keep.VersionID}, {ProjectID: f.project.ProjectID, Kind: "task", ID: addedSnapshot.Task.ID}, {ProjectID: f.project.ProjectID, Kind: "task", ID: addedReplay.Task.ID}} {
		if _, ok := cache[ref]; !ok {
			t.Fatal("visible concurrent object lost", ref)
		}
	}
	changed := cache[query.ObjectRef{ProjectID: f.project.ProjectID, Kind: "task", ID: updated.Task.ID}]
	reassigned := cache[query.ObjectRef{ProjectID: f.project.ProjectID, Kind: "task", ID: assigned.Task.ID}]
	if changed.State != "cancelled" || reassigned.Relevant {
		t.Fatal("concurrent task update missed", changed, reassigned)
	}
	// Reassignment preserves the task under the project read role while removing
	// it from this identity's pending inbox; personal revocation removes content.
	if m207InboxHas(f.inbox(f.reader), assigned.Task.ID) {
		t.Fatal("reassigned task still pending")
	}
	if err = f.call(f.reader, http.MethodGet, "assets/"+string(private.AssetID)+"/versions/"+string(private.VersionID), "", nil, nil); m207Code(err) != errcode.NotFound {
		t.Fatal("revoked exact version remained readable", err)
	}
	if err = f.call(f.reader, http.MethodGet, "messages?project_id="+string(f.project.ProjectID)+"&kind=version&id="+string(private.VersionID), "", nil, nil); m207Code(err) != errcode.UseRestricted {
		t.Fatal("private object discussion remained readable", err)
	}
	metrics, err := f.app.Events.Metrics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f.evidence(map[string]any{"cli_sha256": shaOf(cliBytes), "cli_filtered_page": filteredCLI, "cli_expired_response": expired, "cli_snapshot_start": firstCLI, "old_after": oldAfter, "filtered_page": filtered, "backup_watermarks": backup.Watermarks, "pruned_through": pruned, "snapshot_start": start, "final_after": after, "replacement_rounds": replacements, "duplicate_deliveries": duplicates, "client_objects": cache.sorted(), "authoritative_objects": authoritative.sorted(), "retention_metrics": metrics, "snapshot_added": addedSnapshot.Task.ID, "replay_added": addedReplay.Task.ID, "removed_objects": []ids.ID{private.AssetID, private.VersionID, comment.ID}})
}

func TestM2ResyncAcceptanceFixedContextAndDecision(t *testing.T) {
	af := newAppFlow(t)
	ctx := t.Context()
	servers := remoteServers(t, af.app, true)
	f := &m207{env: af.env, app: af.app, origin: "http://" + servers.Addresses.API}
	f.owner = f.connect(f.login())
	producer, err := af.app.Extensions.Producer(ctx, "org.lantai.corecheck.manifest")
	if err != nil {
		t.Fatal(err)
	}
	// Only the acceptance profile is a pre-approved configuration fixture.
	// Every context/decision below uses real tasks, corecheck processes, QA and
	// HumanGrant approval/publication through the assembled application's owners.
	profile := af.profile("profiles/resync-context", []manifest.AssetType{manifest.TypeDoc}, producer)
	definition := af.definition("flows/resync-context", "org.lantai.corecheck.manifest")
	bundle := func() catalog.ContextBundle {
		var b catalog.ContextBundle
		f.must(f.owner, http.MethodGet, "context?project_id="+string(f.project.ProjectID)+"&asset_type=doc", nil, &b)
		return b
	}
	create := func(key, title string) tasks.View {
		var r tasks.Result
		if err := f.call(f.owner, http.MethodPost, "tasks", key, tasks.CreateRequest{ProjectID: f.project.ProjectID, Type: "question", Title: title, AcceptanceCriteria: []string{"use fixed context and decision"}, Role: identity.RoleContributor, ContextAssetType: "doc"}, &r); err != nil {
			t.Fatal(err)
		}
		return f.task(f.owner, r.TaskID)
	}
	refs := func(b catalog.ContextBundle) []ids.PermanentRef {
		out := []ids.PermanentRef{}
		for _, d := range b.Documents {
			out = append(out, d.Ref)
		}
		return out
	}
	checks := []jobs.Job{}
	approve := func(kind, slug string, supersedes []ids.PermanentRef, draft func(catalog.VersionResult)) catalog.VersionResult {
		var v catalog.VersionResult
		_, job := af.review(profile, definition, "org.lantai.corecheck.manifest", "doc", func(task ids.ID, a tasks.Attempt) catalog.VersionResult {
			data := []byte("# " + kind + " " + slug + "\n\nSynthetic approved instructions.\n")
			spec := catalog.ProjectDocument{Contract: catalog.ProjectDocumentContract, Kind: kind, Title: slug, Scope: catalog.DocumentScope{Kind: "project"}, FilePath: "rules.md", Supersedes: supersedes}
			req := catalog.VersionRequest{Who: af.maker, IdempotencyKey: f.key(), UploadID: f.upload(af.maker, data), Slug: slug, Task: bindTo(task, a), Content: catalog.ContentInput{AssetType: manifest.TypeDoc, Rights: rightsOwned(), Files: []manifest.InputFile{{Path: "rules.md", Role: "primary", SHA256: shaOf(data), Size: int64(len(data))}}}}
			var err error
			v, err = f.catalog.CommitProjectDocument(ctx, req, spec)
			if err != nil {
				t.Fatal(err)
			}
			if draft != nil {
				draft(v)
			}
			return v
		})
		if job.State != "succeeded" || len(job.Checks) != 4 {
			t.Fatal("real corecheck evidence missing", job)
		}
		checks = append(checks, job)
		return v
	}
	firstRules := approve("context", "resync-rules", []ids.PermanentRef{}, nil)
	firstDecision := approve("decision", "resync-decision", []ids.PermanentRef{}, nil)
	before := bundle()
	if len(before.Documents) != 2 {
		t.Fatal("approved context/decision missing", before)
	}
	originalKey := f.key()
	fixed := create(originalKey, "fixed initial instructions")
	if fixed.Meta.Context == nil || !slices.Equal(fixed.Meta.Context.Refs, refs(before)) || fixed.Meta.Context.Digest != before.Digest {
		t.Fatal("initial context not fixed", fixed.Meta.Context, before)
	}
	draftTasks := []tasks.View{}
	secondRules := approve("context", "resync-rules-next", []ids.PermanentRef{firstRules.Ref}, func(v catalog.VersionResult) {
		current := bundle()
		if current.Digest != before.Digest || slices.Contains(refs(current), v.Ref) {
			t.Fatal("unapproved rules superseded effective instructions", current)
		}
		pending := create(f.key(), "created while rules replacement is unapproved")
		if !slices.Equal(pending.Meta.Context.Refs, refs(before)) {
			t.Fatal("draft entered new task", pending.Meta.Context)
		}
		draftTasks = append(draftTasks, pending)
	})
	afterRules := bundle()
	secondDecision := approve("decision", "resync-decision-next", []ids.PermanentRef{firstDecision.Ref}, func(v catalog.VersionResult) {
		current := bundle()
		if current.Digest != afterRules.Digest || slices.Contains(refs(current), v.Ref) {
			t.Fatal("unapproved decision superseded effective instructions", current)
		}
		pending := create(f.key(), "created while decision replacement is unapproved")
		if !slices.Equal(pending.Meta.Context.Refs, refs(afterRules)) {
			t.Fatal("draft decision entered new task", pending.Meta.Context)
		}
		draftTasks = append(draftTasks, pending)
	})
	after := bundle()
	want := []ids.PermanentRef{secondRules.Ref, secondDecision.Ref}
	if len(after.Documents) != 2 || !slices.Contains(refs(after), want[0]) || !slices.Contains(refs(after), want[1]) {
		t.Fatal("approved supersession missing", after)
	}
	newer := create(f.key(), "fixed replacement instructions")
	if newer.Meta.Context.Digest != after.Digest || !slices.Equal(newer.Meta.Context.Refs, refs(after)) {
		t.Fatal("new task fixed old instructions", newer.Meta.Context)
	}
	for _, old := range append(draftTasks, fixed) {
		again := f.task(f.owner, old.Task.ID)
		if !reflect.DeepEqual(again.Meta.Context, old.Meta.Context) {
			t.Fatal("historical fixed context changed", old.Meta.Context, again.Meta.Context)
		}
	}
	replay := create(originalKey, "fixed initial instructions")
	if replay.Task.ID != fixed.Task.ID || !reflect.DeepEqual(replay.Meta.Context, fixed.Meta.Context) {
		t.Fatal("task replay changed fixed context", replay)
	}
	history := []catalog.ContextDocument{}
	for _, ref := range []ids.PermanentRef{firstRules.Ref, firstDecision.Ref} {
		doc, err := f.catalog.GetContextDocument(ctx, f.login().Context, ref, af.app.Reviews)
		if err != nil || doc.Approval == nil || doc.Ref != ref {
			t.Fatal("superseded approval history lost", doc, err)
		}
		history = append(history, doc)
	}
	f.evidence(map[string]any{"initial_bundle": before, "replacement_bundle": after, "original_task_context": fixed.Meta.Context, "new_task_context": newer.Meta.Context, "draft_task_contexts": []*tasks.ContextSnapshot{draftTasks[0].Meta.Context, draftTasks[1].Meta.Context}, "historical_documents": history, "corecheck_jobs": checks, "task_replay": replay.Task.ID})
}
