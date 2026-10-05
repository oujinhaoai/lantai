package integration

import (
	"context"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/catalog"
	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	tc "github.com/oujinhaoai/lantai/internal/contract/tasks"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/ledger"
	"github.com/oujinhaoai/lantai/internal/operations"
	"github.com/oujinhaoai/lantai/internal/tasks"
	"github.com/oujinhaoai/lantai/internal/workflow"
)

// commitContext 新建（asset 为空）或追加项目上下文 Markdown 文档版本。
func (e *env) commitContext(who authz.Context, slug string, asset, base ids.ID, data []byte, task *catalog.TaskBinding) catalog.VersionResult {
	e.t.Helper()
	spec := catalog.ProjectDocument{Contract: catalog.ProjectDocumentContract, Kind: "context", Title: "Project rules", Scope: catalog.DocumentScope{Kind: "project"}, FilePath: "rules.md", Supersedes: []ids.PermanentRef{}}
	req := catalog.VersionRequest{Who: who, IdempotencyKey: e.key(), UploadID: e.upload(who, data), AssetID: asset, BaseVersionID: base, Task: task,
		Content: catalog.ContentInput{AssetType: manifest.TypeDoc, Files: []manifest.InputFile{{Path: "rules.md", Role: "primary", SHA256: shaOf(data), Size: int64(len(data))}}}}
	if asset == "" {
		req.Slug, req.Content.Rights = slug, rightsOwned()
	}
	v, err := e.catalog.CommitProjectDocument(e.t.Context(), req, spec)
	if err != nil {
		e.t.Fatal(err)
	}
	return v
}

// The fully assembled application (not hand-wired modules) must fix the current
// approved context into new tasks and answer @published/@approved from the ledger.
// The context document is delivered, checked, QA-passed, approved by a human and
// auto-published through a real Flow before the application is reopened.
func TestM2ApplicationWiresContextAuthorityAndPublicationSelectors(t *testing.T) {
	f := newFlowEnv(t)
	ctx := t.Context()
	started, err := f.flows.Start(ctx, f.owner, f.key(), workflow.StartRequest{ProjectID: f.project.ProjectID, DefinitionRef: f.definition.Ref, ProfileRef: f.profile.Ref, Title: "Project rules",
		AcceptanceCriteria: []string{"rules are reviewed"}, ExpectedOutputs: []tc.OutputRequirement{{Slug: "rules", AssetType: "doc", CandidateCount: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	f.dispatch()
	taskID := f.latest(f.flow(started.FlowID), "produce").StepRun.TaskIDs[0]
	a := f.claim(f.maker, taskID)
	rules := f.commitContext(f.maker, "rules", "", "", []byte("# Rules\n\nUse synthetic names only.\n"), &catalog.TaskBinding{TaskID: taskID, AttemptID: a.ID, LeaseFence: a.Fence.LeaseFence})
	if _, err = f.tasks.Submit(ctx, f.maker, f.key(), tasks.SubmitRequest{AttemptRef: attemptRef(f, taskID, a), OutputRefs: []ids.PermanentRef{rules.Ref}}); err != nil {
		t.Fatal(err)
	}
	f.sync()
	f.dispatch()
	if _, err = f.flows.SyncJobs(ctx, f.owner, 10); err != nil {
		t.Fatal(err)
	}
	f.dispatch()
	f.qa(started.FlowID, "pass")
	f.sync()
	f.dispatch()
	f.decide(f.flow(started.FlowID).Meta.ReviewTargetID, "approve")
	f.sync()
	f.dispatch()
	f.sync()
	if v := f.flow(started.FlowID); v.Flow.State != "completed" {
		t.Fatalf("flow must approve and publish the rules: %+v", v.Flow)
	}

	app := reopenApplication(t, f.env)
	owner := f.login().Context
	ref := func(selector string) string { return f.project.Key + "/rules@" + selector }
	wantRef := func(selector string, want ids.ID) {
		t.Helper()
		got, err := app.Catalog.Resolve(ctx, owner, ref(selector), 0)
		if err != nil || got.VersionID != want {
			t.Fatal(selector, got.VersionID, want, err)
		}
	}
	wantCode := func(selector string, code errcode.Code) {
		t.Helper()
		if _, err := app.Catalog.Resolve(ctx, owner, ref(selector), 0); errcode.CodeOf(err) != code {
			t.Fatal(selector, code, err)
		}
	}
	// BUG-20261001-05: the assembled catalog reads the current ledger pointers.
	wantRef("published", rules.VersionID)
	wantRef("approved", rules.VersionID)

	cfg := app.Instance.Config()
	cfg.Listen = operations.ListenConfig{API: "127.0.0.1:0", Transfer: "127.0.0.1:0", Operations: "127.0.0.1:0", Merged: true}
	servers, err := app.StartHTTP(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := servers.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	}()
	login := f.login()
	c, err := client.New(client.Config{BaseURL: "http://" + servers.Addresses.API, SessionToken: login.Token, AllowHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	call := func(method, path string, in, out any) {
		t.Helper()
		r, err := c.Do(ctx, method, "/api/v1/"+path, in, client.Options{IdempotencyKey: f.key()})
		if err != nil {
			t.Fatal(path, err)
		}
		if out != nil {
			if err = r.Decode(out); err != nil {
				t.Fatal(err)
			}
		}
	}
	effective := func() catalog.ContextBundle {
		t.Helper()
		var b catalog.ContextBundle
		call(http.MethodGet, "context?project_id="+string(f.project.ProjectID)+"&asset_type=doc", nil, &b)
		return b
	}
	createTask := func(title string) tasks.View {
		t.Helper()
		var created tasks.Result
		call(http.MethodPost, "tasks", tasks.CreateRequest{ProjectID: f.project.ProjectID, Type: "question", Title: title, AcceptanceCriteria: []string{"answer with the fixed rules"}, Role: identity.RoleContributor, ContextAssetType: "doc"}, &created)
		var v tasks.View
		call(http.MethodGet, "tasks/"+string(created.TaskID), nil, &v)
		return v
	}
	refsOf := func(v tasks.View) []ids.PermanentRef {
		t.Helper()
		if v.Meta.Context == nil || v.Meta.Context.AssetType != "doc" {
			t.Fatalf("task did not fix a context snapshot: %+v", v.Meta.Context)
		}
		return v.Meta.Context.Refs
	}
	// BUG-20261001-03: a task created through the real REST entry fixes the
	// current approved context set, with the same digest the context API reports.
	approved := effective()
	if len(approved.Documents) != 1 || approved.Documents[0].Ref != rules.Ref {
		t.Fatalf("approved rules must be effective: %+v", approved.Documents)
	}
	first := createTask("question with rules")
	if refs := refsOf(first); !slices.Equal(refs, []ids.PermanentRef{rules.Ref}) || first.Meta.Context.Digest != approved.Digest {
		t.Fatal("task context snapshot", first.Meta.Context, approved.Digest)
	}

	// An unapproved draft becomes @latest but neither changes @approved/@published
	// nor enters the effective set fixed by later tasks.
	draft := f.commitContext(owner, "", rules.AssetID, rules.VersionID, []byte("# Rules\n\nDraft wording, not reviewed.\n"), nil)
	wantRef("latest", draft.VersionID)
	wantRef("approved", rules.VersionID)
	wantRef("published", rules.VersionID)
	second := createTask("question after a draft")
	if refs := refsOf(second); !slices.Equal(refs, []ids.PermanentRef{rules.Ref}) || second.Meta.Context.Digest != approved.Digest {
		t.Fatal("draft entered the fixed context", second.Meta.Context)
	}

	// Disabling the released version suspends publication and removes it from
	// @approved and from the effective set; tasks keep what they fixed.
	state, err := app.Ledger.VersionControl(ctx, rules.VersionID)
	if err != nil {
		t.Fatal(err)
	}
	var prepared struct {
		Challenge identity.Challenge `json:"challenge"`
	}
	call(http.MethodPost, "human/prepare", map[string]any{"items": []any{map[string]any{"kind": "control", "request": ledger.ControlMutation{Action: "ledger.disable_version", ProjectID: f.project.ProjectID, Kind: "version", ID: rules.VersionID, ExpectedRevision: state.Revision, Reason: "synthetic withdrawal of the rules"}}}}, &prepared)
	var grant identity.Grant
	call(http.MethodPost, "identity/challenges/"+string(prepared.Challenge.ChallengeID)+"/verify", map[string]string{"code": f.fresh()}, &grant)
	gr, err := c.Do(ctx, http.MethodGet, "/api/v1/human/grants/"+string(grant.GrantID), nil, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var items []identity.HumanGrantItem
	if err = gr.Decode(&items); err != nil || len(items) != 1 {
		t.Fatal(items, err)
	}
	call(http.MethodPost, "human/execute", map[string]any{"grant_id": grant.GrantID, "operation_id": items[0].OperationID}, nil)
	wantCode("published", errcode.NotPublished)
	wantCode("approved", errcode.NotFound)
	wantRef("latest", draft.VersionID)
	if b := effective(); len(b.Documents) != 0 {
		t.Fatalf("disabled rules stayed effective: %+v", b.Documents)
	}
	third := createTask("question after withdrawal")
	if refs := refsOf(third); len(refs) != 0 || third.Meta.Context.Digest == approved.Digest {
		t.Fatal("withdrawn rules fixed into a new task", third.Meta.Context)
	}
	var again tasks.View
	call(http.MethodGet, "tasks/"+string(first.Task.ID), nil, &again)
	if refs := refsOf(again); !slices.Equal(refs, []ids.PermanentRef{rules.Ref}) || again.Meta.Context.Digest != approved.Digest {
		t.Fatal("an existing task's fixed context changed", again.Meta.Context)
	}
}
