package integration

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/ledger"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

func TestM2RemoteTrashRestoreConflictAndSharedRead(t *testing.T) {
	bin := buildConsistencyCLI(t)
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	_, agent := e.agent("remote-trash@node", identity.RoleContributor)
	app := reopenApplication(t, e)
	data := []byte("synthetic same-name shared blob")
	old := e.ingest(agent.Context, "remote-trash", data, *rightsOwned())
	servers := remoteServers(t, app, true)
	h := newConsistencyClient(t, bin, "http://"+servers.Addresses.API, agent.Token)
	c, err := client.New(client.Config{BaseURL: h.origin, SessionToken: agent.Token, AllowHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	request := ledger.TrashSelector{ProjectID: old.ProjectID, AssetID: old.AssetID, WholeAsset: true, Reason: "synthetic remote grace trash"}
	preview, exit := h.cli(t, "trash", "preview", "--input", writeTemp(t, request))
	if exit != 0 {
		t.Fatal(exit, preview)
	}
	var in ledger.TrashRequest
	if err = json.Unmarshal(mustJSON(preview), &in); err != nil {
		t.Fatal(err)
	}
	key := e.key()
	first, exit := h.cli(t, "trash", "own", "--input", writeTemp(t, in), "--idempotency-key", key)
	if exit != 0 {
		t.Fatal(exit, first)
	}
	replay, err := c.Do(t.Context(), http.MethodPost, "/api/v1/trash/own", in, client.Options{IdempotencyKey: key})
	if err != nil || !bytes.Equal(mustJSON(first), mustJSON(consistencyJSON(t, replay.Body))) {
		t.Fatal("REST/CLI replay changed the frozen receipt", err)
	}
	// The public lifecycle route returns a receipt carrying the entry summary.
	var receipt commands.Receipt
	if err = json.Unmarshal(replay.Body, &receipt); err != nil {
		t.Fatal(err)
	}
	var entry ledger.TrashEntry
	if err = json.Unmarshal(receipt.ResponseSummary, &entry); err != nil || entry.State != "trashed" {
		t.Fatal(entry, err)
	}
	current := e.ingest(agent.Context, "remote-trash", data, *rightsOwned())
	before, err := app.Lifecycle.Entry(t.Context(), agent.Context, old.ProjectID, entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	restore := ledger.RestoreRequest{ProjectID: old.ProjectID, TrashID: entry.ID, ExpectedRevision: entry.Revision, Reason: "synthetic restore must not overwrite"}
	refusal, exit := h.cli(t, "trash", "restore", "--input", writeTemp(t, restore), "--idempotency-key", e.key())
	if exit != 4 || refusal["error"].(map[string]any)["code"] != string(errcode.PathConflict) {
		t.Fatal("CLI did not diagnose the occupied path", exit, refusal)
	}
	var failure *client.APIError
	if _, err = c.Do(t.Context(), http.MethodPost, "/api/v1/trash/restore", restore, client.Options{IdempotencyKey: e.key()}); !errors.As(err, &failure) || failure.Body.Code != errcode.PathConflict {
		t.Fatal("REST path conflict differs", err)
	}
	after, err := app.Lifecycle.Entry(t.Context(), agent.Context, old.ProjectID, entry.ID)
	if err != nil || !bytes.Equal(mustJSON(before), mustJSON(after)) {
		t.Fatal("path rejection changed the trash entry", err)
	}
	restore.NewSlug = "remote-trash-restored"
	if result, exit := h.cli(t, "trash", "restore", "--input", writeTemp(t, restore), "--idempotency-key", e.key()); exit != 0 {
		t.Fatal(exit, result)
	}
	for _, v := range []struct{ asset, version string }{{string(old.AssetID), string(old.VersionID)}, {string(current.AssetID), string(current.VersionID)}} {
		raw, status := consistencyFileHTTP(t, h, "POST", "/api/v1/assets/"+v.asset+"/versions/"+v.version+"/read-grants", map[string]string{"path": "content.txt", "purpose": "archive_review"})
		var grant client.DownloadGrant
		if status != 201 || json.Unmarshal(raw, &grant) != nil || grant.Size != int64(len(data)) || grant.SHA256 != shaOf(data) {
			t.Fatal("exact permanent reference lost its authorized file", status)
		}
		raw, status = consistencyFileHTTP(t, h, "GET", grant.URL, nil)
		if status != 200 || !bytes.Equal(raw, data) || shaOf(raw) != grant.SHA256 {
			t.Fatal("shared content differs after restoring the old ID", status)
		}
	}
	t.Logf("REST/independent CLI: original ID=%s new ID=%s preserved; size=%d SHA256=%s; rejected path left entry unchanged", old.AssetID, current.AssetID, len(data), shaOf(data))
}
