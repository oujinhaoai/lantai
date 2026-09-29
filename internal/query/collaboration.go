package query

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/event"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/events"
)

// ObjectRef is an exact object locator, not a permission or cached description.
type ObjectRef struct {
	ProjectID ids.ID `json:"project_id"`
	Kind      string `json:"kind"`
	ID        ids.ID `json:"id"`
}

func (r ObjectRef) valid() bool {
	return r.ProjectID.Valid() && r.ID.Valid() && slices.Contains([]string{"project", "asset", "version", "task", "review", "comment", "trash"}, r.Kind)
}

type ObjectState struct {
	Ref          ObjectRef `json:"ref"`
	Revision     int64     `json:"revision"`
	Title        string    `json:"title"`
	State        string    `json:"state"`
	Priority     int       `json:"priority"` // Smaller numbers sort first.
	DueAt        string    `json:"due_at,omitempty"`
	WaitingSince string    `json:"waiting_since,omitempty"`
	// Relevant says the current caller belongs in the object's inbox, distinct
	// from visibility: a reassigned task may remain readable but no longer relevant.
	Relevant bool `json:"-"`
}

// CollaborationObjects is a trusted T03/T05 adapter. Every method reads current
// authority, never index.db. Current rechecks object visibility, personal/rights
// restrictions and inbox assignment. Snapshot enumerates the same visible object
// scope as ResolveEvent in stable order, with an opaque continuation. It returns
// current state, not historical event payload. Current and Snapshot inherit the security guard.
// Recipients expands current assignments/roles, not untrusted payload mentions.
type CollaborationObjects interface {
	ResolveEvent(context.Context, event.Envelope) (ObjectRef, bool, error)
	Current(context.Context, authz.Context, ObjectRef) (ObjectState, error)
	Recipients(context.Context, event.Envelope, ObjectRef) ([]ids.ID, error)
	Snapshot(context.Context, authz.Context, ids.ID, string, int) ([]ObjectState, string, error)
}

// Collaboration owns only runtime inbox projection/read position. HTTP wiring
// belongs to T07; construction does not start goroutines or event consumption.
type Collaboration struct {
	q        *Service
	runtime  *sql.DB
	objects  CollaborationObjects
	consumer *events.Consumer
	mu       sync.Mutex
}

const inboxConsumer = "query.inbox"

func (s *Service) NewCollaboration(runtime *sql.DB, objects CollaborationObjects) (*Collaboration, error) {
	if runtime == nil || objects == nil {
		return nil, errors.New("query: runtime and authoritative collaboration objects required")
	}
	c, err := events.NewConsumer(runtime, inboxConsumer, clock.System{}, s.gate)
	if err != nil {
		return nil, err
	}
	return &Collaboration{q: s, runtime: runtime, objects: objects, consumer: c}, nil
}
func (c *Collaboration) authorized(ctx context.Context, who authz.Context, project ids.ID) error {
	if !project.Valid() {
		return errcode.New(errcode.SchemaInvalid, "project scope required")
	}
	d, err := c.q.authz.Authorize(ctx, who, "events.read", authz.Resource{ProjectID: project, Kind: "project", ID: project})
	if err != nil {
		return err
	}
	return d.Err()
}
func hidden(err error) bool {
	return errcode.CodeOf(err) == errcode.NotFound || errcode.CodeOf(err) == errcode.Forbidden || errcode.CodeOf(err) == errcode.UseRestricted || errcode.CodeOf(err) == errcode.AssetPurged
}
func (c *Collaboration) current(ctx context.Context, who authz.Context, ref ObjectRef) (ObjectState, error) {
	s, err := c.objects.Current(ctx, who, ref)
	if err != nil {
		return s, err
	}
	if s.Ref != ref || s.Revision < 1 {
		return s, errcode.New(errcode.RefMismatch, "invalid authoritative object response")
	}
	if s.DueAt != "" {
		if _, err = clock.Parse(s.DueAt); err != nil {
			return s, err
		}
	}
	if s.WaitingSince != "" {
		if _, err = clock.Parse(s.WaitingSince); err != nil {
			return s, err
		}
	}
	return s, nil
}

type EventRequest struct {
	Who       authz.Context
	ProjectID ids.ID
	After     int64
	Limit     int
	Wait      time.Duration
}
type VisibleEvent struct {
	EventID  ids.ID      `json:"event_id"`
	Sequence int64       `json:"sequence"`
	Type     string      `json:"type"`
	Object   ObjectState `json:"object"`
}
type EventPage struct {
	Events  []VisibleEvent `json:"events"`
	LastSeq int64          `json:"last_seq"`
	// A hidden/deleted target never leaks its ID. Clients replace their visible
	// scope to remove entries they may have cached before deletion/revocation.
	ReplaceRequired bool `json:"replace_required"`
}

func (c *Collaboration) scan(ctx context.Context, r EventRequest) (EventPage, error) {
	out := EventPage{Events: []VisibleEvent{}, LastSeq: r.After}
	ctx, h, err := c.q.gate.Coordinator().Acquire(ctx, commands.Request{Security: commands.ModeShared})
	if err != nil {
		return out, err
	}
	defer h.Release()
	if err = c.authorized(ctx, r.Who, r.ProjectID); err != nil {
		return out, err
	}
	p, err := c.q.events.Read(ctx, r.After, r.Limit)
	if err != nil {
		return out, err
	}
	for _, entry := range p.Entries {
		e := entry.Envelope
		if e.ProjectID != r.ProjectID {
			continue
		}
		if e.EventType == "ledger.control_changed" && e.AggregateType == "project" {
			out.ReplaceRequired = true
		}
		ref, ok, err := c.objects.ResolveEvent(ctx, e)
		if err != nil {
			return out, err
		}
		if !ok {
			continue
		}
		if !ref.valid() || ref.ProjectID != r.ProjectID {
			return out, errcode.New(errcode.RefMismatch, "event object scope mismatch")
		}
		obj, err := c.current(ctx, r.Who, ref)
		if hidden(err) {
			out.ReplaceRequired = true
			continue
		}
		if err != nil {
			return out, err
		}
		out.Events = append(out.Events, VisibleEvent{EventID: e.EventID, Sequence: entry.GlobalSeq, Type: e.EventType, Object: obj})
	}
	out.LastSeq = p.HighWater
	return out, nil
}

// Events releases all guards while waiting. Every wake revalidates session and
// scope; filtered pages return immediately with their scan high-water mark.
func (c *Collaboration) Events(ctx context.Context, r EventRequest) (EventPage, error) {
	if r.Limit == 0 {
		r.Limit = 100
	}
	if r.After < 0 || r.Limit < 1 || r.Limit > 1000 || r.Wait < 0 || r.Wait > 30*time.Second {
		return EventPage{}, errcode.New(errcode.SchemaInvalid, "invalid event window")
	}
	deadline := time.Now().Add(r.Wait)
	for {
		out, err := c.scan(ctx, r)
		if err != nil || out.LastSeq > r.After || r.Wait == 0 || !time.Now().Before(deadline) {
			return out, err
		}
		t := time.NewTimer(min(250*time.Millisecond, time.Until(deadline)))
		select {
		case <-ctx.Done():
			t.Stop()
			return EventPage{}, ctx.Err()
		case <-t.C:
		}
	}
}

type syncCursor struct {
	Principal, Session, Project ids.ID
	Start                       int64
	After                       string
}
type ResyncPage struct {
	Objects     []ObjectState `json:"objects"`
	ReplayAfter int64         `json:"replay_after"`
	NextCursor  string        `json:"next_cursor,omitempty"`
	Replace     bool          `json:"replace"` // Accumulate pages, replace the whole scope on completion, then replay.
}

func (c *Collaboration) seal(v syncCursor) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	n := make([]byte, c.q.cursor.NonceSize())
	if _, err = rand.Read(n); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(c.q.cursor.Seal(n, n, b, []byte("lantai.query.resync/v1"))), nil
}
func (c *Collaboration) unseal(raw string) (syncCursor, error) {
	var v syncCursor
	b, err := base64.RawURLEncoding.DecodeString(raw)
	n := c.q.cursor.NonceSize()
	if err != nil || len(b) < n {
		return v, expired("invalid resync cursor")
	}
	b, err = c.q.cursor.Open(nil, b[:n], b[n:], []byte("lantai.query.resync/v1"))
	if err != nil {
		return v, expired("resync cursor expired")
	}
	if err = json.Unmarshal(b, &v); err != nil {
		return v, expired("invalid resync cursor")
	}
	return v, nil
}

// Resync captures S before the first authoritative page. A continuation is bound
// to the same principal/session/project and S. The source never substitutes a
// lagging index. If S expires during a long scan, discard the partial snapshot.
func (c *Collaboration) Resync(ctx context.Context, who authz.Context, project ids.ID, cursor string, limit int) (ResyncPage, error) {
	var out ResyncPage
	if limit < 1 || limit > 200 {
		return out, errcode.New(errcode.SchemaInvalid, "snapshot limit must be 1..200")
	}
	ctx, h, err := c.q.gate.Coordinator().Acquire(ctx, commands.Request{Security: commands.ModeShared})
	if err != nil {
		return out, err
	}
	defer h.Release()
	if err = c.authorized(ctx, who, project); err != nil {
		return out, err
	}
	v := syncCursor{Principal: who.PrincipalID, Session: who.SessionID, Project: project}
	if cursor == "" {
		v.Start, err = c.q.events.HighWater(ctx)
	} else {
		v, err = c.unseal(cursor)
	}
	if err != nil {
		return out, err
	}
	if v.Principal != who.PrincipalID || v.Session != who.SessionID || v.Project != project {
		return out, expired("resync scope changed")
	}
	if _, err = c.q.events.Read(ctx, v.Start, 1); err != nil {
		return out, err
	}
	objects, next, err := c.objects.Snapshot(ctx, who, project, v.After, limit)
	if err != nil {
		return out, err
	}
	if len(objects) > limit || (next != "" && next == v.After) {
		return out, errors.New("query: invalid snapshot continuation")
	}
	out = ResyncPage{Objects: []ObjectState{}, ReplayAfter: v.Start, Replace: true}
	seen := map[ObjectRef]bool{}
	for _, obj := range objects {
		if !obj.Ref.valid() || obj.Ref.ProjectID != project || seen[obj.Ref] {
			return out, errcode.New(errcode.RefMismatch, "snapshot object scope or duplication")
		}
		seen[obj.Ref] = true
		current, err := c.current(ctx, who, obj.Ref)
		if hidden(err) {
			continue
		}
		if err != nil {
			return out, err
		}
		out.Objects = append(out.Objects, current)
	}
	if next != "" {
		v.After = next
		out.NextCursor, err = c.seal(v)
	}
	return out, err
}

// CatchUpInbox uses the shared transactional consumer: projection, deduplication
// and offset commit together. Recipients/locators are resolved before its SQL
// transaction. Errors leave the offset unchanged; retries also acknowledge a
// previously committed retention watermark.
func (c *Collaboration) CatchUpInbox(ctx context.Context, limit int) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if limit < 1 || limit > 1000 {
		return 0, errcode.New(errcode.SchemaInvalid, "invalid inbox batch")
	}
	if r, ok := c.q.events.(Retainer); ok {
		if err := r.RegisterConsumer(ctx, inboxConsumer, true); err != nil {
			return 0, err
		}
	}
	offset, err := c.consumer.Offset(ctx)
	if err != nil {
		return 0, err
	}
	p, err := c.q.events.Read(ctx, offset, limit)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, entry := range p.Entries {
		ref, recipients, prepareErr := c.prepareInbox(ctx, entry.Envelope)
		applied, err := c.consumer.Apply(ctx, entry, func(ctx context.Context, tx *sql.Tx, e events.Entry) ([]events.Command, error) {
			// Authority/file calls already finished outside SQL. Persist preparation
			// failures through the same consumer failure/backoff path as write failures.
			if prepareErr != nil {
				return nil, prepareErr
			}
			for _, principal := range recipients {
				_, err := tx.ExecContext(ctx, `INSERT INTO query_inbox_items VALUES(?,?,?,?,?,?) ON CONFLICT(principal_id,project_id,object_kind,object_id) DO UPDATE SET event_id=excluded.event_id,global_seq=excluded.global_seq WHERE excluded.global_seq>query_inbox_items.global_seq`, principal, ref.ProjectID, ref.Kind, ref.ID, e.Envelope.EventID, e.GlobalSeq)
				if err != nil {
					return nil, err
				}
			}
			return nil, nil
		})
		if err != nil {
			return count, err
		}
		if applied {
			count++
		}
	}
	offset, err = c.consumer.Offset(ctx)
	if err != nil {
		return count, err
	}
	if r, ok := c.q.events.(Retainer); ok {
		err = r.AcknowledgeConsumer(ctx, inboxConsumer, offset)
	}
	return count, err
}

type InboxItem struct {
	Object   ObjectState `json:"object"`
	EventID  ids.ID      `json:"event_id"`
	Sequence int64       `json:"sequence"`
	Read     bool        `json:"read"`
}
type InboxPage struct {
	Items       []InboxItem `json:"items"`
	Through     int64       `json:"through"`
	ReadThrough int64       `json:"read_through"`
}

// Inbox reads current state for every candidate, so delayed invalidation or
// reassignment events cannot expose a stale item. No historical payload is used.
func (c *Collaboration) Inbox(ctx context.Context, who authz.Context, project ids.ID) (InboxPage, error) {
	out := InboxPage{Items: []InboxItem{}}
	ctx, h, err := c.q.gate.Coordinator().Acquire(ctx, commands.Request{Security: commands.ModeShared})
	if err != nil {
		return out, err
	}
	defer h.Release()
	if err = c.authorized(ctx, who, project); err != nil {
		return out, err
	}
	out.Through, err = c.consumer.Offset(ctx)
	if err != nil {
		return out, err
	}
	err = c.runtime.QueryRowContext(ctx, `SELECT through_seq FROM query_inbox_read WHERE principal_id=? AND project_id=?`, who.PrincipalID, project).Scan(&out.ReadThrough)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return out, err
	}
	rows, err := c.runtime.QueryContext(ctx, `SELECT object_kind,object_id,event_id,global_seq FROM query_inbox_items WHERE principal_id=? AND project_id=? AND global_seq<=? ORDER BY object_kind,object_id LIMIT 10001`, who.PrincipalID, project, out.Through)
	if err != nil {
		return out, err
	}
	var candidates []InboxItem
	for rows.Next() {
		var i InboxItem
		i.Object.Ref.ProjectID = project
		if err = rows.Scan(&i.Object.Ref.Kind, &i.Object.Ref.ID, &i.EventID, &i.Sequence); err != nil {
			rows.Close()
			return out, err
		}
		candidates = append(candidates, i)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	if len(candidates) > 10000 {
		return out, errcode.New(errcode.QuotaExceeded, "inbox scope exceeds bounded snapshot limit")
	}
	for _, i := range candidates {
		obj, err := c.current(ctx, who, i.Object.Ref)
		if hidden(err) {
			continue
		}
		if err != nil {
			return out, err
		}
		if !obj.Relevant {
			continue
		}
		i.Object = obj
		i.Read = i.Sequence <= out.ReadThrough
		out.Items = append(out.Items, i)
	}
	slices.SortFunc(out.Items, func(a, b InboxItem) int {
		if a.Object.Priority != b.Object.Priority {
			if a.Object.Priority < b.Object.Priority {
				return -1
			}
			return 1
		}
		dueA, dueB := a.Object.DueAt, b.Object.DueAt
		if dueA == "" {
			dueA = "~"
		}
		if dueB == "" {
			dueB = "~"
		}
		if n := strings.Compare(dueA, dueB); n != 0 {
			return n
		}
		if n := strings.Compare(a.Object.WaitingSince, b.Object.WaitingSince); n != 0 {
			return n
		}
		if n := strings.Compare(a.Object.Ref.Kind, b.Object.Ref.Kind); n != 0 {
			return n
		}
		return strings.Compare(string(a.Object.Ref.ID), string(b.Object.Ref.ID))
	})
	return out, nil
}

// MarkRead advances only the caller's project watermark, never business state.
// Future/unconsumed sequences are rejected and old retries cannot move it back.
func (c *Collaboration) MarkRead(ctx context.Context, who authz.Context, project ids.ID, through int64) error {
	ctx, h, err := c.q.gate.Acquire(ctx, commands.Request{Security: commands.ModeShared})
	if err != nil {
		return err
	}
	defer h.Release()
	if err = c.authorized(ctx, who, project); err != nil {
		return err
	}
	high, err := c.consumer.Offset(ctx)
	if err != nil {
		return err
	}
	if through < 0 || through > high {
		return errcode.New(errcode.PreconditionFailed, "read position exceeds consumed events")
	}
	_, err = c.runtime.ExecContext(ctx, `INSERT INTO query_inbox_read VALUES(?,?,?) ON CONFLICT(principal_id,project_id) DO UPDATE SET through_seq=max(through_seq,excluded.through_seq)`, who.PrincipalID, project, through)
	return err
}

// InboxSeed comes from current T03/T05 authority, not historical notification
// payload. Rebuild seeds have no event_id; Sequence denotes the snapshot boundary.
type InboxSeed struct {
	PrincipalID ids.ID
	Ref         ObjectRef
}
type InboxRebuilder interface {
	InboxSnapshot(context.Context) ([]InboxSeed, error)
}

// RebuildInbox is a maintenance-only recovery hook for a deleted/expired inbox
// projection. Preserve per-identity read positions, atomically replace references,
// then replay from S; changing recipients is still checked on every actual read.
func (c *Collaboration) RebuildInbox(ctx context.Context) (int64, error) {
	if err := c.q.gate.RequireMaintenance(ctx); err != nil {
		return 0, err
	}
	source, ok := c.objects.(InboxRebuilder)
	if !ok {
		return 0, errors.New("query: authoritative inbox rebuild source required")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if r, ok := c.q.events.(Retainer); ok {
		if err := r.RegisterConsumer(ctx, inboxConsumer, true); err != nil {
			return 0, err
		}
	}
	start, err := c.q.events.HighWater(ctx)
	if err != nil {
		return 0, err
	}
	seeds, err := source.InboxSnapshot(ctx)
	if err != nil {
		return 0, err
	}
	if _, err = c.q.events.Read(ctx, start, 1); err != nil {
		return 0, err
	}
	err = c.consumer.RebuildProjection(ctx, start, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM query_inbox_items`); err != nil {
			return err
		}
		for _, seed := range seeds {
			if !seed.PrincipalID.Valid() || !seed.Ref.valid() {
				return errors.New("query: invalid authoritative inbox seed")
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO query_inbox_items VALUES(?,?,?,?,'',?) ON CONFLICT(principal_id,project_id,object_kind,object_id) DO NOTHING`, seed.PrincipalID, seed.Ref.ProjectID, seed.Ref.Kind, seed.Ref.ID, start); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	if r, ok := c.q.events.(Retainer); ok {
		err = r.AcknowledgeConsumer(ctx, inboxConsumer, start)
	}
	return start, err
}

func (c *Collaboration) prepareInbox(ctx context.Context, e event.Envelope) (ObjectRef, []ids.ID, error) {
	ref, ok, err := c.objects.ResolveEvent(ctx, e)
	if err == nil && ok && (!ref.valid() || ref.ProjectID != e.ProjectID) {
		err = errcode.New(errcode.RefMismatch, "invalid inbox event scope")
	}
	var recipients []ids.ID
	if err == nil && ok {
		recipients, err = c.objects.Recipients(ctx, e, ref)
	}
	if err == nil && len(recipients) > 10000 {
		err = errcode.New(errcode.QuotaExceeded, "inbox recipient expansion exceeds limit")
	}
	if err == nil {
		for _, id := range recipients {
			if !id.Valid() {
				err = errcode.New(errcode.RefMismatch, "invalid inbox recipient")
				break
			}
		}
	}
	if err != nil {
		switch errcode.CodeOf(err) {
		case errcode.SchemaInvalid, errcode.RefMismatch:
			err = &events.Failure{Kind: events.SchemaIncompatible, Cause: err}
		case errcode.InvalidStateTransition:
			err = &events.Failure{Kind: events.MissingPrerequisite, Cause: err}
		}
	}
	return ref, recipients, err
}

// RetryInboxEvent is a T08 maintenance hook after an operator repairs the cause.
// It releases failure backoff only; it never advances the offset or a domain state.
func (c *Collaboration) RetryInboxEvent(ctx context.Context, id ids.ID) error {
	if err := c.q.gate.RequireMaintenance(ctx); err != nil {
		return err
	}
	if !id.Valid() {
		return errcode.New(errcode.SchemaInvalid, "")
	}
	return c.consumer.Retry(ctx, id)
}

// InboxMetrics is a trusted local operations read; it contains no object data.
func (c *Collaboration) InboxMetrics(ctx context.Context) (events.ConsumerMetrics, error) {
	return c.consumer.Metrics(ctx)
}

// InboxFailures exposes failed reminder/review/discussion routing to T08. The
// owning consumer enforces maintenance and returns identifiers, not user text.
func (c *Collaboration) InboxFailures(ctx context.Context, after int64, limit int) ([]events.ConsumerFailure, error) {
	return c.consumer.Failures(ctx, after, limit)
}
