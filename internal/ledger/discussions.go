package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/oujinhaoai/lantai/internal/catalog/pathrule"
	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/event"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/schema"
)

type DiscussionTarget struct {
	ProjectID ids.ID `json:"project_id"`
	Kind      string `json:"kind"`
	ID        ids.ID `json:"id"`
}

func (t DiscussionTarget) valid() bool {
	return t.ProjectID.Valid() && t.ID.Valid() && slices.Contains([]string{"project", "task", "asset", "version"}, t.Kind)
}

// Anchor points to an immutable file version. Coordinates never float with the
// current asset alias. Values: time/frame/line/page [start,end], region [x,y,w,h],
// camera [position xyz, target xyz, vertical field-of-view degrees].
type Anchor struct {
	Ref      ids.PermanentRef `json:"ref"`
	FilePath string           `json:"file_path"`
	Kind     string           `json:"kind"`
	Values   []float64        `json:"values"`
}

func (a Anchor) validate() error {
	if err := a.Ref.Validate(true); err != nil {
		return invalid("exact anchor reference required")
	}
	if err := pathrule.Check(a.FilePath); err != nil {
		return err
	}
	for _, v := range a.Values {
		if math.IsInf(v, 0) || math.IsNaN(v) {
			return invalid("anchor coordinates must be finite")
		}
	}
	switch a.Kind {
	case "time", "frame", "line", "page":
		if len(a.Values) != 2 || a.Values[0] < 0 || a.Values[1] < a.Values[0] {
			return invalid("invalid anchor range")
		}
		if a.Kind != "time" && (a.Values[0] != math.Trunc(a.Values[0]) || a.Values[1] != math.Trunc(a.Values[1])) {
			return invalid("integer anchor coordinates required")
		}
		if (a.Kind == "line" || a.Kind == "page") && a.Values[0] < 1 {
			return invalid("line/page anchors are one-based")
		}
	case "region":
		if len(a.Values) != 4 || a.Values[0] < 0 || a.Values[1] < 0 || a.Values[2] <= 0 || a.Values[3] <= 0 || a.Values[0]+a.Values[2] > 1 || a.Values[1]+a.Values[3] > 1 {
			return invalid("region must be inside normalized image")
		}
	case "camera":
		if len(a.Values) != 7 || a.Values[6] <= 0 || a.Values[6] >= 180 || (a.Values[0] == a.Values[3] && a.Values[1] == a.Values[4] && a.Values[2] == a.Values[5]) {
			return invalid("invalid camera anchor")
		}
	default:
		return invalid("unsupported anchor type")
	}
	return nil
}

type Mention struct {
	PrincipalID ids.ID `json:"principal_id,omitempty"`
	Role        string `json:"role,omitempty"`
}
type MessageInput struct {
	Target     DiscussionTarget `json:"target"`
	Kind       string           `json:"kind"`
	Text       string           `json:"text"`
	Anchors    []Anchor         `json:"anchors"`
	Mentions   []Mention        `json:"mentions"`
	ReplyTo    ids.ID           `json:"reply_to,omitempty"`
	Supersedes ids.ID           `json:"supersedes,omitempty"`
}
type Message struct {
	Sequence int64  `json:"sequence"`
	ID       ids.ID `json:"message_id"`
	MessageInput
	AuthorID    ids.ID `json:"author_id"`
	SessionID   ids.ID `json:"session_id"`
	OperationID ids.ID `json:"operation_id"`
	CreatedAt   string `json:"created_at"`
}

// DiscussionAccess is a trusted read-only composition of T01/T02/T05. It must
// validate current task visibility, anchor file existence/permissions and mention
// membership without reacquiring the inherited security guard. No method sends
// notifications or changes a task/review. Nil permits only unadorned core targets.
type DiscussionAccess interface {
	Object(context.Context, authz.Context, DiscussionTarget) error
	Task(context.Context, authz.Context, DiscussionTarget, bool) error
	Anchor(context.Context, authz.Context, DiscussionTarget, Anchor) error
	Mention(context.Context, authz.Context, DiscussionTarget, Mention) error
}

func (s *Service) discussionTarget(ctx context.Context, who authz.Context, t DiscussionTarget, write bool, access DiscussionAccess) error {
	if !t.valid() {
		return invalid("invalid discussion target")
	}
	action := authz.Action("ledger.read_discussion")
	if write {
		action = "ledger.post_message"
	}
	if err := s.authorize(ctx, who, action, t.ProjectID, t.Kind, t.ID); err != nil {
		return err
	}
	switch t.Kind {
	case "project":
		if t.ID != t.ProjectID {
			return errcode.New(errcode.RefMismatch, "")
		}
		_, err := s.Project(ctx, t.ID)
		return err
	case "asset":
		a, err := s.Asset(ctx, t.ID)
		if err != nil {
			return err
		}
		if a.ProjectID != t.ProjectID {
			return errcode.New(errcode.RefMismatch, "")
		}
	case "version":
		v, err := s.VersionByID(ctx, t.ID)
		if err != nil {
			return err
		}
		if v.ProjectID != t.ProjectID {
			return errcode.New(errcode.RefMismatch, "")
		}
	case "task":
		if access == nil {
			return errcode.New(errcode.InvalidStateTransition, "T05 task visibility adapter required")
		}
		return access.Task(ctx, who, t, write)
	}
	if t.Kind == "asset" || t.Kind == "version" {
		if access == nil {
			return errcode.New(errcode.InvalidStateTransition, "object visibility adapter required")
		}
		return access.Object(ctx, who, t)
	}
	return nil
}
func validateMessage(m MessageInput) error {
	if !m.Target.valid() || !slices.Contains([]string{"note", "question", "answer", "feedback", "handoff", "decision_proposal"}, m.Kind) || len(strings.TrimSpace(m.Text)) == 0 || len(m.Text) > 64<<10 || !utf8.ValidString(m.Text) || len(m.Anchors) > 100 || len(m.Mentions) > 100 {
		return invalid("invalid message")
	}
	if m.ReplyTo != "" && !m.ReplyTo.Valid() || m.Supersedes != "" && !m.Supersedes.Valid() {
		return invalid("invalid related message")
	}
	for _, a := range m.Anchors {
		if err := a.validate(); err != nil {
			return err
		}
	}
	seen := map[Mention]bool{}
	for _, v := range m.Mentions {
		if (v.PrincipalID == "") == (v.Role == "") || (v.PrincipalID != "" && !v.PrincipalID.Valid()) || (v.Role != "" && !slices.Contains([]string{"owner", "reviewer", "checker", "coordinator", "contributor", "curator", "viewer"}, v.Role)) || seen[v] {
			return invalid("invalid or duplicate mention")
		}
		seen[v] = true
	}
	return nil
}

// PostMessage is an append-only ledger transaction with its receipt and outbox.
// Answers and decision proposals have no approval/publication side effects.
func (s *Service) PostMessage(ctx context.Context, who authz.Context, key string, m MessageInput, access DiscussionAccess) (Message, error) {
	var out Message
	if err := validateMessage(m); err != nil {
		return out, err
	}
	if m.Anchors == nil {
		m.Anchors = []Anchor{}
	}
	if m.Mentions == nil {
		m.Mentions = []Mention{}
	}
	raw, err := canonjson.CanonicalizeValue(m)
	if err != nil {
		return out, err
	}
	if len(raw) > 48<<10 {
		return out, invalid("message exceeds bounded record size")
	}
	hash, err := commands.RequestHash(commands.HashInput{CommandType: "ledger.post_message", ProjectID: m.Target.ProjectID, Body: raw})
	if err != nil {
		return out, err
	}
	ctx, release, err := s.write(ctx, m.Target.ProjectID)
	if err != nil {
		return out, err
	}
	defer release()
	if err = s.discussionTarget(ctx, who, m.Target, true, access); err != nil {
		return out, err
	}
	epoch, err := s.authority.RecoveryEpoch(ctx)
	if err != nil {
		return out, err
	}
	op, err := s.ids.New()
	if err != nil {
		return out, err
	}
	cmd := commands.Context{OperationID: op, IdempotencyKey: key, CommandType: "ledger.post_message", ActorID: who.PrincipalID, SessionID: who.SessionID, ProjectID: m.Target.ProjectID, RequestHash: hash, RecoveryEpoch: epoch, PolicyRevision: who.PolicyRevision}
	// Read-only ports run before the ledger transaction. They may use owner read
	// APIs, but cannot write, call plugins or reacquire the inherited guard.
	if (len(m.Anchors) > 0 || len(m.Mentions) > 0) && access == nil {
		return out, invalid("anchor/mention authority required")
	}
	for _, a := range m.Anchors {
		if err := access.Anchor(ctx, who, m.Target, a); err != nil {
			return out, err
		}
	}
	for _, v := range m.Mentions {
		if err := access.Mention(ctx, who, m.Target, v); err != nil {
			return out, err
		}
	}
	result, err := s.store.Execute(ctx, s.db, cmd, func(ctx context.Context, tx *sql.Tx) (commands.Result, error) {
		for _, id := range []ids.ID{m.ReplyTo, m.Supersedes} {
			if id == "" {
				continue
			}
			old, err := readJSON[Message](ctx, tx, `SELECT record FROM ledger_messages WHERE message_id=? AND project_id=? AND object_kind=? AND object_id=?`, id, m.Target.ProjectID, m.Target.Kind, m.Target.ID)
			if err != nil {
				return commands.Result{}, err
			}
			if id == m.Supersedes && old.AuthorID != who.PrincipalID {
				return commands.Result{}, errcode.New(errcode.Forbidden, "only author may supersede a message")
			}
		}

		id, err := s.ids.New()
		if err != nil {
			return commands.Result{}, err
		}
		out = Message{ID: id, MessageInput: m, AuthorID: who.PrincipalID, SessionID: who.SessionID, OperationID: op, CreatedAt: clock.Format(s.clock.Now())}
		inserted, err := tx.ExecContext(ctx, `INSERT INTO ledger_messages(message_id,project_id,object_kind,object_id,operation_id,record) VALUES(?,?,?,?,?,?)`, id, m.Target.ProjectID, m.Target.Kind, m.Target.ID, op, encoded(out))
		if err != nil {
			return commands.Result{}, err
		}
		out.Sequence, err = inserted.LastInsertId()
		if err != nil {
			return commands.Result{}, err
		}
		registry, err := schema.Default()
		if err != nil {
			return commands.Result{}, err
		}
		if err = registry.ValidateJSON("lantai.discussion-message/v1", []byte(encoded(out))); err != nil {
			return commands.Result{}, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE ledger_messages SET record=? WHERE message_id=?`, encoded(out), id); err != nil {
			return commands.Result{}, err
		}
		e, err := s.event(cmd, "comment.posted", "comment", id, 1, map[string]any{"target": m.Target, "message_id": id, "kind": m.Kind, "mentions": m.Mentions})
		if err != nil {
			return commands.Result{}, err
		}
		// The text/anchors remain behind object authorization, not copied into events.
		return commands.Result{Status: commands.ReceiptSucceeded, ResponseCode: 201, Summary: out, Events: []event.Envelope{e}}, nil
	})
	if err != nil {
		return out, err
	}
	err = json.Unmarshal(result.Receipt.ResponseSummary, &out)
	return out, err
}
func (s *Service) Messages(ctx context.Context, who authz.Context, target DiscussionTarget, after int64, limit int, access DiscussionAccess) ([]Message, error) {
	if limit < 1 || limit > 200 || after < 0 {
		return nil, invalid("invalid discussion page")
	}
	ctx, h, err := s.gate.Coordinator().Acquire(ctx, commands.Request{Security: commands.ModeShared})
	if err != nil {
		return nil, err
	}
	defer h.Release()
	if err = s.discussionTarget(ctx, who, target, false, access); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT record FROM ledger_messages WHERE project_id=? AND object_kind=? AND object_id=? AND sequence>? ORDER BY sequence LIMIT ?`, target.ProjectID, target.Kind, target.ID, after, limit)
	if err != nil {
		return nil, err
	}
	out := []Message{}
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			rows.Close()
			return nil, err
		}
		var m Message
		if err = json.Unmarshal([]byte(raw), &m); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, m)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	// Anchor/mention visibility may have narrowed since posting. Omit inaccessible
	// references without disclosing their IDs or count, preserving the body itself.
	for i := range out {
		anchors := []Anchor{}
		for _, a := range out[i].Anchors {
			if access != nil {
				if e := access.Anchor(ctx, who, target, a); e == nil {
					anchors = append(anchors, a)
				} else if !discussionHidden(e) {
					return nil, e
				}
			}
		}
		out[i].Anchors = anchors
		mentions := []Mention{}
		for _, v := range out[i].Mentions {
			if access != nil {
				if e := access.Mention(ctx, who, target, v); e == nil {
					mentions = append(mentions, v)
				} else if !discussionHidden(e) {
					return nil, e
				}
			}
		}
		out[i].Mentions = mentions
	}
	return out, nil
}
func discussionHidden(err error) bool {
	return slices.Contains([]errcode.Code{errcode.NotFound, errcode.Forbidden, errcode.UseRestricted, errcode.AssetPurged}, errcode.CodeOf(err))
}
