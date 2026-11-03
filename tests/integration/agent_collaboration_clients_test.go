package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/cli"
	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/ledger"
	"github.com/oujinhaoai/lantai/internal/mcpserver"
)

func TestAgentCollaborationClients(t *testing.T) {
	f := newAppFlow(t)
	producer, err := f.app.Extensions.Producer(t.Context(), "org.lantai.corecheck.manifest")
	if err != nil {
		t.Fatal(err)
	}
	// Synthetic bootstrap uses the original human-factor protocol and actual
	// one-shot checker. The ordinary Agent never receives that human session.
	profile := f.approvedProfile("profiles/collaboration", profileDocument("collaboration", producer), ids.PermanentRef{})
	v, flow := f.candidate("docs/collaboration", manifest.TypeDoc, map[string][]byte{"data.txt": []byte("synthetic reviewed content")}, map[string]any{})
	job := f.checkCandidate(v, flow, "org.lantai.corecheck.manifest")
	principal, session := f.agent("collaboration@synthetic", identity.RoleOwner)
	servers := remoteServers(t, f.app, true)
	origin := "http://" + servers.Addresses.API
	c, err := client.New(client.Config{BaseURL: origin, SessionToken: session.Token, AllowHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	s, err := mcpserver.New(t.Context(), mcpserver.Config{Client: c})
	if err != nil {
		t.Fatal(err)
	}
	ct, st := mcp.NewInMemoryTransports()
	ss, err := s.Connect(t.Context(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "collaboration-acceptance", Version: "1"}, nil).Connect(t.Context(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	call := func(name string, args any, wantError bool) map[string]any {
		t.Helper()
		r, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil || r.IsError != wantError {
			t.Fatalf("%s: %v %s", name, err, mustJSON(r))
		}
		if len(mustJSON(r)) > mcpserver.MaxResultBytes {
			t.Fatal("unbounded serialized result", name)
		}
		return consistencyJSON(t, mustJSON(r.StructuredContent))
	}
	state, err := f.ledger.VersionControl(t.Context(), v.VersionID)
	if err != nil {
		t.Fatal(err)
	}
	submit := ledger.SubmitReview{ProjectID: f.project.ProjectID, VersionID: v.VersionID, ExpectedRevision: state.Revision, ProfileRef: profile.Ref, EvidenceIDs: evidenceIDs(job), Flow: flow}
	targetJSON := call("review_submit", map[string]any{"request": submit, "idempotency_key": "original-review-submit"}, false)
	var target ledger.ReviewTarget
	if err := json.Unmarshal(mustJSON(targetJSON), &target); err != nil || !target.ID.Valid() || target.SubmittedBy != principal.ID {
		t.Fatal("review submission not attributed to normal Agent", target, err)
	}
	replayed := call("review_submit", map[string]any{"request": submit, "idempotency_key": "original-review-submit"}, false)
	if replayed["target_id"] != targetJSON["target_id"] {
		t.Fatal("review replay replaced target")
	}
	call("review_target_read", map[string]any{"id": target.ID}, false)
	// An invented review cannot publish; the later human decision is confined
	// to this temporary fixture and uses fresh challenge factors.
	publication := ledger.PublishRequest{ProjectID: f.project.ProjectID, VersionID: v.VersionID, ReviewID: ids.New(), ExpectedRevision: 0, Action: "publish", Reason: "Synthetic original approved review required"}
	call("review_publish", map[string]any{"request": publication, "idempotency_key": "reject-unapproved-publication"}, true)
	f.decide(target.ID, "approve")
	state, err = f.ledger.VersionControl(t.Context(), v.VersionID)
	if err != nil {
		t.Fatal(err)
	}
	assetState, err := f.ledger.AssetControl(t.Context(), v.AssetID)
	if err != nil {
		t.Fatal(err)
	}
	publication.ReviewID = state.EffectiveReviewID
	publication.ExpectedRevision = assetState.PublicationRevision
	published := call("review_publish", map[string]any{"request": publication, "idempotency_key": "original-approved-publication"}, false)
	again := call("review_publish", map[string]any{"request": publication, "idempotency_key": "original-approved-publication"}, false)
	if !bytes.Equal(mustJSON(published), mustJSON(again)) {
		t.Fatal("publication replay changed receipt")
	}
	// Milestone writes use the original owner port and current task facts.
	milestone := identity.Milestone{ProjectID: f.project.ProjectID, Name: "Synthetic current progress", OwnerID: f.admin.Context.PrincipalID, TaskIDs: []ids.ID{flow.TaskID}}
	request := map[string]any{"expected_revision": 0, "milestone": milestone}
	result := call("milestone_set", map[string]any{"project_id": f.project.ProjectID, "request": request, "idempotency_key": "original-milestone"}, false)
	var receipt identity.Result
	if err := json.Unmarshal(mustJSON(result), &receipt); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(receipt.Summary, &milestone); err != nil || !milestone.ID.Valid() {
		t.Fatal(err, milestone)
	}
	call("milestone_set", map[string]any{"project_id": f.project.ProjectID, "request": request, "idempotency_key": "original-milestone"}, false)
	call("milestone_set", map[string]any{"project_id": f.project.ProjectID, "request": map[string]any{"expected_revision": 0, "milestone": milestone}, "idempotency_key": "new-intent-stale-milestone"}, true)
	progress := call("milestone_progress", map[string]any{"project_id": f.project.ProjectID, "id": milestone.ID}, false)
	if progress["total"] != float64(1) {
		t.Fatal("milestone progress not from real task", progress)
	}
	page := call("milestone_list", map[string]any{"project_id": f.project.ProjectID, "limit": 1}, false)
	if page["complete"] != true || len(page["items"].([]any)) != 1 {
		t.Fatal(page)
	}
	work := t.TempDir()
	sessionFile := filepath.Join(work, "session.json")
	writeJSON(t, sessionFile, map[string]any{"schema": "lantai.client-session/v1", "origin": origin, "token": session.Token})
	for _, args := range [][]string{{"milestone", "list"}, {"milestone", "progress", "--id", string(milestone.ID)}} {
		var stdout, stderr bytes.Buffer
		args = append(args, "--project", string(f.project.ProjectID), "--server", origin, "--allow-http", "--session-file", sessionFile)
		if code := cli.Run(t.Context(), args, &stdout, &stderr); code != 0 || !bytes.Contains(stdout.Bytes(), []byte(milestone.ID)) {
			t.Fatal("CLI milestone failed", code, stderr.String())
		}
	}
	message := ledger.MessageInput{Target: ledger.DiscussionTarget{ProjectID: f.project.ProjectID, Kind: "project", ID: f.project.ProjectID}, Kind: "note", Text: "Synthetic collaboration follow-up", Anchors: []ledger.Anchor{}, Mentions: []ledger.Mention{{PrincipalID: principal.ID}}}
	call("message_post", map[string]any{"request": message, "idempotency_key": "original-message"}, false)
	if err := f.app.Sync(t.Context()); err != nil {
		t.Fatal(err)
	}
	inbox := call("inbox_read", map[string]any{"project_id": f.project.ProjectID}, false)
	call("inbox_mark_read", map[string]any{"project_id": f.project.ProjectID, "through": inbox["through"]}, false)
	call("inbox_mark_read", map[string]any{"project_id": f.project.ProjectID, "through": float64(9007199254740991)}, true)
	readInbox := call("inbox_read", map[string]any{"project_id": f.project.ProjectID}, false)
	if readInbox["read_through"] != inbox["through"] {
		t.Fatal("inbox position lost", readInbox, inbox)
	}
	cursor := ""
	var replayAfter any
	objects := 0
	for {
		snapshot := call("resync_read", map[string]any{"project_id": f.project.ProjectID, "cursor": cursor, "limit": 1}, false)
		if snapshot["replace"] != true {
			t.Fatal("resync lost scope replacement", snapshot)
		}
		if replayAfter == nil {
			replayAfter = snapshot["replay_after"]
		} else if replayAfter != snapshot["replay_after"] {
			t.Fatal("resync changed fixed replay boundary")
		}
		objects += len(snapshot["objects"].([]any))
		next, _ := snapshot["next_cursor"].(string)
		if next == "" {
			break
		}
		if next == cursor {
			t.Fatal("nonadvancing cursor")
		}
		cursor = next
	}
	if objects == 0 {
		t.Fatal("resync omitted all current objects")
	}
	call("events_read", map[string]any{"project_id": f.project.ProjectID, "after": fmt.Sprint(replayAfter), "limit": 1}, false)
	call("extension_commands", map[string]any{}, false)
	for _, tool := range []string{"extension_packages", "extension_enablements"} {
		denied := call(tool, map[string]any{}, true)
		if !strings.Contains(string(mustJSON(denied)), "FORBIDDEN") {
			t.Fatal("HumanOnly governance changed", denied)
		}
	}
	// Ordinary own-trash/restore preserve the exact reference and real bytes.
	data := []byte("synthetic ordinary trash bytes")
	ordinary := f.ingest(session.Context, "ordinary/agent-trash", data, *rightsOwned())
	preview := call("trash_preview", map[string]any{"request": ledger.TrashSelector{ProjectID: f.project.ProjectID, AssetID: ordinary.AssetID, WholeAsset: true, Reason: "Synthetic own grace recycling"}}, false)
	var trashRequest ledger.TrashRequest
	if err := json.Unmarshal(mustJSON(preview), &trashRequest); err != nil {
		t.Fatal(err)
	}
	trashed := call("trash_own", map[string]any{"request": trashRequest, "idempotency_key": "original-own-trash"}, false)
	var command commands.Receipt
	if err := json.Unmarshal(mustJSON(trashed), &command); err != nil {
		t.Fatal(err)
	}
	var entry ledger.TrashEntry
	if err := json.Unmarshal(command.ResponseSummary, &entry); err != nil || entry.State != "trashed" {
		t.Fatal(entry, err)
	}
	call("trash_read", map[string]any{"project_id": f.project.ProjectID, "id": entry.ID}, false)
	restore := ledger.RestoreRequest{ProjectID: f.project.ProjectID, TrashID: entry.ID, ExpectedRevision: entry.Revision, Reason: "Synthetic ordinary restore"}
	call("trash_restore", map[string]any{"request": restore, "idempotency_key": "original-own-restore"}, false)
	directory := filepath.Join(work, "restored")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Pull(t.Context(), string(ordinary.AssetID), string(ordinary.VersionID), directory, "archive_review"); err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(filepath.Join(directory, "content.txt"))
	if err != nil || !bytes.Equal(actual, data) || shaOf(actual) != shaOf(data) {
		t.Fatal("restored exact bytes changed", err)
	}
	f.setRole(principal.ID, identity.RoleOwner, false)
	denied := call("milestone_list", map[string]any{"project_id": f.project.ProjectID}, true)
	if !strings.Contains(string(mustJSON(denied)), "FORBIDDEN") && !strings.Contains(string(mustJSON(denied)), "NOT_FOUND") {
		t.Fatal("revoked project still readable", denied)
	}
}
