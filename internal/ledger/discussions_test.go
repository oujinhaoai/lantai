package ledger

import (
	"context"
	"math"
	"testing"

	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

type discussionAccess struct{ denyObject, denyAnchor, denyMention bool }

func (d *discussionAccess) Object(context.Context, authz.Context, DiscussionTarget) error {
	if d.denyObject {
		return errcode.New(errcode.NotFound, "")
	}
	return nil
}
func (d *discussionAccess) Task(ctx context.Context, who authz.Context, t DiscussionTarget, _ bool) error {
	return d.Object(ctx, who, t)
}
func (d *discussionAccess) Anchor(context.Context, authz.Context, DiscussionTarget, Anchor) error {
	if d.denyAnchor {
		return errcode.New(errcode.NotFound, "")
	}
	return nil
}
func (d *discussionAccess) Mention(context.Context, authz.Context, DiscussionTarget, Mention) error {
	if d.denyMention {
		return errcode.New(errcode.NotFound, "")
	}
	return nil
}
func TestDiscussionAppendReplayAndObjectIsolation(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.az.Grant(f.who.PrincipalID, f.project, "ledger.post_message", "ledger.read_discussion")
	in := MessageInput{Target: DiscussionTarget{ProjectID: f.project, Kind: "project", ID: f.project}, Kind: "answer", Text: "This answer is discussion data, not an approval."}
	first, err := f.s.PostMessage(ctx, f.who, "message-1", in, nil)
	if err != nil {
		t.Fatal(err)
	}
	again, err := f.s.PostMessage(ctx, f.who, "message-1", in, nil)
	if err != nil || again.ID != first.ID {
		t.Fatal(again, err)
	}
	in.Text = "another body"
	_, err = f.s.PostMessage(ctx, f.who, "message-1", in, nil)
	wantCode(t, err, errcode.IdempotencyConflict)
	in.Kind = "decision_proposal"
	in.ReplyTo = first.ID
	in.Supersedes = first.ID
	second, err := f.s.PostMessage(ctx, f.who, "message-2", in, nil)
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := f.s.Messages(ctx, f.who, in.Target, 0, 20, nil)
	if err != nil || len(msgs) != 2 || msgs[0].Text == in.Text || msgs[1].ID != second.ID {
		t.Fatal(msgs, err)
	}
	var count int
	if err = f.db.QueryRowContext(ctx, `SELECT count(*) FROM outbox WHERE event_type='comment.posted'`).Scan(&count); err != nil || count != 2 {
		t.Fatal(count, err)
	}
	other := f.newProject(t)
	f.az.Grant(f.who.PrincipalID, other, "ledger.post_message", "ledger.read_discussion")
	in.Target = DiscussionTarget{ProjectID: other, Kind: "project", ID: other}
	_, err = f.s.PostMessage(ctx, f.who, "cross-thread", in, nil)
	wantCode(t, err, errcode.NotFound)
	f.az.Revoke(f.who.PrincipalID, f.project)
	_, err = f.s.Messages(ctx, f.who, first.Target, 0, 10, nil)
	if err == nil {
		t.Fatal("revoked discussion readable")
	}
}
func TestDiscussionAnchorsAndReferencesReauthorize(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.az.Grant(f.who.PrincipalID, f.project, "ledger.post_message", "ledger.read_discussion")
	a := Anchor{Ref: ids.PermanentRef{InstanceID: ids.New(), AssetID: ids.New(), VersionID: ids.New()}, FilePath: "movie.mp4", Kind: "time", Values: []float64{1, 2}}
	access := &discussionAccess{}
	in := MessageInput{Target: DiscussionTarget{ProjectID: f.project, Kind: "task", ID: ids.New()}, Kind: "feedback", Text: "Check this range", Anchors: []Anchor{a}, Mentions: []Mention{{PrincipalID: ids.New()}}}
	if _, err := f.s.PostMessage(ctx, f.who, "no-task-source", in, nil); err == nil {
		t.Fatal("missing authority accepted")
	}
	posted, err := f.s.PostMessage(ctx, f.who, "anchored", in, access)
	if err != nil {
		t.Fatal(err)
	}
	access.denyAnchor = true
	access.denyMention = true
	msgs, err := f.s.Messages(ctx, f.who, in.Target, 0, 10, access)
	if err != nil || len(msgs) != 1 || len(msgs[0].Anchors) != 0 || len(msgs[0].Mentions) != 0 {
		t.Fatal(msgs, err)
	}
	_, err = f.s.PostMessage(ctx, f.who, "anchored", in, access)
	wantCode(t, err, errcode.NotFound)
	access.denyObject = true
	_, err = f.s.Messages(ctx, f.who, posted.Target, 0, 10, access)
	wantCode(t, err, errcode.NotFound)
	for _, bad := range []Anchor{
		{Ref: a.Ref, FilePath: "../secret", Kind: "time", Values: []float64{1, 2}},
		{Ref: a.Ref, FilePath: a.FilePath, Kind: "time", Values: []float64{math.NaN(), 2}},
		{Ref: a.Ref, FilePath: a.FilePath, Kind: "region", Values: []float64{.8, .8, .5, .5}},
		{Ref: a.Ref, FilePath: a.FilePath, Kind: "line", Values: []float64{0, 3}},
		{Ref: a.Ref, FilePath: a.FilePath, Kind: "frame", Values: []float64{1.2, 3}},
		{Ref: a.Ref, FilePath: a.FilePath, Kind: "camera", Values: []float64{0, 0, 0, 0, 0, 0, 90}},
	} {
		if bad.validate() == nil {
			t.Fatalf("invalid anchor accepted: %+v", bad)
		}
	}
}
func TestDiscussionSQLFailureRollsBackMessageAndReceipt(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.az.Grant(f.who.PrincipalID, f.project, "ledger.post_message", "ledger.read_discussion")
	if _, err := f.db.ExecContext(ctx, `CREATE TRIGGER fail_comment BEFORE INSERT ON outbox WHEN NEW.event_type='comment.posted' BEGIN SELECT RAISE(ABORT,'synthetic outbox fault'); END`); err != nil {
		t.Fatal(err)
	}
	in := MessageInput{Target: DiscussionTarget{ProjectID: f.project, Kind: "project", ID: f.project}, Kind: "note", Text: "atomic message"}
	if _, err := f.s.PostMessage(ctx, f.who, "atomic", in, nil); err == nil {
		t.Fatal("expected failure")
	}
	msgs, err := f.s.Messages(ctx, f.who, in.Target, 0, 10, nil)
	if err != nil || len(msgs) != 0 {
		t.Fatal(msgs, err)
	}
	if _, err = f.db.ExecContext(ctx, `DROP TRIGGER fail_comment`); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.PostMessage(ctx, f.who, "atomic", in, nil); err != nil {
		t.Fatal(err)
	}
}
