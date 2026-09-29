package integration

import (
	"encoding/json"
	"testing"

	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/ledger"
	"github.com/oujinhaoai/lantai/internal/provenance"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

func TestM2DiscussionsUseCurrentObjectAndAnchorRights(t *testing.T) {
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	ctx := t.Context()
	e.setRole(e.admin.Context.PrincipalID, identity.RoleOwner, true)
	who := e.login().Context
	principal, viewer := e.agent("discussion-viewer@node", identity.RoleViewer)
	v := e.ingest(who, "discussion-object", []byte("synthetic anchored document"), *rightsOwned())
	other := e.ingest(who, "discussion-other", []byte("other synthetic document"), *rightsOwned())
	access, err := e.ledger.NewDiscussionObjects(e.catalog, e.rights, e.id, nil)
	if err != nil {
		t.Fatal(err)
	}
	target := ledger.DiscussionTarget{ProjectID: e.project.ProjectID, Kind: "version", ID: v.VersionID}
	input := ledger.MessageInput{Target: target, Kind: "question", Text: "Review this synthetic line", Anchors: []ledger.Anchor{{Ref: v.Ref, FilePath: "content.txt", Kind: "line", Values: []float64{1, 1}}}, Mentions: []ledger.Mention{{PrincipalID: principal.ID}}}
	key := e.key()
	message, err := e.ledger.PostMessage(ctx, who, key, input, access)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := e.ledger.PostMessage(ctx, who, key, input, access)
	if err != nil || replay.ID != message.ID {
		t.Fatal(replay, err)
	}
	list, err := e.ledger.Messages(ctx, viewer.Context, target, 0, 20, access)
	if err != nil || len(list) != 1 || len(list[0].Anchors) != 1 || len(list[0].Mentions) != 1 {
		t.Fatal(list, err)
	}
	wrong := input
	wrong.Anchors = []ledger.Anchor{{Ref: other.Ref, FilePath: "content.txt", Kind: "line", Values: []float64{1, 1}}}
	if _, err = e.ledger.PostMessage(ctx, who, e.key(), wrong, access); errcode.CodeOf(err) != errcode.RefMismatch {
		t.Fatal("cross-object anchor", err)
	}
	wrong.Anchors = []ledger.Anchor{{Ref: v.Ref, FilePath: "missing.txt", Kind: "line", Values: []float64{1, 1}}}
	if _, err = e.ledger.PostMessage(ctx, who, e.key(), wrong, access); errcode.CodeOf(err) != errcode.NotFound {
		t.Fatal("missing file anchor", err)
	}
	// Project discussions may reference their visible objects, but references are
	// filtered again when those objects become private.
	projectInput := input
	projectInput.Target = ledger.DiscussionTarget{ProjectID: e.project.ProjectID, Kind: "project", ID: e.project.ProjectID}
	if _, err = e.ledger.PostMessage(ctx, who, e.key(), projectInput, access); err != nil {
		t.Fatal(err)
	}
	evidence := rightsEvidence(t, e, who, v, false)
	personal := "personal"
	_, err = e.rights.ApplyAssertion(ctx, who, e.key(), provenance.AssertionRequest{Subject: v.Ref, ManifestDigest: v.ManifestDigest, ExpectedRevision: 1, Kind: "restrict", Fields: provenance.AssertionFields{Sensitivity: &personal}, EvidenceIDs: []ids.ID{evidence.RecordID}, Reason: "synthetic current personal restriction"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.ledger.Messages(ctx, viewer.Context, target, 0, 20, access); err == nil {
		t.Fatal("private object discussion leaked")
	}
	list, err = e.ledger.Messages(ctx, viewer.Context, projectInput.Target, 0, 20, access)
	if err != nil || len(list) != 1 || len(list[0].Anchors) != 0 {
		t.Fatal("hidden anchor leaked", list, err)
	}
	readers, _ := json.Marshal([]ids.ID{who.PrincipalID, viewer.Context.PrincipalID})
	e.sudo(&identity.SetPolicy{ProjectID: e.project.ProjectID, Key: "personal.readers", ExpectedRevision: 0, Value: readers})
	list, err = e.ledger.Messages(ctx, viewer.Context, target, 0, 20, access)
	if err != nil || len(list) != 1 || len(list[0].Anchors) != 1 {
		t.Fatal(list, err)
	}
	before, err := e.ledger.VersionControl(ctx, v.VersionID)
	if err != nil {
		t.Fatal(err)
	}
	answer := input
	answer.Kind = "answer"
	answer.ReplyTo = message.ID
	if _, err = e.ledger.PostMessage(ctx, who, e.key(), answer, access); err != nil {
		t.Fatal(err)
	}
	after, err := e.ledger.VersionControl(ctx, v.VersionID)
	if err != nil || after != before {
		t.Fatal("answer changed review state", before, after, err)
	}
	task := input
	task.Target = ledger.DiscussionTarget{ProjectID: e.project.ProjectID, Kind: "task", ID: ids.New()}
	if _, err = e.ledger.PostMessage(ctx, who, e.key(), task, access); errcode.CodeOf(err) != errcode.InvalidStateTransition {
		t.Fatal("unwired T05 task accepted", err)
	}
}
