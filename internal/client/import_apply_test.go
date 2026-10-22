package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

// This httptest peer models only existing REST upload/commit/manifest/read-grant
// endpoints. All payloads are generated strings, never local user material.
type importPeer struct {
	mu                                                                sync.Mutex
	createKeys                                                        map[string]string
	uploads                                                           map[string]Upload
	data                                                              map[string][]byte
	receipts                                                          map[string]map[string]any
	versions                                                          map[string][]InputFile
	versionAssets                                                     map[string]string
	instance                                                          string
	createCount, commitCount, commitCalls, operationCalls, grantCalls int
	loseCommit                                                        bool
	pending                                                           bool
	deny                                                              bool
	badManifest                                                       bool
	wrongOperation                                                    bool
	wrongManifestIdentity                                             string
	transferCalls                                                     int
	onCreate                                                          func()
	onCommit                                                          func()
	onGrant                                                           func()
	truncateDownload                                                  bool
	requestedRanges                                                   []string
}

func newImportPeer(t *testing.T) (*Client, *importPeer) {
	t.Helper()
	p := &importPeer{createKeys: map[string]string{}, uploads: map[string]Upload{}, data: map[string][]byte{}, receipts: map[string]map[string]any{}, versions: map[string][]InputFile{}, versionAssets: map[string]string{}, instance: string(ids.New())}
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { p.serve(t, w, r) })
	return c, p
}
func (p *importPeer) serve(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	case r.URL.Path == "/api/v1/meta" && r.Method == http.MethodGet:
		writeJSON(w, map[string]any{"api_version": "v1", "instance_id": p.instance})
	case r.URL.Path == "/api/v1/uploads" && r.Method == "POST":
		if p.deny {
			w.WriteHeader(http.StatusForbidden)
			writeJSON(w, errcode.New(errcode.Forbidden, "not authorized").Envelope("synthetic"))
			return
		}
		key := r.Header.Get("Idempotency-Key")
		if id := p.createKeys[key]; id != "" {
			writeJSON(w, p.uploads[id])
			return
		}
		var in struct {
			Files []InputFile `json:"files"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Error(err)
		}
		u := Upload{UploadID: string(ids.New()), OperationID: string(ids.New()), State: "open"}
		for _, f := range in.Files {
			u.Files = append(u.Files, UploadFile{SHA256: f.SHA256, Size: f.Size, PartSize: max(f.Size, 1), PartCount: 1, State: "pending", PartURLTemplate: "/xfer/import/" + u.UploadID + "/" + f.SHA256 + "/{part_number}"})
		}
		p.createKeys[key] = u.UploadID
		p.uploads[u.UploadID] = u
		p.createCount++
		if p.onCreate != nil {
			p.onCreate()
		}
		writeJSON(w, u)
	case len(parts) == 4 && parts[2] == "uploads" && r.Method == "GET":
		writeJSON(w, p.uploads[parts[3]])
	case len(parts) == 5 && parts[0] == "xfer" && parts[1] == "import":
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		if importSHA(b) != parts[3] || r.Header.Get("Lantai-Part-Sha256") != parts[3] {
			t.Error("wrong synthetic upload digest")
		}
		p.data[parts[3]] = b
		writeJSON(w, map[string]bool{"ok": true})
	case len(parts) == 7 && parts[2] == "uploads" && parts[4] == "files" && parts[6] == "complete":
		u := p.uploads[parts[3]]
		for i := range u.Files {
			if u.Files[i].SHA256 == parts[5] {
				u.Files[i].State = "verified"
			}
		}
		p.uploads[parts[3]] = u
		writeJSON(w, map[string]bool{"ok": true})
	case len(parts) == 5 && parts[2] == "uploads" && parts[4] == "commit":
		p.commitCalls++
		key := r.Header.Get("Idempotency-Key")
		receipt := p.receipts[key]
		if receipt == nil {
			var body PushInput
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			asset := body.AssetID
			if asset == "" {
				asset = string(ids.New())
			}
			version := string(ids.New())
			receipt = map[string]any{"asset_id": asset, "version_id": version, "operation_id": p.uploads[parts[3]].OperationID, "committed_at": "2026-10-18T00:00:00Z", "description_pending": p.pending, "description_error": "FORBIDDEN"}
			p.receipts[key] = receipt
			p.versions[version] = body.Content.Files
			p.versionAssets[version] = asset
			p.commitCount++
		}
		if p.loseCommit {
			p.loseCommit = false
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			conn.Close()
			return
		}
		if p.onCommit != nil {
			p.onCommit()
		}
		receipt["description_pending"] = p.pending
		if p.wrongOperation {
			copy := map[string]any{}
			for k, v := range receipt {
				copy[k] = v
			}
			copy["operation_id"] = string(ids.New())
			writeJSON(w, copy)
			return
		}
		writeJSON(w, receipt)
	case len(parts) == 4 && parts[2] == "operations":
		p.operationCalls++
		writeJSON(w, map[string]string{"operation_id": parts[3], "stage": "committed"})
	case len(parts) == 6 && parts[2] == "assets" && parts[4] == "versions":
		files := p.versions[parts[5]]
		if p.badManifest {
			files = append([]InputFile(nil), files...)
			files[0].SHA256 = importSHA([]byte("different"))
		}
		content := map[string]any{"files": files}
		raw, err := json.Marshal(content)
		if err != nil {
			t.Error(err)
		}
		canonical, err := canonjson.Canonicalize(raw)
		if err != nil {
			t.Error(err)
		}
		manifestDigest := digest.Of(canonical)
		version := map[string]any{"asset_id": p.versionAssets[parts[5]], "version_id": parts[5], "manifest_digest": manifestDigest}
		manifest := map[string]any{"instance_id": p.instance, "asset_id": p.versionAssets[parts[5]], "version_id": parts[5], "manifest_digest": manifestDigest, "content": content}
		switch p.wrongManifestIdentity {
		case "version-asset":
			version["asset_id"] = string(ids.New())
		case "version-id":
			version["version_id"] = string(ids.New())
		case "manifest-asset":
			manifest["asset_id"] = string(ids.New())
		case "manifest-id":
			manifest["version_id"] = string(ids.New())
		case "instance":
			manifest["instance_id"] = "invalid"
		case "digest":
			manifest["manifest_digest"] = digest.Of([]byte("wrong"))
			version["manifest_digest"] = manifest["manifest_digest"]
		case "digest-binding":
			manifest["manifest_digest"] = digest.Of([]byte("different"))
		}
		writeJSON(w, map[string]any{"version": version, "manifest": manifest})
	case len(parts) == 7 && parts[2] == "assets" && parts[6] == "read-grants":
		p.grantCalls++
		var req struct {
			Path    string `json:"path"`
			Purpose string `json:"purpose"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Purpose != "reference" {
			t.Errorf("purpose changed: %q", req.Purpose)
		}
		for _, f := range p.versions[parts[5]] {
			if f.Path == req.Path {
				if p.onGrant != nil {
					p.onGrant()
				}
				writeJSON(w, DownloadGrant{URL: "/xfer/import-read/" + f.SHA256, SHA256: f.SHA256, Size: f.Size, Path: f.Path})
				return
			}
		}
		w.WriteHeader(404)
	case len(parts) == 3 && parts[0] == "xfer" && parts[1] == "import-read":
		p.transferCalls++
		data := p.data[parts[2]]
		offset := 0
		if rg := r.Header.Get("Range"); rg != "" {
			p.requestedRanges = append(p.requestedRanges, rg)
			_, _ = fmt.Sscanf(rg, "bytes=%d-", &offset)
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, len(data)-1, len(data)))
			w.Header().Set("Content-Length", strconv.Itoa(len(data)-offset))
			w.WriteHeader(http.StatusPartialContent)
		} else {
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		}
		if p.truncateDownload {
			p.truncateDownload = false
			_, _ = w.Write(data[offset : offset+1])
			return
		}
		_, _ = w.Write(data[offset:])
	default:
		t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		w.WriteHeader(404)
	}
}
func TestImportApplyTwiceAndLostCommitResponseReuseOriginalOperation(t *testing.T) {
	root, in := importFixture(t)
	plan := importMustPlan(t, root, in)
	c, peer := newImportPeer(t)
	peer.loseCommit = true
	state := importStateDir(t)
	report, err := c.ApplyImport(context.Background(), plan, root, state)
	if err == nil || report.Complete || len(report.Results) != 1 || report.Results[0].Status != "unknown" {
		t.Fatalf("lost commit reported complete: %+v %v", report, err)
	}
	var first PushState
	if err = LoadState(filepath.Join(state, "scene.push.json"), &first); err != nil {
		t.Fatal(err)
	}
	if !first.Committing || first.OperationID == "" {
		t.Fatalf("missing original operation: %+v", first)
	}
	report, err = c.ApplyImport(context.Background(), plan, root, state)
	if err != nil || !report.Complete || report.Results[0].Status != "committed" {
		t.Fatalf("resume failed: %+v %v", report, err)
	}
	if _, err = c.ApplyImport(context.Background(), plan, root, state); err != nil {
		t.Fatal(err)
	}
	if peer.createCount != 1 || peer.commitCount != 1 || peer.commitCalls != 2 || peer.operationCalls != 1 {
		t.Fatalf("duplicate operation: %+v", peer)
	}
	var after PushState
	if err = LoadState(filepath.Join(state, "scene.push.json"), &after); err != nil {
		t.Fatal(err)
	}
	if first.CreateKey != after.CreateKey || first.CommitKey != after.CommitKey || first.OperationID != report.Results[0].OperationID {
		t.Fatal("replacement keys/operation created")
	}
}
func TestImportApplyDescriptionPendingAndBatchFailureRemainVisible(t *testing.T) {
	root, in := importFixture(t)
	a := in.Assets[0]
	a.Key = "second"
	a.Directory = "other"
	a.Push.Content.Files = []InputFile{{Path: "out.png", Role: "primary"}}
	a.Push.Content.AssetType = "image"
	importWrite(t, root, "other/out.png", "synthetic render")
	in.Assets = append(in.Assets, a)
	plan := importMustPlan(t, root, in)
	c, peer := newImportPeer(t)
	peer.pending = true
	state := importStateDir(t)
	report, err := c.ApplyImport(context.Background(), plan, root, state)
	if err == nil || report.Complete {
		t.Fatalf("pending commit failed: %+v %v", report, err)
	}
	for _, r := range report.Results {
		if r.Status != "description_pending" || r.ErrorCode != "FORBIDDEN" {
			t.Fatalf("pending hidden: %+v", r)
		}
	}
	if _, err = c.ApplyImport(context.Background(), plan, root, state); err == nil {
		t.Fatal("pending metadata declared complete")
	}
	if peer.commitCount != 2 || peer.createCount != 2 {
		t.Fatal("pending metadata triggered another version")
	}
	peer.pending = false
	completed, err := c.ApplyImport(context.Background(), plan, root, state)
	if err != nil || !completed.Complete || peer.commitCount != 2 || peer.createCount != 2 {
		t.Fatalf("original commit metadata reconciliation failed: %+v %v", completed, err)
	}
	encoded, _ := json.Marshal(report)
	if strings.Contains(string(encoded), "Bearer") || strings.Contains(string(encoded), "/xfer/") || strings.Contains(string(encoded), "url") {
		t.Fatal("transfer credentials in batch receipt")
	}
	// A definite authorization failure is separately reported and never causes
	// permissions to be changed or an initial metadata status to be upgraded.
	c2, denied := newImportPeer(t)
	denied.deny = true
	failed, err := c2.ApplyImport(context.Background(), plan, root, importStateDir(t))
	if err == nil || failed.Complete {
		t.Fatal("failed batch treated as complete")
	}
	for _, r := range failed.Results {
		if r.Status != "failed" || r.ErrorCode != "FORBIDDEN" {
			t.Fatalf("missing failure: %+v", r)
		}
	}
}
func TestImportApplyRejectsChangedInputOriginAndMissingOriginalState(t *testing.T) {
	root, in := importFixture(t)
	plan := importMustPlan(t, root, in)
	c, peer := newImportPeer(t)
	state := importStateDir(t)
	if _, err := c.ApplyImport(context.Background(), plan, root, state); err != nil {
		t.Fatal(err)
	}
	other, _ := newImportPeer(t)
	if _, err := other.ApplyImport(context.Background(), plan, root, state); err == nil {
		t.Fatal("origin switch accepted")
	}
	if err := os.Remove(filepath.Join(state, "scene.push.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ApplyImport(context.Background(), plan, root, state); err == nil {
		t.Fatal("missing state replaced")
	}
	if peer.createCount != 1 {
		t.Fatal("invented replacement upload")
	}
	importWrite(t, root, "project/main.blend", "changed")
	if _, err := c.ApplyImport(context.Background(), plan, root, importStateDir(t)); err == nil {
		t.Fatal("changed source uploaded")
	}
	if peer.createCount != 1 {
		t.Fatal("wrote before validation")
	}
}
func TestImportStateMustBeSeparatePrivateAndNonSymlink(t *testing.T) {
	root, in := importFixture(t)
	plan := importMustPlan(t, root, in)
	c, _ := newImportPeer(t)
	if _, err := c.ApplyImport(context.Background(), plan, root, filepath.Join(root, "state")); err == nil {
		t.Fatal("state nested in source accepted")
	}
	state := importStateDir(t)
	if os.PathSeparator != '\\' {
		if err := os.Chmod(state, 0755); err != nil {
			t.Fatal(err)
		}
		if _, err := c.ApplyImport(context.Background(), plan, root, state); err == nil {
			t.Fatal("public state accepted")
		}
		if err := os.Chmod(state, 0700); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(t.TempDir(), "state")
	if err := os.Symlink(state, link); err == nil {
		if _, err := c.ApplyImport(context.Background(), plan, root, link); err == nil {
			t.Fatal("symlink state accepted")
		}
	}
}

func TestImportApplyRejectsWrongOperationReceiptAndReconcilesOriginal(t *testing.T) {
	root, in := importFixture(t)
	plan := importMustPlan(t, root, in)
	c, peer := newImportPeer(t)
	peer.wrongOperation = true
	state := importStateDir(t)
	report, err := c.ApplyImport(context.Background(), plan, root, state)
	if err == nil || report.Complete || len(report.Results) != 1 || report.Results[0].Status != "unknown" {
		t.Fatalf("wrong-operation receipt accepted: %+v %v", report, err)
	}
	var saved PushState
	if err = LoadState(filepath.Join(state, "scene.push.json"), &saved); err != nil {
		t.Fatal(err)
	}
	if report.Results[0].OperationID != saved.OperationID {
		t.Fatal("lost original operation identity")
	}
	peer.wrongOperation = false
	report, err = c.ApplyImport(context.Background(), plan, root, state)
	if err != nil || !report.Complete || peer.commitCount != 1 || peer.createCount != 1 || report.Results[0].OperationID != saved.OperationID {
		t.Fatalf("original receipt reconciliation failed: %+v %v", report, err)
	}
}
