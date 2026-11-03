package mcpserver

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
)

func TestUploadControlPagesRedactGrantsChangeAndRevocation(t *testing.T) {
	const secret = "SYNTHETIC_TRANSFER_TOKEN_MUST_NOT_APPEAR"
	var nonce atomic.Int32
	var changed, denied atomic.Bool
	var cancelled atomic.Int32
	cs := batchTestSession(t, "", func(w http.ResponseWriter, r *http.Request) {
		if denied.Load() {
			w.WriteHeader(403)
			_ = json.NewEncoder(w).Encode(errcode.New(errcode.Forbidden, "revoked").Envelope("synthetic"))
			return
		}
		if r.Method == http.MethodDelete && r.URL.Path == "/api/v1/uploads/"+resourceTestAsset {
			cancelled.Add(1)
			w.WriteHeader(204)
			return
		}
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/uploads/"+resourceTestAsset {
			t.Errorf("unexpected upload route %s %s", r.Method, r.URL)
			w.WriteHeader(500)
			return
		}
		parts := make([]int, 700)
		for i := range parts {
			parts[i] = i + 1
		}
		if changed.Load() {
			parts = append(parts, 701)
		}
		u := client.Upload{UploadID: resourceTestAsset, OperationID: resourceTestVersion, State: "open", Files: []client.UploadFile{{SHA256: strings.Repeat("a", 64), Size: 1000, PartSize: 1, PartCount: 1000, State: "receiving", ReceivedParts: parts, PartURLTemplate: "https://example.invalid/?token=" + secret + string(rune('a'+nonce.Add(1)))}, {SHA256: strings.Repeat("b", 64), Size: 10, PartSize: 1, PartCount: 10, State: "open", PartURLTemplate: secret}}}
		_ = json.NewEncoder(w).Encode(u)
	})
	call := func(name string, args any, wantError bool) map[string]any {
		t.Helper()
		r, v := resourceCall(t, cs, name, args)
		if r.IsError != wantError || strings.Contains(string(mustBatchJSON(t, r)), secret) {
			t.Fatal("bad upload result or grant leaked", name, v)
		}
		return v
	}
	first := call("upload_status", map[string]any{"upload_id": resourceTestAsset, "limit": 1}, false)
	if first["complete"] != false || first["total_files"] != float64(2) {
		t.Fatal(first)
	}
	hash := first["sha256"].(string)
	second := call("upload_status", map[string]any{"upload_id": resourceTestAsset, "limit": 1, "offset": 1, "expected_sha256": hash}, false)
	if second["complete"] != true || second["sha256"] != hash {
		t.Fatal("refreshed grant changed unchanged status identity", second)
	}
	parts := call("upload_parts", map[string]any{"upload_id": resourceTestAsset, "sha256": strings.Repeat("a", 64), "limit": 500, "expected_sha256": hash}, false)
	if len(parts["received_parts"].([]any)) != 500 || parts["complete"] != false {
		t.Fatal(parts)
	}
	last := call("upload_parts", map[string]any{"upload_id": resourceTestAsset, "sha256": strings.Repeat("a", 64), "offset": 500, "limit": 500, "expected_sha256": hash}, false)
	if len(last["received_parts"].([]any)) != 200 || last["complete"] != true {
		t.Fatal(last)
	}
	changed.Store(true)
	conflict := call("upload_status", map[string]any{"upload_id": resourceTestAsset, "offset": 1, "expected_sha256": hash}, true)
	if !strings.Contains(string(mustBatchJSON(t, conflict)), "PRECONDITION_FAILED") {
		t.Fatal(conflict)
	}
	denied.Store(true)
	refused := call("upload_parts", map[string]any{"upload_id": resourceTestAsset, "sha256": strings.Repeat("a", 64), "offset": 500, "expected_sha256": hash}, true)
	if !strings.Contains(string(mustBatchJSON(t, refused)), "FORBIDDEN") {
		t.Fatal(refused)
	}
	denied.Store(false)
	accepted := call("upload_cancel", map[string]any{"upload_id": resourceTestAsset}, false)
	if cancelled.Load() != 1 || accepted["recheck_required"] != true || accepted["request_accepted"] != true {
		t.Fatal(accepted)
	}
}

func TestUploadCheckAndCommitKeepExactOriginalRequest(t *testing.T) {
	const secret = "SYNTHETIC_EXTRA_GRANT_MUST_NOT_APPEAR"
	var commits atomic.Int32
	cs := batchTestSession(t, "", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/uploads/" + resourceTestAsset + "/check":
			if r.Method != http.MethodPost {
				t.Error(r.Method)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{map[string]any{"sha256": strings.Repeat("a", 64), "size": 1, "status": "granted", "authorization": secret}}})
		case "/api/v1/uploads/" + resourceTestAsset + "/commit":
			commits.Add(1)
			if r.Method != http.MethodPost || r.Header.Get("Idempotency-Key") != "original-commit-key" {
				t.Errorf("original commit key changed %s %s", r.Method, r.Header)
			}
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["slug"] != "original" {
				t.Errorf("original input changed %v %v", body, err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"asset_id": resourceTestAsset, "version_id": resourceTestVersion, "description_pending": true})
		default:
			t.Errorf("unexpected route %s", r.URL)
			w.WriteHeader(500)
		}
	})
	r, p := resourceCall(t, cs, "upload_check", map[string]any{"upload_id": resourceTestAsset, "request": map[string]any{"files": []any{map[string]any{"sha256": strings.Repeat("a", 64), "size": 1}}}})
	if r.IsError || strings.Contains(string(mustBatchJSON(t, r)), secret) || len(p["items"].([]any)) != 1 {
		t.Fatal(p)
	}
	r, _ = resourceCall(t, cs, "resource_commit", map[string]any{"upload_id": resourceTestAsset, "idempotency_key": "original-commit-key", "request": map[string]any{}})
	if !r.IsError || commits.Load() != 0 {
		t.Fatal("invalid commit reached HTTP")
	}
	request := map[string]any{"slug": "original", "content": map[string]any{"asset_type": "doc", "files": []any{map[string]any{"path": "data.txt", "role": "source", "sha256": strings.Repeat("a", 64), "size": 1}}, "rights": map[string]any{"usage": "production", "license": "LicenseRef-Owned", "sensitivity": "normal"}}}
	for range 2 {
		r, p := resourceCall(t, cs, "resource_commit", map[string]any{"upload_id": resourceTestAsset, "idempotency_key": "original-commit-key", "request": request})
		if r.IsError || p["description_pending"] != true {
			t.Fatal(p)
		}
	}
	if commits.Load() != 2 {
		t.Fatal("commit retry invented replacement request", commits.Load())
	}
}
