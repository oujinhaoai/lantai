package integration

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

func TestMetadataBatchOriginalKeysPartialConflictAndCurrentVerification(t *testing.T) {
	e := newEnv(t, storage.Config{}, transfer.Limits{})
	principal, agent := e.agent("metadata-batch@synthetic", identity.RoleOwner)
	first := e.ingest(agent.Context, "metadata-batch/one", []byte("one"), *rightsOwned())
	second := e.ingest(agent.Context, "metadata-batch/two", []byte("two"), *rightsOwned())
	app := reopenApplication(t, e)
	servers := assetRemoteServers(t, app)
	c, err := client.New(client.Config{BaseURL: "http://" + servers.Addresses.API, SessionToken: agent.Token, AllowHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	read := func(asset ids.ID) (client.Response, map[string]any) {
		t.Helper()
		r, err := c.Do(t.Context(), "GET", "/api/v1/assets/"+string(asset)+"?view=full", nil, client.Options{})
		if err != nil {
			t.Fatal(err)
		}
		return r, consistencyJSON(t, r.Body)["description"].(map[string]any)
	}
	for _, asset := range []ids.ID{first.AssetID, second.AssetID} {
		r, _ := read(asset)
		if _, err := c.Do(t.Context(), "PATCH", "/api/v1/assets/"+string(asset)+"/metadata", map[string]any{"title": "initial", "extra": map[string]any{"unknown_retained": 1234567890123}}, client.Options{IfMatch: r.ETag, IdempotencyKey: string(ids.New())}); err != nil {
			t.Fatal(err)
		}
	}
	selection := client.MetadataBatchInput{Schema: client.MetadataBatchInputSchema, Items: []client.MetadataBatchItem{{AssetID: string(first.AssetID), Patch: json.RawMessage(`{"title":"first next","profile":{"schema":"lantai.asset-profile/v1","business_state":"active"}}`)}, {AssetID: string(second.AssetID), Patch: json.RawMessage(`{"title":"second next"}`)}}}
	plan, err := c.PlanMetadataBatch(t.Context(), selection)
	if err != nil {
		t.Fatal(err)
	}
	_, before := read(first.AssetID)
	if before["title"] != "initial" {
		t.Fatal("plan mutated metadata", before)
	}
	r, _ := read(second.AssetID)
	if _, err := c.Do(t.Context(), "PATCH", "/api/v1/assets/"+string(second.AssetID)+"/metadata", map[string]any{"title": "concurrent writer"}, client.Options{IfMatch: r.ETag, IdempotencyKey: string(ids.New())}); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(t.TempDir(), "metadata-state.json")
	report, err := c.ApplyMetadataBatch(t.Context(), plan, statePath)
	if err == nil || report.Complete || report.CurrentRemoteVerified || len(report.Results) != 2 || report.Results[0].Status != "verified" || report.Results[1].Status != "conflict" {
		t.Fatal("partial/conflict lost", report, err)
	}
	_, after := read(first.AssetID)
	if after["title"] != "first next" || after["extra"].(map[string]any)["unknown_retained"] != float64(1234567890123) {
		t.Fatal("unknown extra overwritten", after)
	}
	var stateBefore map[string]any
	if err := client.LoadState(statePath, &stateBefore); err != nil {
		t.Fatal(err)
	}
	secondReport, err := c.ApplyMetadataBatch(t.Context(), plan, statePath)
	if err == nil || !reflect.DeepEqual(report.Results, secondReport.Results) {
		t.Fatal("replay changed fixed partial results", secondReport, err)
	}
	var stateAfter map[string]any
	if err := client.LoadState(statePath, &stateAfter); err != nil {
		t.Fatal(err)
	}
	for i, row := range stateBefore["rows"].([]any) {
		if row.(map[string]any)["idempotency_key"] != stateAfter["rows"].([]any)[i].(map[string]any)["idempotency_key"] {
			t.Fatal("replay replaced original key")
		}
	}
	_, replayed := read(first.AssetID)
	if replayed["revision"] != after["revision"] {
		t.Fatal("successful row applied twice", replayed)
	}
	// Mutating a frozen intent cannot reuse its original state or keys.
	saved, err := client.ReadPrivate(statePath)
	if err != nil {
		t.Fatal(err)
	}
	changed := plan
	changed.Items = append([]client.MetadataBatchPlannedItem(nil), plan.Items...)
	changed.Items[0].Patch = json.RawMessage(`{"title":"different intent"}`)
	if _, err := c.ApplyMetadataBatch(t.Context(), changed, statePath); err == nil {
		t.Fatal("changed plan accepted")
	}
	actual, err := client.ReadPrivate(statePath)
	if err != nil || !bytes.Equal(saved, actual) {
		t.Fatal("invalid plan replaced original state", err)
	}
	// A later writer stays intact. Replaying the original accepted key returns
	// the same receipt, while the fresh read identifies a verification conflict.
	r, _ = read(first.AssetID)
	if _, err := c.Do(t.Context(), "PATCH", "/api/v1/assets/"+string(first.AssetID)+"/metadata", map[string]any{"title": "later writer"}, client.Options{IfMatch: r.ETag, IdempotencyKey: string(ids.New())}); err != nil {
		t.Fatal(err)
	}
	later, err := c.ApplyMetadataBatch(t.Context(), plan, statePath)
	if err == nil || later.Results[0].Status != "verification_failed" || later.CurrentRemoteVerified {
		t.Fatal("later revision silently overwritten", later, err)
	}
	_, current := read(first.AssetID)
	if current["title"] != "later writer" {
		t.Fatal(current)
	}
	e.setRole(principal.ID, identity.RoleOwner, false)
	denied, err := c.ApplyMetadataBatch(t.Context(), plan, statePath)
	if err == nil || denied.Complete || denied.CurrentRemoteVerified || len(denied.Results) != 2 || denied.Results[0].AcceptedSHA256 != report.Results[0].AcceptedSHA256 || denied.Results[0].Status != "verification_failed" {
		t.Fatal("revocation resurrected historical verification or lost acceptance", denied, err)
	}
}
