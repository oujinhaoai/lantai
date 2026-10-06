package query

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/event"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	"github.com/oujinhaoai/lantai/internal/events"
	"github.com/oujinhaoai/lantai/internal/platform/sqlite"
	"github.com/oujinhaoai/lantai/internal/platform/sqlite/migrations"
)

type collaborationObjects struct {
	objects    map[ObjectRef]ObjectState
	recipients map[ObjectRef][]ids.ID
	denied     map[ObjectRef]bool
	onScan     func()
	fail       error
}

func (o *collaborationObjects) ResolveEvent(_ context.Context, e event.Envelope) (ObjectRef, bool, error) {
	r := ObjectRef{ProjectID: e.ProjectID, Kind: e.AggregateType, ID: e.AggregateID}
	return r, r.valid(), o.fail
}
func (o *collaborationObjects) Current(_ context.Context, who authz.Context, ref ObjectRef) (ObjectState, error) {
	if o.fail != nil {
		return ObjectState{}, o.fail
	}
	s, ok := o.objects[ref]
	if !ok || o.denied[ref] {
		return s, errcode.New(errcode.NotFound, "")
	}
	s.Relevant = slices.Contains(o.recipients[ref], who.PrincipalID)
	return s, nil
}
func (o *collaborationObjects) Recipients(_ context.Context, _ event.Envelope, ref ObjectRef) ([]ids.ID, error) {
	return o.recipients[ref], o.fail
}
func (o *collaborationObjects) Snapshot(_ context.Context, _ authz.Context, project ids.ID, after string, limit int) ([]ObjectState, string, error) {
	if o.onScan != nil {
		fn := o.onScan
		o.onScan = nil
		fn()
	}
	var all []ObjectState
	for r, s := range o.objects {
		if r.ProjectID == project && string(r.ID) > after {
			all = append(all, s)
		}
	}
	slices.SortFunc(all, func(a, b ObjectState) int { return strings.Compare(string(a.Ref.ID), string(b.Ref.ID)) })
	next := ""
	if len(all) > limit {
		all = all[:limit]
		next = string(all[len(all)-1].Ref.ID)
	}
	return all, next, o.fail
}
func collaborationFixture(t *testing.T) (*fixture, *Collaboration, *collaborationObjects) {
	t.Helper()
	f := setup(t)
	f.az.Grant(f.who.PrincipalID, f.project, "events.read")
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
	obj := &collaborationObjects{objects: map[ObjectRef]ObjectState{}, recipients: map[ObjectRef][]ids.ID{}, denied: map[ObjectRef]bool{}}
	c, err := f.s.NewCollaboration(db, obj)
	if err != nil {
		t.Fatal(err)
	}
	return f, c, obj
}
func addCollaborationEvent(f *fixture, o *collaborationObjects, kind string, revision int64) ObjectRef {
	ref := ObjectRef{ProjectID: f.project, Kind: kind, ID: ids.New()}
	o.objects[ref] = ObjectState{Ref: ref, Revision: revision, Title: "Current title", State: "submitted", Priority: 1}
	o.recipients[ref] = []ids.ID{f.who.PrincipalID}
	appendCollaborationEvent(f, ref, revision)
	return ref
}
func appendCollaborationEvent(f *fixture, ref ObjectRef, revision int64) {
	f.log.entries = append(f.log.entries, events.Entry{GlobalSeq: int64(len(f.log.entries) + 1), Envelope: event.Envelope{EventID: ids.New(), ProjectID: ref.ProjectID, EventType: "review.recorded", AggregateType: ref.Kind, AggregateID: ref.ID, AggregateRevision: revision, Payload: []byte(`{"secret":"must not leave server"}`)}})
}
func TestCollaborationEventsFilterHighWaterAndResync(t *testing.T) {
	f, c, o := collaborationFixture(t)
	ctx := t.Context()
	ref := addCollaborationEvent(f, o, "version", 1)
	o.denied[ref] = true
	page, err := c.Events(ctx, EventRequest{Who: f.who, ProjectID: f.project, Wait: time.Second})
	if err != nil || page.LastSeq != 1 || len(page.Events) != 0 || !page.ReplaceRequired {
		t.Fatal(page, err)
	}
	delete(o.denied, ref)
	addCollaborationEvent(f, o, "task", 1)
	f.log.pruned = 1
	if _, err = c.Events(ctx, EventRequest{Who: f.who, ProjectID: f.project}); errcode.CodeOf(err) != errcode.CursorExpired {
		t.Fatal(err)
	}
	// An event collected during enumeration must be replayable after S.
	o.onScan = func() { appendCollaborationEvent(f, ref, 2); v := o.objects[ref]; v.Revision = 2; o.objects[ref] = v }
	snapshot, err := c.Resync(ctx, f.who, f.project, "", 1)
	if err != nil || snapshot.ReplayAfter != 2 || snapshot.NextCursor == "" || !snapshot.Replace {
		t.Fatal(snapshot, err)
	}
	last, err := c.Resync(ctx, f.who, f.project, snapshot.NextCursor, 1)
	if err != nil || last.ReplayAfter != 2 || last.NextCursor != "" {
		t.Fatal(last, err)
	}
	replay, err := c.Events(ctx, EventRequest{Who: f.who, ProjectID: f.project, After: last.ReplayAfter})
	if err != nil || len(replay.Events) != 1 || replay.Events[0].Object.Revision != 2 {
		t.Fatal(replay, err)
	}
	other := f.who
	other.SessionID = ids.New()
	if _, err = c.Resync(ctx, other, f.project, snapshot.NextCursor, 1); err == nil {
		t.Fatal("cursor accepted by another session")
	}
	f.log.pruned = 3
	if _, err = c.Resync(ctx, f.who, f.project, snapshot.NextCursor, 1); errcode.CodeOf(err) != errcode.CursorExpired {
		t.Fatal(err)
	}
}

func TestProjectControlEventReplacesVisibleResourceScope(t *testing.T) {
	f, c, o := collaborationFixture(t)
	ref := addCollaborationEvent(f, o, "project", 2)
	f.log.entries[0].Envelope.EventType = "ledger.control_changed"
	f.log.entries[0].Envelope.SchemaVersion = 1
	state := o.objects[ref]
	state.State = "archived"
	o.objects[ref] = state
	page, err := c.Events(t.Context(), EventRequest{Who: f.who, ProjectID: f.project})
	if err != nil || !page.ReplaceRequired || page.LastSeq != 1 || len(page.Events) != 1 || page.Events[0].Object.State != "archived" {
		t.Fatal("visible project event did not invalidate child scope", page, err)
	}
	changes, err := f.s.Changes(t.Context(), f.who, 0, 10)
	if err != nil || !changes.ResyncRequired || changes.HighWater != 1 {
		t.Fatal("legacy change reader retained archived project children", changes, err)
	}
}

// Policy and membership changes alter the visible scope without an object event,
// so they require a replacement; unrelated identity events and other projects'
// policies do not (BUG-20261001-04).
func TestCollaborationScopeChangesRequireReplacement(t *testing.T) {
	for _, c := range []struct {
		name, typ string
		project   bool
		want      bool
	}{
		{"project policy", "project.policy_set", true, true},
		{"role granted", "project.role_granted", true, true},
		{"role revoked", "project.role_revoked", true, true},
		{"system policy", "policy.system_policy_set", false, true},
		{"other project policy", "project.policy_set", false, false},
		{"session started", "session.started", false, false},
		{"credential issued", "principal.credential_issued", false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, coll, _ := collaborationFixture(t)
			e := event.Envelope{EventID: ids.New(), EventType: c.typ, SchemaVersion: 1, AggregateType: "project", AggregateID: ids.New(), Payload: []byte(`{}`)}
			if c.project {
				e.ProjectID, e.AggregateID = f.project, f.project
			} else if c.typ == "project.policy_set" {
				e.ProjectID = ids.New()
				e.AggregateID = e.ProjectID
			}
			f.log.entries = append(f.log.entries, events.Entry{GlobalSeq: 1, Envelope: e})
			page, err := coll.Events(t.Context(), EventRequest{Who: f.who, ProjectID: f.project})
			if err != nil || page.LastSeq != 1 || len(page.Events) != 0 || page.ReplaceRequired != c.want {
				t.Fatal(page, err)
			}
		})
	}
}
func TestCollaborationInboxDedupReassignmentAndReadPosition(t *testing.T) {
	f, c, o := collaborationFixture(t)
	ctx := t.Context()
	ref := addCollaborationEvent(f, o, "task", 3)
	appendCollaborationEvent(f, ref, 1) // Delayed old event must never replace current business state.
	n, err := c.CatchUpInbox(ctx, 100)
	if err != nil || n != 2 {
		t.Fatal(n, err)
	}
	if n, err = c.CatchUpInbox(ctx, 100); err != nil || n != 0 {
		t.Fatal(n, err)
	}
	inbox, err := c.Inbox(ctx, f.who, f.project)
	if err != nil || len(inbox.Items) != 1 || inbox.Items[0].Object.Revision != 3 || inbox.Items[0].Read {
		t.Fatal(inbox, err)
	}
	if err = c.MarkRead(ctx, f.who, f.project, 3); err == nil {
		t.Fatal("future read position")
	}
	if err = c.MarkRead(ctx, f.who, f.project, 2); err != nil {
		t.Fatal(err)
	}
	if err = c.MarkRead(ctx, f.who, f.project, 1); err != nil {
		t.Fatal(err)
	}
	inbox, err = c.Inbox(ctx, f.who, f.project)
	if err != nil || !inbox.Items[0].Read || inbox.ReadThrough != 2 {
		t.Fatal(inbox, err)
	}
	// Assignment changes before an event is delivered: object still readable but no longer in inbox.
	o.recipients[ref] = nil
	inbox, err = c.Inbox(ctx, f.who, f.project)
	if err != nil || len(inbox.Items) != 0 {
		t.Fatal(inbox, err)
	}
	o.recipients[ref] = []ids.ID{f.who.PrincipalID}
	o.denied[ref] = true
	inbox, err = c.Inbox(ctx, f.who, f.project)
	if err != nil || len(inbox.Items) != 0 {
		t.Fatal(inbox, err)
	}
	delete(o.denied, ref)
	delete(o.objects, ref)
	inbox, err = c.Inbox(ctx, f.who, f.project)
	if err != nil || len(inbox.Items) != 0 {
		t.Fatal(inbox, err)
	}
}
func TestCollaborationLongPollCancellationAndRevocation(t *testing.T) {
	f, c, _ := collaborationFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := c.Events(ctx, EventRequest{Who: f.who, ProjectID: f.project, Wait: time.Second}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := c.Events(t.Context(), EventRequest{Who: f.who, ProjectID: f.project, Wait: 2 * time.Second})
		done <- err
	}()
	// Static authority serializes revocations. Poll must not hold security guard while idle.
	f.az.Revoke(f.who.PrincipalID, f.project)
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("revocation missed")
		}
	case <-time.After(time.Second):
		t.Fatal("poll did not reauthorize")
	}
}

func (o *collaborationObjects) InboxSnapshot(context.Context) ([]InboxSeed, error) {
	if o.fail != nil {
		return nil, o.fail
	}
	var out []InboxSeed
	for ref, principals := range o.recipients {
		if _, ok := o.objects[ref]; !ok {
			continue
		}
		for _, principal := range principals {
			out = append(out, InboxSeed{principal, ref})
		}
	}
	if o.onScan != nil {
		fn := o.onScan
		o.onScan = nil
		fn()
	}
	return out, nil
}
func TestCollaborationInboxRecoveryFromExpiredLog(t *testing.T) {
	f, c, o := collaborationFixture(t)
	ctx := t.Context()
	ref := addCollaborationEvent(f, o, "task", 1)
	f.log.pruned = 1
	if _, err := c.CatchUpInbox(ctx, 100); errcode.CodeOf(err) != errcode.CursorExpired {
		t.Fatal(err)
	}
	if _, err := c.RebuildInbox(ctx); err == nil {
		t.Fatal("rebuild outside maintenance")
	}
	o.onScan = func() { appendCollaborationEvent(f, ref, 2) }
	mctx, h, err := f.gate.Maintain(ctx, "maintenance")
	if err != nil {
		t.Fatal(err)
	}
	start, err := c.RebuildInbox(mctx)
	h.Release()
	f.gate.Open()
	if err != nil || start != 1 {
		t.Fatal(start, err)
	}
	if n, err := c.CatchUpInbox(ctx, 100); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	inbox, err := c.Inbox(ctx, f.who, f.project)
	if err != nil || len(inbox.Items) != 1 || inbox.Through != 2 {
		t.Fatal(inbox, err)
	}
}

func TestCollaborationRebuildRollsBackProjectionAndOffset(t *testing.T) {
	f, c, o := collaborationFixture(t)
	ctx := t.Context()
	ref := addCollaborationEvent(f, o, "task", 1)
	if _, err := c.CatchUpInbox(ctx, 100); err != nil {
		t.Fatal(err)
	}
	if err := c.MarkRead(ctx, f.who, f.project, 1); err != nil {
		t.Fatal(err)
	}
	appendCollaborationEvent(f, ref, 2)
	if _, err := c.runtime.ExecContext(ctx, `CREATE TRIGGER fail_inbox_rebuild BEFORE INSERT ON query_inbox_items BEGIN SELECT RAISE(ABORT,'synthetic rebuild failure'); END`); err != nil {
		t.Fatal(err)
	}
	mctx, h, err := f.gate.Maintain(ctx, "maintenance")
	if err != nil {
		t.Fatal(err)
	}
	_, rebuildErr := c.RebuildInbox(mctx)
	h.Release()
	f.gate.Open()
	if rebuildErr == nil {
		t.Fatal("injected rebuild unexpectedly succeeded")
	}
	page, err := c.Inbox(ctx, f.who, f.project)
	if err != nil || len(page.Items) != 1 || page.Through != 1 || page.ReadThrough != 1 || !page.Items[0].Read {
		t.Fatal("partial rebuild leaked", page, err)
	}
	if _, err = c.runtime.ExecContext(ctx, `DROP TRIGGER fail_inbox_rebuild`); err != nil {
		t.Fatal(err)
	}
	mctx, h, err = f.gate.Maintain(ctx, "maintenance")
	if err != nil {
		t.Fatal(err)
	}
	through, err := c.RebuildInbox(mctx)
	h.Release()
	f.gate.Open()
	if err != nil || through != 2 {
		t.Fatal(through, err)
	}
	page, err = c.Inbox(ctx, f.who, f.project)
	if err != nil || len(page.Items) != 1 || page.Through != 2 || page.ReadThrough != 1 {
		t.Fatal(page, err)
	}
}

func TestCollaborationPreparationFailurePersistsAcrossRestart(t *testing.T) {
	f, c, o := collaborationFixture(t)
	ctx := t.Context()
	addCollaborationEvent(f, o, "trash", 1)
	o.fail = errcode.New(errcode.RefMismatch, "synthetic invalid authority routing")
	if _, err := c.CatchUpInbox(ctx, 100); err == nil {
		t.Fatal("routing failure ignored")
	}
	metrics, err := c.InboxMetrics(ctx)
	if err != nil || metrics.Through != 0 || metrics.Failures != 1 {
		t.Fatal(metrics, err)
	}
	restarted, err := f.s.NewCollaboration(c.runtime, o)
	if err != nil {
		t.Fatal(err)
	}
	o.fail = nil
	if _, err = restarted.CatchUpInbox(ctx, 100); !errors.Is(err, events.ErrBlocked) {
		t.Fatal("failure lost on restart", err)
	}
	id := f.log.entries[0].Envelope.EventID
	if err = restarted.RetryInboxEvent(ctx, id); err == nil {
		t.Fatal("retry outside maintenance")
	}
	if _, err = restarted.InboxFailures(ctx, 0, 100); err == nil {
		t.Fatal("failure identifiers exposed outside maintenance")
	}
	mctx, h, err := f.gate.Maintain(ctx, "maintenance")
	if err != nil {
		t.Fatal(err)
	}
	failures, err := restarted.InboxFailures(mctx, 0, 1)
	if err != nil || len(failures) != 1 || failures[0].EventID != id || failures[0].GlobalSeq != 1 || failures[0].Kind != events.SchemaIncompatible || failures[0].Attempts != 1 || failures[0].RetryAtMillis != 0 {
		t.Fatal("persisted routing failure cannot be inspected", failures, err)
	}
	next, err := restarted.InboxFailures(mctx, failures[0].GlobalSeq, 1)
	if err != nil || len(next) != 0 {
		t.Fatal("failure page repeated an item", next, err)
	}
	if _, err = restarted.InboxFailures(mctx, -1, 100); err == nil {
		t.Fatal("invalid failure cursor accepted")
	}
	err = restarted.RetryInboxEvent(mctx, id)
	h.Release()
	f.gate.Open()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = restarted.InboxFailures(mctx, 0, 100); err == nil {
		t.Fatal("released maintenance context exposed failure queue")
	}
	if n, err := restarted.CatchUpInbox(ctx, 100); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	metrics, err = restarted.InboxMetrics(ctx)
	if err != nil || metrics.Through != 1 || metrics.Failures != 0 {
		t.Fatal(metrics, err)
	}
	mctx, h, err = f.gate.Maintain(ctx, "maintenance")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { h.Release(); f.gate.Open() }()
	failures, err = restarted.InboxFailures(mctx, 0, 100)
	if err != nil || len(failures) != 0 {
		t.Fatal("successful retry left a failure item", failures, err)
	}
}
