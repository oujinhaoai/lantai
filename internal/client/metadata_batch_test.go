package client

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

func TestMetadataBatchLostResponseKeepsOriginalKeysAndRejectsForgedAcceptance(t *testing.T) {
	asset, project, instance, principal := string(ids.New()), string(ids.New()), string(ids.New()), string(ids.New())
	revision := int64(1)
	title := "initial"
	mutations := 0
	lose := true
	receipts := map[string]json.RawMessage{}
	description := func() map[string]any {
		return map[string]any{"asset_id": asset, "project_id": project, "revision": revision, "title": title, "extra": map[string]any{"unknown": 1234567890123}}
	}
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/meta":
			_ = json.NewEncoder(w).Encode(map[string]any{"instance_id": instance})
		case r.URL.Path == "/api/v1/whoami":
			_ = json.NewEncoder(w).Encode(map[string]any{"principal": map[string]any{"principal_id": principal}})
		case r.Method == "GET" && r.URL.Path == "/api/v1/assets/"+asset:
			if r.URL.Query().Get("view") != "full" {
				t.Error("verification omitted unknown extra")
			}
			w.Header().Set("ETag", "\""+strconv.FormatInt(revision, 10)+"\"")
			_ = json.NewEncoder(w).Encode(map[string]any{"asset_id": asset, "description": description()})
		case r.Method == "PATCH" && r.URL.Path == "/api/v1/assets/"+asset+"/metadata":
			key := r.Header.Get("Idempotency-Key")
			if !ids.ID(key).Valid() {
				t.Error("missing original persisted key")
			}
			if receipt, ok := receipts[key]; ok {
				_, _ = w.Write(receipt)
				return
			}
			if r.Header.Get("If-Match") != "\""+strconv.FormatInt(revision, 10)+"\"" {
				w.WriteHeader(412)
				_ = json.NewEncoder(w).Encode(errcode.New(errcode.PreconditionFailed, "stale original revision").Envelope(""))
				return
			}
			var patch map[string]any
			if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
				t.Fatal(err)
			}
			title = patch["title"].(string)
			revision++
			mutations++
			raw, _ := json.Marshal(description())
			receipts[key] = raw
			if lose {
				lose = false
				w.WriteHeader(502)
				_ = json.NewEncoder(w).Encode(errcode.New(errcode.StorageUnavailable, "response lost after acceptance").Envelope(""))
				return
			}
			_, _ = w.Write(raw)
		default:
			t.Errorf("unexpected method/path %s %s", r.Method, r.URL)
			w.WriteHeader(404)
		}
	}))
	defer remote.Close()
	c, err := New(Config{BaseURL: remote.URL, AllowHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	plan, err := c.PlanMetadataBatch(t.Context(), MetadataBatchInput{Schema: MetadataBatchInputSchema, Items: []MetadataBatchItem{{AssetID: asset, Patch: json.RawMessage(`{"title":"next"}`)}}})
	if err != nil {
		t.Fatal(err)
	}
	if mutations != 0 {
		t.Fatal("planning wrote remote metadata")
	}
	state := filepath.Join(t.TempDir(), "state.json")
	first, err := c.ApplyMetadataBatch(t.Context(), plan, state)
	if err == nil || first.Complete || first.Results[0].Status != "unknown" || mutations != 1 {
		t.Fatal(first, err, mutations)
	}
	var saved metadataBatchState
	if err := LoadState(state, &saved); err != nil {
		t.Fatal(err)
	}
	key := saved.Rows[0].Key
	completed, err := c.ApplyMetadataBatch(t.Context(), plan, state)
	if err != nil || !completed.Complete || !completed.CurrentRemoteVerified || mutations != 1 {
		t.Fatal("original response loss duplicated write", completed, err, mutations)
	}
	if err := LoadState(state, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Rows[0].Key != key {
		t.Fatal("original key replaced")
	}
	saved.Rows[0].AcceptedSHA256 = "0000000000000000000000000000000000000000000000000000000000000000"
	if err := SaveState(state, saved); err != nil {
		t.Fatal(err)
	}
	forged, err := c.ApplyMetadataBatch(t.Context(), plan, state)
	if err == nil || forged.Complete || forged.CurrentRemoteVerified || mutations != 1 {
		t.Fatal("caller-edited receipt became current acceptance", forged, err)
	}
}
