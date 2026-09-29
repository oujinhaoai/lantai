package integration

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/event"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	"github.com/oujinhaoai/lantai/internal/events"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/ledger"
	"github.com/oujinhaoai/lantai/internal/provenance"
	"github.com/oujinhaoai/lantai/internal/query"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

func collaborationForTest(t *testing.T, e *env, reviews *ledger.Reviews, life *ledger.Lifecycle, tasks query.CollaborationTasks) (*events.Store, *query.Collaboration) {
	t.Helper()
	access, err := e.ledger.NewDiscussionObjects(e.catalog, e.rights, e.id, nil)
	if err != nil {
		t.Fatal(err)
	}
	source, err := e.ledger.NewCollaborationSource(reviews, life, access)
	if err != nil {
		t.Fatal(err)
	}
	objects, err := query.NewCoreCollaborationObjects(source, e.id, tasks)
	if err != nil {
		t.Fatal(err)
	}
	log, q := e.eventQuery()
	c, err := q.NewCollaboration(e.inst.DB(ownership.Runtime), objects)
	if err != nil {
		t.Fatal(err)
	}
	return log, c
}

// This fixture owns only the explicitly unavailable T05 current assignment port.
// Ledger, provenance, identity, events, inbox SQL and resync are real modules.
type collaborationTaskFixture struct {
	state    query.ObjectState
	assignee ids.ID
}

func (f *collaborationTaskFixture) Current(_ context.Context, _ authz.Context, ref query.ObjectRef) (query.ObjectState, error) {
	if ref != f.state.Ref {
		return query.ObjectState{}, errcode.New(errcode.NotFound, "")
	}
	return f.state, nil
}
func (f *collaborationTaskFixture) Refs(_ context.Context, project, after ids.ID, limit int) ([]ids.ID, error) {
	if f.state.Ref.ProjectID == project && f.state.Ref.ID > after {
		return []ids.ID{f.state.Ref.ID}, nil
	}
	return nil, nil
}
func (f *collaborationTaskFixture) Recipients(_ context.Context, ref query.ObjectRef) ([]ids.ID, error) {
	if ref != f.state.Ref {
		return nil, errcode.New(errcode.NotFound, "")
	}
	return []ids.ID{f.assignee}, nil
}
func collectTaskEvent(t *testing.T, e *env, log *events.Store, ref query.ObjectRef, revision int64) commands.OutboxRecord {
	t.Helper()
	ev, err := event.New(&ids.Generator{Clock: e.clk, Rand: rand.Reader}, e.clk, event.Params{EventType: "task.assigned", SchemaVersion: 1, AggregateType: "task", AggregateID: ref.ID, AggregateRevision: revision, ActorID: e.admin.Context.PrincipalID, ProjectID: ref.ProjectID, OperationID: ids.New(), Payload: map[string]any{"old_assignee": "do not use payload recipients"}})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := ev.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	record := commands.OutboxRecord{EventID: ev.EventID, OperationID: ev.OperationID, Envelope: raw}
	if _, err = log.Collect(t.Context(), []commands.OutboxRecord{record}); err != nil {
		t.Fatal(err)
	}
	return record
}
func TestM2CollaborationCurrentAuthorityAndRebuild(t *testing.T) {
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	ctx := t.Context()
	e.setRole(e.admin.Context.PrincipalID, identity.RoleOwner, true)
	who := e.login().Context
	viewer, session := e.agent("collaboration-viewer@node", identity.RoleViewer)
	e.setRole(viewer.ID, identity.RoleReviewer, true)
	v := e.ingest(who, "collaboration-resource", []byte("synthetic collaboration document"), *rightsOwned())
	access, err := e.ledger.NewDiscussionObjects(e.catalog, e.rights, e.id, nil)
	if err != nil {
		t.Fatal(err)
	}
	target := ledger.DiscussionTarget{ProjectID: e.project.ProjectID, Kind: "version", ID: v.VersionID}
	direct, err := e.ledger.PostMessage(ctx, who, e.key(), ledger.MessageInput{Target: target, Kind: "question", Text: "private body must not enter notifications", Mentions: []ledger.Mention{{PrincipalID: viewer.ID}}}, access)
	if err != nil {
		t.Fatal(err)
	}
	role, err := e.ledger.PostMessage(ctx, who, e.key(), ledger.MessageInput{Target: target, Kind: "handoff", Text: "role mention", Mentions: []ledger.Mention{{Role: "reviewer"}}}, access)
	if err != nil {
		t.Fatal(err)
	}
	task := &collaborationTaskFixture{state: query.ObjectState{Ref: query.ObjectRef{ProjectID: e.project.ProjectID, Kind: "task", ID: ids.New()}, Title: "Current task", State: "working", Revision: 3, Priority: 1}, assignee: viewer.ID}
	log, c := collaborationForTest(t, e, nil, nil, task)
	e.relay(log)
	record := collectTaskEvent(t, e, log, task.state.Ref, 3)
	if _, err = c.CatchUpInbox(ctx, 1000); err != nil {
		t.Fatal(err)
	}
	if n, err := log.Collect(ctx, []commands.OutboxRecord{record}); err != nil || n != 0 {
		t.Fatal("duplicate event", n, err)
	}
	if n, err := c.CatchUpInbox(ctx, 1000); err != nil || n != 0 {
		t.Fatal(n, err)
	}
	page, err := c.Inbox(ctx, session.Context, e.project.ProjectID)
	if err != nil || len(page.Items) != 3 {
		t.Fatal(page, err)
	}
	if err = c.MarkRead(ctx, session.Context, e.project.ProjectID, page.Through); err != nil {
		t.Fatal(err)
	}
	e.setRole(viewer.ID, identity.RoleReviewer, false)
	page, err = c.Inbox(ctx, session.Context, e.project.ProjectID)
	if err != nil || len(page.Items) != 2 {
		t.Fatal(page, err)
	}
	for _, item := range page.Items {
		if !item.Read || item.Object.Ref.ID == role.ID {
			t.Fatal("old role inbox survived", item)
		}
	}
	evidence := rightsEvidence(t, e, who, v, false)
	personal := "personal"
	if _, err = e.rights.ApplyAssertion(ctx, who, e.key(), provenance.AssertionRequest{Subject: v.Ref, ManifestDigest: v.ManifestDigest, ExpectedRevision: 1, Kind: "restrict", Fields: provenance.AssertionFields{Sensitivity: &personal}, EvidenceIDs: []ids.ID{evidence.RecordID}, Reason: "synthetic privacy restriction"}); err != nil {
		t.Fatal(err)
	}
	page, err = c.Inbox(ctx, session.Context, e.project.ProjectID)
	if err != nil || len(page.Items) != 1 || page.Items[0].Object.Ref != task.state.Ref {
		t.Fatal("stale private notification", page, err)
	}
	task.assignee = who.PrincipalID
	task.state.Revision = 4
	page, err = c.Inbox(ctx, session.Context, e.project.ProjectID)
	if err != nil || len(page.Items) != 0 {
		t.Fatal("reassigned task leaked", page, err)
	}
	collectTaskEvent(t, e, log, task.state.Ref, 1) // late old revision routes using current assignment
	if _, err = c.CatchUpInbox(ctx, 1000); err != nil {
		t.Fatal(err)
	}
	owner, err := c.Inbox(ctx, who, e.project.ProjectID)
	if err != nil || len(owner.Items) != 1 || owner.Items[0].Object.Revision != 4 {
		t.Fatal(owner, err)
	}
	eventsPage, err := c.Events(ctx, query.EventRequest{Who: session.Context, ProjectID: e.project.ProjectID, Limit: 1000})
	if err != nil || !eventsPage.ReplaceRequired {
		t.Fatal(eventsPage, err)
	}
	for _, event := range eventsPage.Events {
		if slices.Contains([]ids.ID{direct.ID, role.ID, v.VersionID}, event.Object.Ref.ID) {
			t.Fatal("private event survived", event)
		}
	}
	cursor := ""
	seen := map[query.ObjectRef]bool{}
	var boundary int64
	for {
		snapshot, err := c.Resync(ctx, session.Context, e.project.ProjectID, cursor, 1)
		if err != nil {
			t.Fatal(err)
		}
		if cursor != "" && snapshot.ReplayAfter != boundary {
			t.Fatal("snapshot boundary changed")
		}
		boundary = snapshot.ReplayAfter
		for _, object := range snapshot.Objects {
			if seen[object.Ref] {
				t.Fatal("duplicate resync object")
			}
			seen[object.Ref] = true
			if slices.Contains([]ids.ID{direct.ID, role.ID, v.VersionID, v.AssetID}, object.Ref.ID) {
				t.Fatal("private resync object", object)
			}
		}
		if snapshot.NextCursor == "" {
			break
		}
		cursor = snapshot.NextCursor
	}
	if !seen[task.state.Ref] {
		t.Fatal("task omitted from merged snapshot")
	}
	mctx, held, err := e.inst.Gate().Maintain(ctx, "maintenance")
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.RebuildInbox(mctx)
	held.Release()
	e.inst.Gate().Open()
	if err != nil {
		t.Fatal(err)
	}
	owner, err = c.Inbox(ctx, who, e.project.ProjectID)
	if err != nil || len(owner.Items) != 1 || owner.Items[0].Object.Ref != task.state.Ref {
		t.Fatal(owner, err)
	}
	page, err = c.Inbox(ctx, session.Context, e.project.ProjectID)
	if err != nil || len(page.Items) != 0 || page.ReadThrough == 0 {
		t.Fatal("rebuild lost current filtering/read watermark", page, err)
	}
	raw, _ := json.Marshal(eventsPage)
	if strings.Contains(string(raw), "private body must not enter notifications") {
		t.Fatal("message body leaked")
	}
	e.setRole(viewer.ID, identity.RoleViewer, false)
	if _, err = c.Inbox(ctx, session.Context, e.project.ProjectID); err == nil {
		t.Fatal("revoked project still readable")
	}
}
