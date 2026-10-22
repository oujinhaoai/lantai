package client

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"net/http"
	"path/filepath"
	"testing"
)

type importNoNetwork func(*http.Request) (*http.Response, error)

func (f importNoNetwork) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Older v1 reports must remain replayable without silently gaining a current
// authority claim or requiring read permission merely to recover a receipt.
func TestImportHistoricalReceiptNeverClaimsCurrentRemoteVerification(t *testing.T) {
	root, input := importFixture(t)
	plan := importMustPlan(t, root, input)
	c, err := New(Config{BaseURL: "https://same-origin.invalid", SessionToken: "synthetic-restored-session"})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	reject := importNoNetwork(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("review probe forbids all network")
	})
	c.api.Transport = reject
	c.transfer.Transport = reject
	state := importStateDir(t)
	a, v, op := string(ids.New()), string(ids.New()), string(ids.New())
	raw, err := json.Marshal(map[string]any{"asset_id": a, "version_id": v, "operation_id": op, "committed_at": "2026-10-18T18:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	request, err := json.Marshal(plan.Input.Assets[0].Push)
	if err != nil {
		t.Fatal(err)
	}
	push := PushState{Schema: "lantai.client-push/v1", Origin: c.Origin(), RequestHash: importSHA(request), CreateKey: string(ids.New()), CommitKey: string(ids.New()), OperationID: op, Result: raw}
	if err = SaveState(filepath.Join(state, "scene.push.json"), push); err != nil {
		t.Fatal(err)
	}
	old := ImportReport{Schema: ImportReportSchema, Origin: c.Origin(), PlanDigest: plan.Digest, Complete: true, Results: []ImportResult{{Key: "scene", Status: "committed", AssetID: a, VersionID: v, OperationID: op}}}
	legacy, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(legacy, &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "completion_basis")
	delete(fields, "current_remote_verified")
	if err = SaveState(filepath.Join(state, "import.json"), fields); err != nil {
		t.Fatal(err)
	}
	got, err := c.ApplyImport(context.Background(), plan, root, state)
	if err != nil || !got.Complete || calls != 0 {
		t.Fatalf("historical cache behavior changed: complete=%v calls=%d error=%v", got.Complete, calls, err)
	}
	if got.CompletionBasis != ImportHistoricalReceipts || got.CurrentRemoteVerified {
		t.Fatal("historical result claims remote verification", got)
	}
	verified, err := c.VerifyImport(context.Background(), plan, got, root, state, t.TempDir(), "archive_review")
	if err == nil || verified.Complete || verified.CurrentRemoteVerified || calls == 0 {
		t.Fatal("offline receipt or cache became remote verification", verified, err, calls)
	}
	got.CurrentRemoteVerified = true
	if err = SaveState(filepath.Join(state, "import.json"), got); err != nil {
		t.Fatal(err)
	}
	if _, err = c.ApplyImport(context.Background(), plan, root, state); err == nil {
		t.Fatal("import state accepted forged remote verification claim")
	}
}
