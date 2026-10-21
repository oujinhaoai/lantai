package localassistant

import (
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

// This synthetic TLS peer models only existing upload/commit/operation HTTP
// semantics. The assistant invokes the production Client.ApplyImport unchanged.
type importTLSFixture struct {
	t             *testing.T
	mu            sync.Mutex
	upload        client.Upload
	createKey     string
	commitKey     string
	createCount   int
	versionCount  int
	commitCalls   int
	operationGets int
	loseCommit    bool
	pending       bool
	receipt       map[string]any
	afterCommit   func()
	bytes         []byte
}

func (f *importTLSFixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	write := func(v any) { w.Header().Set("Content-Type", "application/json"); _ = json.NewEncoder(w).Encode(v) }
	if r.Header.Get("Authorization") != "Bearer synthetic-import-session" {
		f.t.Error("explicit selected session was not used")
		w.WriteHeader(401)
		return
	}
	switch {
	case r.Method == "POST" && r.URL.Path == "/api/v1/uploads":
		key := r.Header.Get("Idempotency-Key")
		if f.createKey != "" {
			if key != f.createKey {
				f.t.Error("retry invented another upload key")
			}
			write(f.upload)
			return
		}
		var in struct {
			Files []client.InputFile `json:"files"`
		}
		if json.NewDecoder(r.Body).Decode(&in) != nil || len(in.Files) != 1 {
			f.t.Error("invalid upload specs")
		}
		f.createKey = key
		f.createCount++
		f.upload = client.Upload{UploadID: string(ids.New()), OperationID: string(ids.New()), State: "open", Files: []client.UploadFile{{SHA256: in.Files[0].SHA256, Size: in.Files[0].Size, PartSize: max(in.Files[0].Size, 1), PartCount: 1, State: "pending", PartURLTemplate: "/xfer/v1/uploads/synthetic/{part_number}"}}}
		write(f.upload)
	case r.Method == "PUT" && r.URL.Path == "/xfer/v1/uploads/synthetic/1":
		f.bytes, _ = io.ReadAll(r.Body)
		if hash(f.bytes) != f.upload.Files[0].SHA256 || r.Header.Get("Lantai-Part-Sha256") != hash(f.bytes) {
			f.t.Error("original uploader lost part/file digest")
		}
		write(map[string]bool{"ok": true})
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/complete"):
		f.upload.Files[0].State = "verified"
		write(map[string]bool{"ok": true})
	case r.Method == "GET" && r.URL.Path == "/api/v1/uploads/"+f.upload.UploadID:
		write(f.upload)
	case r.Method == "POST" && r.URL.Path == "/api/v1/uploads/"+f.upload.UploadID+"/commit":
		key := r.Header.Get("Idempotency-Key")
		f.commitCalls++
		if f.commitKey == "" {
			f.commitKey = key
			f.versionCount++
			f.receipt = map[string]any{"asset_id": string(ids.New()), "version_id": string(ids.New()), "operation_id": f.upload.OperationID, "committed_at": "2026-10-20T00:00:00Z"}
		} else if f.commitKey != key {
			f.t.Error("retry invented another commit key")
		}
		f.receipt["description_pending"] = f.pending
		if f.afterCommit != nil {
			f.afterCommit()
			f.afterCommit = nil
			return
		}
		if f.loseCommit {
			f.loseCommit = false
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				f.t.Error(err)
				return
			}
			_ = conn.Close()
			return
		}
		write(f.receipt)
	case r.Method == "GET" && r.URL.Path == "/api/v1/operations/"+f.upload.OperationID:
		f.operationGets++
		write(map[string]string{"operation_id": f.upload.OperationID, "stage": "committed"})
	default:
		f.t.Errorf("unexpected HTTP path %s %s", r.Method, r.URL.Path)
		w.WriteHeader(404)
	}
}

func TestAssistantImportUnknownAndPendingResumeOriginalIdentityAndKeys(t *testing.T) {
	for _, scenario := range []string{"lost-commit", "description-pending"} {
		t.Run(scenario, func(t *testing.T) {
			root := workspace(t)
			job := importJob(t, root)
			planResult, err := executeJob(t, root, job)
			if err != nil {
				t.Fatal(err)
			}
			peer := &importTLSFixture{t: t, loseCommit: scenario == "lost-commit", pending: scenario == "description-pending"}
			server := httptest.NewUnstartedServer(http.HandlerFunc(peer.serve))
			server.Config.ErrorLog = log.New(io.Discard, "", 0)
			server.StartTLS()
			defer server.Close()
			if err := client.SaveState(filepath.Join(root, "credentials/session.json"), map[string]string{"schema": "lantai.client-session/v1", "origin": server.URL, "token": "synthetic-import-session"}); err != nil {
				t.Fatal(err)
			}
			if err := client.WritePrivate(filepath.Join(root, "credentials/ca.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})); err != nil {
				t.Fatal(err)
			}
			apply := Job{Schema: JobSchema, JobID: "original-import", Action: "import.apply", Origin: server.URL, Input: planResult.Result, Directory: "sources", State: "state/batch", Output: "reports/apply.json"}
			path := writeJob(t, root, apply)
			inspection, err := InspectFile(path, root)
			if err != nil {
				t.Fatal(err)
			}
			opts := Options{Workspace: root, ApprovalDigest: inspection.ApprovalDigest, SessionFile: "credentials/session.json", CAFile: "credentials/ca.pem"}
			first, err := RunFile(t.Context(), path, opts)
			if err == nil || first.Status == "completed" || first.ExitCode == 0 {
				t.Fatalf("incomplete first run=%+v err=%v", first, err)
			}
			if scenario == "lost-commit" && first.Status != "unknown" {
				t.Fatalf("unknown result became %s", first.Status)
			}
			if scenario == "description-pending" && first.Status != "partial" {
				t.Fatalf("pending became %s", first.Status)
			}
			var before client.PushState
			statePath := filepath.Join(root, "state/batch/selected.push.json")
			if client.LoadState(statePath, &before) != nil || before.CreateKey == "" || before.CommitKey == "" {
				t.Fatal("original keys were not retained")
			}
			peer.mu.Lock()
			peer.pending = false
			peer.mu.Unlock()
			second, err := RunFile(t.Context(), path, opts)
			if err != nil || second.Status != "completed" {
				t.Fatalf("original resume=%+v err=%v", second, err)
			}
			var after client.PushState
			if client.LoadState(statePath, &after) != nil || before.CreateKey != after.CreateKey || before.CommitKey != after.CommitKey || before.UploadID != after.UploadID || before.OperationID != after.OperationID {
				t.Fatal("resume changed original keys or upload/operation identity")
			}
			var report client.ImportReport
			if json.Unmarshal(second.Result, &report) != nil || !report.Complete || report.CurrentRemoteVerified || report.CompletionBasis != client.ImportHistoricalReceipts {
				t.Fatal("historical import receipt was upgraded into current verification")
			}
			peer.mu.Lock()
			counts := fmt.Sprintf("%d/%d/%d/%d", peer.createCount, peer.versionCount, peer.commitCalls, peer.operationGets)
			peer.mu.Unlock()
			if counts != "1/1/2/1" {
				t.Fatalf("unexpected create/version/commit/reconcile counts: %s", counts)
			}
			third, err := RunFile(t.Context(), path, opts)
			if err != nil || third.Status != "completed" || string(third.Result) != string(second.Result) {
				t.Fatal("historical complete replay did not retain exact receipt")
			}
			peer.mu.Lock()
			afterCounts := fmt.Sprintf("%d/%d/%d/%d", peer.createCount, peer.versionCount, peer.commitCalls, peer.operationGets)
			peer.mu.Unlock()
			if afterCounts != counts {
				t.Fatal("completed receipt replay performed another remote write")
			}
			original, _ := os.ReadFile(filepath.Join(root, "sources/selected.txt"))
			if string(original) != "synthetic selected material" {
				t.Fatal("import changed source")
			}
		})
	}
}
