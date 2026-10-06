package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/oujinhaoai/lantai/internal/catalog"
	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	tc "github.com/oujinhaoai/lantai/internal/contract/tasks"
	"github.com/oujinhaoai/lantai/internal/extensions"
	"github.com/oujinhaoai/lantai/internal/extensions/exttest"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/jobs"
	"github.com/oujinhaoai/lantai/internal/ledger"
	"github.com/oujinhaoai/lantai/internal/node"
	"github.com/oujinhaoai/lantai/internal/tasks"
	"github.com/oujinhaoai/lantai/internal/workflow"
)

// TEST-M2-11: hiding human tools from MCP is not sufficient. The public HTTP
// boundary must refuse ordinary sessions and recheck frozen grants at acceptance.
// All three domains have usable targets and a successful human control case.
func TestClientConsistencySensitive(t *testing.T) {
	bin := buildConsistencyCLI(t)
	for _, kind := range []string{"review", "early_purge", "extension_enable"} {
		t.Run(kind, func(t *testing.T) {
			f := newSensitiveFixture(t, bin, kind)
			grant, child := f.grant(t, f.intent)
			body := map[string]any{"grant_id": grant, "operation_id": child}
			var acceptedBody map[string]any
			deny := func(t *testing.T, h *consistencyClient, path string, input any, code errcode.Code, reason string) {
				t.Helper()
				before := f.snapshot(t)
				sensitiveCompare(t, h, path, input, code, reason)
				f.unchanged(t, before)
			}
			t.Run("owner_agent_cannot_prepare", func(t *testing.T) {
				deny(t, f.agent, "prepare", map[string]any{"items": []any{f.intent}}, errcode.Forbidden, "")
			})
			t.Run("read_only_agent_cannot_prepare", func(t *testing.T) {
				deny(t, f.readOnly, "prepare", map[string]any{"items": []any{f.intent}}, errcode.Forbidden, "")
			})
			t.Run("agent_cannot_read_stolen_grant", func(t *testing.T) {
				deny(t, f.agent, "grants/"+string(grant), nil, errcode.Forbidden, "")
			})
			t.Run("agent_cannot_execute_stolen_grant", func(t *testing.T) {
				deny(t, f.agent, "execute", body, errcode.Forbidden, "")
			})
			t.Run("human_missing_grant", func(t *testing.T) {
				deny(t, f.human, "execute", map[string]any{"grant_id": ids.New(), "operation_id": child}, errcode.NotFound, "")
			})
			t.Run("grant_is_bound_to_original_session", func(t *testing.T) {
				deny(t, f.otherSession, "execute", body, errcode.HumanGrantMismatch, "")
			})
			t.Run("child_from_other_action_is_refused", func(t *testing.T) {
				_, other := f.grant(t, map[string]any{"kind": "control", "request": ledger.ControlMutation{Action: "ledger.disable_version", ProjectID: f.project.ProjectID, Kind: "version", ID: f.spare.VersionID, ExpectedRevision: 1, Reason: "synthetic unrelated action"}})
				deny(t, f.human, "execute", map[string]any{"grant_id": grant, "operation_id": other}, errcode.HumanGrantMismatch, "")
			})
			t.Run("client_cannot_replace_frozen_request", func(t *testing.T) {
				deny(t, f.human, "execute", map[string]any{"grant_id": grant, "operation_id": child, "request": map[string]any{"kind": "control", "target_id": f.spare.VersionID}}, errcode.SchemaInvalid, "")
			})
			t.Run("unfinished_grant_expires", func(t *testing.T) {
				g, op := f.grant(t, f.intent)
				f.clk.Advance(6 * time.Minute)
				deny(t, f.human, "execute", map[string]any{"grant_id": g, "operation_id": op}, errcode.HumanProofRequired, "grant_expired")
			})
			t.Run("withdrawn_grant_is_refused", func(t *testing.T) {
				g, op := f.grant(t, f.intent)
				// Withdrawal has no public route; use the existing identity owner
				// API to change this fixture, then test actual remote execution.
				if err := f.id.RevokeGrant(t.Context(), f.who, g); err != nil {
					t.Fatal(err)
				}
				deny(t, f.human, "execute", map[string]any{"grant_id": g, "operation_id": op}, errcode.HumanProofRequired, "grant_revoked")
			})
			t.Run("mcp_annotation_cannot_expose_human_execution", func(t *testing.T) {
				before := f.snapshot(t)
				out, err := f.agent.mcp.CallTool(t.Context(), &mcp.CallToolParams{Name: "human_execute", Arguments: map[string]any{"grant_id": grant, "operation_id": child, "readOnlyHint": true}})
				if err == nil && (out == nil || !out.IsError) {
					t.Fatal("MCP accepted a human action")
				}
				out, err = f.agent.mcp.CallTool(t.Context(), &mcp.CallToolParams{Name: "whoami"})
				if err != nil || out.IsError {
					t.Fatal("sensitive-tool refusal was a broken stdio session", err)
				}
				f.unchanged(t, before)
			})
			t.Run("human_success_and_expired_grant_replay", func(t *testing.T) {
				g, op := f.grant(t, f.intent)
				input := map[string]any{"grant_id": g, "operation_id": op}
				acceptedBody = input
				first := sensitiveCompare(t, f.human, "execute", input, "", "")
				f.assertAccepted(t)
				before := f.snapshot(t)
				f.clk.Advance(6 * time.Minute)
				replay := sensitiveCompare(t, f.human, "execute", input, "", "")
				if !bytes.Equal(mustJSON(first), mustJSON(replay)) {
					t.Fatal("expired-grant replay did not retain the original result")
				}
				f.unchanged(t, before)
			})
			if kind == "extension_enable" {
				t.Run("server_enable_replay_does_not_repeat_probe", func(t *testing.T) {
					in := f.enable
					in.Target, in.Probe = "server", true
					g, op := f.grant(t, map[string]any{"kind": "extension_enable", "request": in})
					input := map[string]any{"grant_id": g, "operation_id": op}
					first := sensitiveCompare(t, f.human, "execute", input, "", "")
					activation, ok := first["activation"].(map[string]any)
					if !ok || activation["state"] != "ready" || activation["probe_id"] == "" {
						t.Fatal("positive server probe did not complete")
					}
					before := f.snapshot(t)
					f.clk.Advance(6 * time.Minute)
					replay := sensitiveCompare(t, f.human, "execute", input, "", "")
					if !bytes.Equal(mustJSON(first), mustJSON(replay)) {
						t.Fatal("server replay repeated or changed its probe")
					}
					f.unchanged(t, before)
				})
				t.Run("disable_replay_keeps_proof_and_original_generation", func(t *testing.T) {
					list, err := f.app.ExtensionManager.Enablements(t.Context(), f.who)
					if err != nil {
						t.Fatal(err)
					}
					var cliID ids.ID
					for _, v := range list {
						if v.Target == "cli" {
							cliID = v.ID
						}
					}
					g, op := f.grant(t, map[string]any{"kind": "extension_disable", "request": extensions.DisableRequest{EnablementID: cliID, Mode: "revoke", Reason: "synthetic remote disablement"}})
					input := map[string]any{"grant_id": g, "operation_id": op}
					first := sensitiveCompare(t, f.human, "execute", input, "", "")
					deny(t, f.otherSession, "execute", input, errcode.HumanGrantMismatch, "")
					deny(t, f.agent, "execute", input, errcode.Forbidden, "")
					// Re-enable the same logical ID with a new authorized operation.
					ng, no := f.grant(t, f.intent)
					sensitiveCompare(t, f.human, "execute", map[string]any{"grant_id": ng, "operation_id": no}, "", "")
					before := f.snapshot(t)
					f.clk.Advance(6 * time.Minute)
					replay := sensitiveCompare(t, f.human, "execute", input, "", "")
					if !bytes.Equal(mustJSON(first), mustJSON(replay)) {
						t.Fatal("old disable replay returned a later generation")
					}
					f.unchanged(t, before)
					if err := f.id.RevokeGrant(t.Context(), f.who, g); err != nil {
						t.Fatal(err)
					}
					// Completed items remain replayable without fresh proof, but
					// still require the original current human session and permission.
					sensitiveCompare(t, f.human, "execute", input, "", "")
					f.unchanged(t, before)
				})
			}
			t.Run("ended_session_cannot_replay", func(t *testing.T) {
				if err := f.id.EndSession(t.Context(), f.who, f.who.SessionID); err != nil {
					t.Fatal(err)
				}
				if acceptedBody == nil {
					t.Fatal("successful-operation fixture missing")
				}
				deny(t, f.human, "execute", acceptedBody, errcode.TokenRevoked, "")
			})
			t.Run("target_change_after_approval_is_refused", func(t *testing.T) {
				fresh := newSensitiveFixture(t, bin, kind)
				g, op := fresh.grant(t, fresh.intent)
				fresh.changeTarget(t)
				before := fresh.snapshot(t)
				code := errcode.ReviewTargetStale
				if kind == "early_purge" {
					code = errcode.PreconditionFailed
				} else if kind == "extension_enable" {
					code = errcode.PreconditionFailed
				}
				sensitiveCompare(t, fresh.human, "execute", map[string]any{"grant_id": g, "operation_id": op}, code, "")
				fresh.unchanged(t, before)
			})
			if kind == "review" {
				t.Run("review_role_revocation_after_approval_is_refused", func(t *testing.T) {
					fresh := newSensitiveFixture(t, bin, kind)
					g, op := fresh.grant(t, fresh.intent)
					fresh.setRole(fresh.who.PrincipalID, identity.RoleOwner, false)
					before := fresh.snapshot(t)
					sensitiveCompare(t, fresh.human, "execute", map[string]any{"grant_id": g, "operation_id": op}, errcode.NotFound, "")
					fresh.unchanged(t, before)
				})
			}
		})
	}
}

type sensitiveFixture struct {
	*appFlow
	kind                                 string
	who                                  authz.Context
	human, otherSession, agent, readOnly *consistencyClient
	intent                               map[string]any
	version, spare                       catalog.VersionResult
	target                               ledger.ReviewTarget
	trash                                ledger.TrashEntry
	enable                               extensions.EnableRequest
}

func newSensitiveFixture(t *testing.T, bin, kind string) *sensitiveFixture {
	t.Helper()
	f := &sensitiveFixture{appFlow: newAppFlow(t), kind: kind}
	f.spare = f.ingest(f.owner, "sensitive-spare", []byte("synthetic separate target"), *rightsOwned())
	switch kind {
	case "review":
		f.version, f.target = sensitiveReviewTarget(t, f.appFlow)
		f.intent = map[string]any{"kind": "review", "request": ledger.ReviewDecision{Action: identity.ActRecordReview, TargetID: f.target.ID, ExpectedRevision: f.target.Revision, Verdict: "approve", Reason: "synthetic remote approval", Waivers: []ledger.ReviewWaiver{}}}
	case "early_purge":
		f.version = f.ingest(f.owner, "sensitive-trash", []byte("synthetic retained bytes"), *rightsOwned())
		f.trash = trashForTest(t, f.env, f.app.Lifecycle, f.owner, f.version.AssetID)
		f.intent = map[string]any{"kind": "trash_mutation", "request": ledger.TrashMutation{Action: identity.ActPurge, ProjectID: f.project.ProjectID, TrashID: f.trash.ID, ExpectedRevision: f.trash.Revision, FilesDigest: f.trash.FilesDigest, Reason: "synthetic early purge"}}
	case "extension_enable":
		pkgDir := t.TempDir()
		pkg := exttest.Write(t, pkgDir, exttest.Spec{Server: true, CLI: true})
		producer, err := f.app.Extensions.Producer(t.Context(), "org.lantai.corecheck.manifest")
		if err != nil {
			t.Fatal(err)
		}
		profile := f.profile("sensitive/package-profile", []manifest.AssetType{manifest.TypePlugin}, producer)
		f.review(profile, f.definition("sensitive/package-flow", "org.lantai.corecheck.manifest"), "org.lantai.corecheck.manifest", "plugin", func(task ids.ID, attempt tasks.Attempt) catalog.VersionResult {
			upload, inputs := f.uploadFiles(f.maker, readDir(t, pkgDir))
			v, err := f.catalog.CommitVersion(t.Context(), catalog.VersionRequest{Who: f.maker, IdempotencyKey: f.key(), UploadID: upload, Slug: "sensitive/plugin", Task: bindTo(task, attempt), Content: catalog.ContentInput{AssetType: manifest.TypePlugin, Rights: rightsOwned(), Files: inputs, Metadata: map[string]any{"extension_id": pkg.Manifest.ID, "extension_version": pkg.Manifest.Version}}})
			if err != nil {
				t.Fatal(err)
			}
			f.version = v
			return v
		})
		if _, err := f.app.ExtensionManager.Import(t.Context(), f.owner, f.key(), extensions.ImportRequest{AssetID: f.version.AssetID, VersionID: f.version.VersionID}); err != nil {
			t.Fatal(err)
		}
		f.enable = extensions.EnableRequest{ExtensionID: pkg.Manifest.ID, ExtensionVersion: pkg.Manifest.Version, PackageDigest: pkg.Digest, Target: "cli", ScopeKind: "instance", Config: json.RawMessage(`{"mode":"pass"}`), ConfigRevision: 1, Trust: extensions.TrustUnenforced, Reason: "synthetic remote enablement"}
		f.intent = map[string]any{"kind": "extension_enable", "request": f.enable}
	}
	principal, _ := f.env.agent("sensitive-owner@node", identity.RoleOwner)
	agentSessions := []identity.IssuedSession{}
	for _, scopes := range [][]identity.Scope{{identity.ScopeRead, identity.ScopeIngest, identity.ScopeOrganize, identity.ScopeTask}, {identity.ScopeRead}} {
		current, err := f.id.GetPrincipal(t.Context(), f.login().Context, principal.ID)
		if err != nil {
			t.Fatal(err)
		}
		credential := f.sudo(&identity.IssueCredential{PrincipalID: current.ID, ExpectedRevision: current.Revision, Scopes: scopes})
		session, err := f.id.ExchangeToken(t.Context(), credential.Secret, identity.SessionRequest{Channel: identity.ChannelCLI})
		if err != nil {
			t.Fatal(err)
		}
		agentSessions = append(agentSessions, session)
	}
	// Fixture writes occurred after application startup. Complete their event
	// and inbox/audit synchronization before checking readiness and listening,
	// as the other client-consistency fixtures do.
	if err := f.app.Sync(t.Context()); err != nil {
		t.Fatal("sensitive fixture synchronization failed", err)
	}
	servers := remoteServers(t, f.app, true)
	origin := "http://" + servers.Addresses.API
	human := f.login()
	f.who = human.Context
	f.human = newConsistencyClient(t, bin, origin, human.Token)
	f.otherSession = newConsistencyClient(t, bin, origin, f.login().Token)
	f.agent = newConsistencyClient(t, bin, origin, agentSessions[0].Token)
	f.readOnly = newConsistencyClient(t, bin, origin, agentSessions[1].Token)
	return f
}

// Only the initial profile is the existing pre-approved fixture. Production,
// corecheck, QA and the submitted review target use the real assembled modules.
func sensitiveReviewTarget(t *testing.T, f *appFlow) (catalog.VersionResult, ledger.ReviewTarget) {
	t.Helper()
	ctx := t.Context()
	producer, err := f.app.Extensions.Producer(ctx, "org.lantai.corecheck.manifest")
	if err != nil {
		t.Fatal(err)
	}
	profile := f.profile("sensitive/review-profile", []manifest.AssetType{manifest.TypeDoc}, producer)
	definition := f.definition("sensitive/review-flow", "org.lantai.corecheck.manifest")
	started, err := f.flows.Start(ctx, f.owner, f.key(), workflow.StartRequest{ProjectID: f.project.ProjectID, DefinitionRef: definition.Ref, ProfileRef: profile.Ref, Title: "sensitive review fixture", AcceptanceCriteria: []string{"valid synthetic document"}, ExpectedOutputs: []tc.OutputRequirement{{Slug: "out", AssetType: "doc", CandidateCount: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	f.dispatch()
	task := f.latest(f.flow(started.FlowID), "produce").StepRun.TaskIDs[0]
	attempt := f.claim(f.maker, task)
	v, err := f.commitDoc(f.maker, "sensitive/review", "", "", []byte("synthetic reviewed bytes"), bindTo(task, attempt))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.tasks.Submit(ctx, f.maker, f.key(), tasks.SubmitRequest{AttemptRef: attemptRef(f.flowEnv, task, attempt), OutputRefs: []ids.PermanentRef{v.Ref}}); err != nil {
		t.Fatal(err)
	}
	f.sync()
	f.dispatch()
	list, err := f.app.Jobs.List(ctx, f.owner, f.project.ProjectID, "", 100)
	if err != nil || len(list) != 1 {
		t.Fatal("real check job missing", err)
	}
	if _, err = f.app.Nodes.Observe(ctx, f.worker, f.key(), node.Observation{ProjectID: f.project.ProjectID, Capabilities: []string{"org.lantai.corecheck.manifest"}, Slots: 1}); err != nil {
		t.Fatal(err)
	}
	job, err := f.app.Jobs.Run(ctx, f.worker, f.key(), jobs.Control{JobID: list[0].ID, ExpectedRevision: list[0].Revision})
	if err != nil || job.State != "succeeded" {
		t.Fatal("real corecheck did not succeed", err)
	}
	if _, err = f.flows.SyncJobs(ctx, f.owner, 10); err != nil {
		t.Fatal(err)
	}
	f.dispatch()
	f.qa(started.FlowID, "pass")
	f.sync()
	f.dispatch()
	target, err := f.app.Reviews.Target(ctx, f.owner, f.flow(started.FlowID).Meta.ReviewTargetID)
	if err != nil {
		t.Fatal(err)
	}
	return v, target
}

func (f *sensitiveFixture) grant(t *testing.T, intent map[string]any) (ids.ID, ids.ID) {
	t.Helper()
	raw, status := consistencyFileHTTP(t, f.human, "POST", "/api/v1/human/prepare", map[string]any{"items": []any{intent}})
	var prepared struct {
		Challenge identity.Challenge `json:"challenge"`
	}
	if status != 201 || json.Unmarshal(raw, &prepared) != nil || prepared.Challenge.Summary == "" || !prepared.Challenge.RequestHash.Valid() {
		t.Fatal("real human challenge refused", status, string(raw))
	}
	raw, status = consistencyFileHTTP(t, f.human, "POST", "/api/v1/identity/challenges/"+string(prepared.Challenge.ChallengeID)+"/verify", map[string]string{"code": f.fresh()})
	var grant identity.Grant
	if status != 201 || json.Unmarshal(raw, &grant) != nil || !grant.GrantID.Valid() {
		t.Fatal("real TOTP proof refused", status)
	}
	raw, status = consistencyFileHTTP(t, f.human, "GET", "/api/v1/human/grants/"+string(grant.GrantID), nil)
	var items []identity.HumanGrantItem
	if status != 200 || json.Unmarshal(raw, &items) != nil || len(items) != 1 || !items[0].OperationID.Valid() {
		t.Fatal("real frozen batch missing", status)
	}
	return grant.GrantID, items[0].OperationID
}

// Refusals compare every machine-error field except the per-request ID. Human
// actions deliberately have no MCP counterpart; Python uses its public request
// primitive, and CLI runs as an independent process.
func sensitiveCompare(t *testing.T, h *consistencyClient, action string, input any, expected errcode.Code, reason string) map[string]any {
	t.Helper()
	method, args := "POST", []string{"human", action}
	if input == nil {
		method, args = "GET", []string{"human", "items", "--id", action[len("grants/"):]}
	} else {
		args = append(args, "--input", writeTemp(t, input))
	}
	path := "/api/v1/human/" + action
	raw, status := consistencyFileHTTP(t, h, method, path, input)
	want := consistencyJSON(t, raw)
	failed := status >= 400
	if failed != (expected != "") {
		t.Fatalf("REST expected %s, HTTP=%d body=%s", expected, status, raw)
	}
	cliValue, exit := h.cli(t, args...)
	wantExit := 0
	if failed {
		wantExit = 1
		if expected == errcode.SchemaInvalid {
			wantExit = 2
		} else if status == 401 {
			wantExit = 5
		} else if status == 409 || status == 412 {
			wantExit = 4
		}
	}
	if exit != wantExit {
		t.Fatalf("CLI exit=%d, want=%d, HTTP=%d", exit, wantExit, status)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	sdk, err := filepath.Abs("../../sdk/python")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, python, "-c", consistencyPython)
	cmd.Env = append(consistencyEnv(), "PYTHONPATH="+sdk)
	cmd.Stdin = bytes.NewReader(mustJSON(map[string]any{"session": h.session, "method": "request", "args": []any{method, path, input}}))
	pyRaw, err := cmd.CombinedOutput()
	if err != nil || bytes.Contains(pyRaw, []byte(h.token)) {
		t.Fatal("Python sensitive request failed or leaked a credential", err)
	}
	pyValue := consistencyJSON(t, pyRaw)
	values := map[string]map[string]any{"REST": want, "CLI": cliValue, "Python": pyValue}
	if failed {
		for name, value := range values {
			body, ok := value["error"].(map[string]any)
			requestID, _ := body["request_id"].(string)
			if !ok || body["code"] != string(expected) || requestID == "" {
				t.Fatal(name, "lost or changed the machine error", value)
			}
			if reason != "" {
				found := false
				details, _ := body["details"].([]any)
				for _, item := range details {
					if detail, ok := item.(map[string]any); ok && detail["reason"] == reason {
						found = true
					}
				}
				if !found {
					t.Fatal(name, "did not identify the proof failure", body)
				}
			}
			delete(body, "request_id")
		}
	}
	for name, value := range map[string]map[string]any{"CLI": cliValue, "Python": pyValue} {
		if !bytes.Equal(mustJSON(want), mustJSON(value)) {
			t.Fatalf("%s differs from REST: REST=%s actual=%s", name, mustJSON(want), mustJSON(value))
		}
	}
	t.Logf("REST/CLI/Python identical; error=%s reason=%s HTTP=%d CLI_exit=%d", expected, reason, status, exit)
	return want
}

func (f *sensitiveFixture) snapshot(t *testing.T) []byte {
	t.Helper()
	state, err := f.ledger.VersionControl(t.Context(), f.version.VersionID)
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]any{"version": state}
	if f.kind == "early_purge" {
		entry, err := f.app.Lifecycle.Entry(t.Context(), f.owner, f.project.ProjectID, f.trash.ID)
		if err != nil {
			t.Fatal(err)
		}
		values["trash"] = entry
	}
	// These are read-only owner-local observations, never joins or fixture writes.
	for _, database := range []ownership.Database{ownership.Main, ownership.Ledger} {
		var count int
		if err := f.inst.DB(database).QueryRowContext(t.Context(), "SELECT count(*) FROM command_receipts").Scan(&count); err != nil {
			t.Fatal(err)
		}
		values[string(database)+"_receipts"] = count
	}
	for _, table := range []string{"extensions_activations", "extensions_invocations"} {
		var count int
		if err := f.inst.DB(ownership.Runtime).QueryRowContext(t.Context(), "SELECT count(*) FROM "+table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		values[table] = count
	}
	var enablements int
	if err := f.inst.DB(ownership.Main).QueryRowContext(t.Context(), "SELECT count(*) FROM extensions_enablements").Scan(&enablements); err != nil {
		t.Fatal(err)
	}
	values["enablements"] = enablements
	rows, err := f.inst.DB(ownership.Main).QueryContext(t.Context(), "SELECT record FROM extensions_enablements ORDER BY enablement_id")
	if err != nil {
		t.Fatal(err)
	}
	records := []string{}
	for rows.Next() {
		var record string
		if err := rows.Scan(&record); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	rows.Close()
	values["enablement_records"] = records
	files := map[string]string{}
	for _, area := range []string{"projects", "trash", "blobs"} {
		root := filepath.Join(f.inst.Layout().Home, area)
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if os.IsNotExist(err) {
				return nil
			}
			if err != nil || entry.IsDir() {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(f.inst.Layout().Home, path)
			if err != nil {
				return err
			}
			files[filepath.ToSlash(rel)] = string(digest.Of(data))
			return nil
		})
		if err != nil {
			t.Fatal("synthetic file inventory failed", err)
		}
	}
	values["files"] = files
	return mustJSON(values)
}

func (f *sensitiveFixture) unchanged(t *testing.T, before []byte) {
	t.Helper()
	if !bytes.Equal(before, f.snapshot(t)) {
		t.Fatal("refusal or receipt replay changed domain state, receipts, execution or stored bytes")
	}
	t.Log("domain state, receipt counts, activation/invocation counts and stored file digests unchanged")
}

func (f *sensitiveFixture) assertAccepted(t *testing.T) {
	t.Helper()
	switch f.kind {
	case "review":
		state, err := f.ledger.VersionControl(t.Context(), f.version.VersionID)
		if err != nil || state.ReviewState != "approved" || !state.EffectiveReviewID.Valid() {
			t.Fatal("positive review did not approve", err)
		}
	case "early_purge":
		entry, err := f.app.Lifecycle.Entry(t.Context(), f.owner, f.project.ProjectID, f.trash.ID)
		if err != nil || entry.State != "purged" {
			t.Fatal("positive early purge did not complete", err)
		}
	case "extension_enable":
		list, err := f.app.ExtensionManager.Enablements(t.Context(), f.who)
		if err != nil || len(list) != 1 {
			t.Fatal("positive enablement missing", err)
		}
	}
}

func (f *sensitiveFixture) changeTarget(t *testing.T) {
	t.Helper()
	var intent map[string]any
	switch f.kind {
	case "review":
		in := f.intent["request"].(ledger.ReviewDecision)
		in.Verdict, in.Reason = "return", "synthetic concurrent return"
		intent = map[string]any{"kind": "review", "request": in}
	case "early_purge":
		in := f.intent["request"].(ledger.TrashMutation)
		in.Action, in.Reason = identity.ActHold, "synthetic concurrent hold"
		intent = map[string]any{"kind": "trash_mutation", "request": in}
	case "extension_enable":
		intent = f.intent
	}
	g, op := f.grant(t, intent)
	raw, status := consistencyFileHTTP(t, f.human, "POST", "/api/v1/human/execute", map[string]any{"grant_id": g, "operation_id": op})
	if status != 200 {
		t.Fatal("concurrent target mutation refused", status, string(raw))
	}
	t.Logf("%s target changed through another actual human grant", f.kind)
}
