package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/application"
	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/operations"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

// TestWebManagedTransferLiveHTTPS runs actual browser TypeScript adapters in
// the existing Node 24 runtime against a real temporary Go application. It is
// opt-in because normal Go checks do not require Node or web/node_modules.
// Node supplies a small in-memory browser transport, not a real browser/sandbox.
func TestWebManagedTransferLiveHTTPS(t *testing.T) {
	if os.Getenv("LANTAI_WEB_NODE_INTEGRATION") != "1" {
		t.Skip("set LANTAI_WEB_NODE_INTEGRATION=1 for real TS-to-Go synthetic HTTPS acceptance")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("explicit web integration requires the existing Node 24 runtime")
	}
	version, err := exec.CommandContext(t.Context(), node, "--version").Output()
	if err != nil || !strings.HasPrefix(strings.TrimSpace(string(version)), "v24.") {
		t.Fatalf("explicit web integration requires Node 24, found %q: %v", strings.TrimSpace(string(version)), err)
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	web := filepath.Join(root, "web")
	for _, required := range []string{"unit/register.mjs", "tests/managed-transfer.live.mjs", "node_modules/typescript/package.json"} {
		if _, err := os.Stat(filepath.Join(web, required)); err != nil {
			t.Fatalf("explicit web integration needs existing %s; no dependencies will be downloaded: %v", required, err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	cfg := storage.Config{PartSize: 64 << 10, SinglePartMax: 64 << 10, MinFreeBytes: 1 << 20}
	e := newEnv(t, cfg, transfer.Limits{})
	e.setRole(e.admin.Context.PrincipalID, identity.RoleContributor, true)
	a := reopenApplication(t, e, cfg)

	var apiTarget, transferTarget atomic.Pointer[httputil.ReverseProxy]
	var countsMu sync.Mutex
	creates, commits := map[string]int{}, map[string]int{}
	front := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var proxy *httputil.ReverseProxy
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/v1/"):
			proxy = apiTarget.Load()
		case strings.HasPrefix(r.URL.Path, "/xfer/v1/"):
			proxy = transferTarget.Load()
		default:
			http.NotFound(w, r)
			return
		}
		if proxy == nil {
			http.Error(w, "test application not started", http.StatusServiceUnavailable)
			return
		}
		if r.Method == http.MethodPost {
			countsMu.Lock()
			if r.URL.Path == "/api/v1/uploads" {
				creates[r.Header.Get("Idempotency-Key")]++
			} else if strings.HasPrefix(r.URL.Path, "/api/v1/uploads/") && strings.HasSuffix(r.URL.Path, "/commit") {
				commits[r.Header.Get("Idempotency-Key")]++
			}
			countsMu.Unlock()
		}
		proxy.ServeHTTP(w, r)
	}))
	front.Config.ErrorLog = log.New(io.Discard, "", 0)
	front.StartTLS()
	t.Cleanup(front.Close)
	httpCfg := a.Instance.Config()
	httpCfg.Listen = operations.ListenConfig{API: "127.0.0.1:0", Transfer: "127.0.0.1:0", Operations: "127.0.0.1:0"}
	httpCfg.HTTP.AllowedOrigins = []string{front.URL}
	servers, err := a.StartHTTP(ctx, httpCfg, application.WithAccessLog(io.Discard))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := servers.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	for _, target := range []struct {
		address string
		store   *atomic.Pointer[httputil.ReverseProxy]
	}{{servers.Addresses.API, &apiTarget}, {servers.Addresses.Transfer, &transferTarget}} {
		u, err := url.Parse("http://" + target.address)
		if err != nil {
			t.Fatal(err)
		}
		proxy := httputil.NewSingleHostReverseProxy(u)
		proxy.ErrorLog = log.New(io.Discard, "", 0)
		target.store.Store(proxy)
	}
	// Only the synthetic public certificate is exported. The TLS private key
	// and all application key material stay inside the temporary Go fixture.
	ca := filepath.Join(t.TempDir(), "synthetic-ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: front.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	code := e.fresh()
	input, err := json.Marshal(map[string]any{
		"origin": front.URL, "project_id": e.project.ProjectID, "instance_id": a.Instance.InstanceID(),
		"name": "ada", "password": adminPassword, "code": code, "clock_ms": e.clk.Now().UnixMilli(),
	})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, node, "--import", "./unit/register.mjs", "./tests/managed-transfer.live.mjs")
	cmd.Dir, cmd.Stdin, cmd.WaitDelay = web, bytes.NewReader(input), 5*time.Second
	for _, variable := range os.Environ() {
		name, _, _ := strings.Cut(variable, "=")
		switch name {
		case "NODE_EXTRA_CA_CERTS", "NODE_OPTIONS", "NODE_TLS_REJECT_UNAUTHORIZED", "NODE_USE_ENV_PROXY", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy":
			continue
		}
		cmd.Env = append(cmd.Env, variable)
	}
	cmd.Env = append(cmd.Env, "NODE_EXTRA_CA_CERTS="+ca, "NODE_TLS_REJECT_UNAUTHORIZED=1")
	var output, diagnostic bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &diagnostic
	if err := cmd.Run(); err != nil {
		t.Fatalf("live TypeScript-to-Go acceptance failed: %v\n%s", err, diagnostic.String())
	}
	type observation struct {
		Name       string           `json:"name"`
		Input      client.PushInput `json:"input"`
		State      client.PushState `json:"state"`
		AssetID    ids.ID           `json:"asset_id"`
		VersionID  ids.ID           `json:"version_id"`
		CommitPOST int              `json:"commit_posts"`
	}
	var result struct {
		Schema string `json:"schema"`
		TLS    struct {
			TrustedCertificate    bool `json:"trusted_certificate"`
			UntrustedRejected     bool `json:"untrusted_rejected"`
			WrongHostnameRejected bool `json:"wrong_hostname_rejected"`
		} `json:"tls"`
		Cases  []observation `json:"cases"`
		Record observation   `json:"record"`
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatalf("decode bounded Node result: %v", err)
	}
	if result.Schema != "lantai.web-managed-transfer-live/v1" || !result.TLS.TrustedCertificate || !result.TLS.UntrustedRejected || !result.TLS.WrongHostnameRejected || len(result.Cases) != 3 || result.Record.Name != "management-record" {
		t.Fatal("Node result omitted required cases or strict TLS evidence")
	}
	wantCases := map[string]bool{"normal": false, "metadata-null": false, "unicode-html": false}
	for _, row := range result.Cases {
		if _, ok := wantCases[row.Name]; !ok || wantCases[row.Name] {
			t.Fatal("unexpected or duplicate live input case")
		}
		wantCases[row.Name] = true
	}
	who := e.login().Context
	seen := map[ids.ID]bool{}
	for _, row := range append(result.Cases, result.Record) {
		if !row.AssetID.Valid() || !row.VersionID.Valid() || seen[row.AssetID] || row.Input.ProjectID != string(e.project.ProjectID) || row.State.Origin != front.URL || row.State.Schema != "lantai.client-push/v1" || row.State.CreateKey == "" || row.State.CommitKey == "" || row.State.CreateKey == row.State.CommitKey {
			t.Fatalf("invalid fixed identity or original state in %s", row.Name)
		}
		seen[row.AssetID] = true
		// This establishes compatibility for these Node-generated lexical
		// inputs only, not arbitrary Go json.RawMessage documents/imports.
		rawInput, err := json.Marshal(row.Input)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(rawInput)
		if hex.EncodeToString(sum[:]) != row.State.RequestHash {
			t.Fatalf("Node-to-Go request_hash mismatch for generated %s input", row.Name)
		}
		versions, err := a.Ledger.AssetVersions(ctx, row.AssetID, 0, 10)
		if err != nil || len(versions) != 1 || versions[0].VersionID != row.VersionID || versions[0].VersionNumber != 1 || string(versions[0].OperationID) != row.State.OperationID {
			t.Fatalf("%s original-key replay created another version: %v", row.Name, err)
		}
		exact, err := a.Catalog.GetVersion(ctx, who, row.AssetID, row.VersionID)
		if err != nil {
			t.Fatalf("read accepted %s manifest: %v", row.Name, err)
		}
		installed, err := a.Storage.ReadVersionFiles(ctx, versions[0], 256<<10)
		if err != nil || len(installed) != len(row.Input.Content.Files) {
			t.Fatalf("read installed %s byte set: %v", row.Name, err)
		}
		for _, file := range row.Input.Content.Files {
			data, ok := installed[file.Path]
			sum := sha256.Sum256(data)
			if !ok || int64(len(data)) != file.Size || hex.EncodeToString(sum[:]) != file.SHA256 {
				t.Fatalf("installed byte digest mismatch for %s", row.Name)
			}
		}
		if row.Name == "management-record" {
			rights := exact.Manifest.Content.Rights
			if rights.Usage != "reference" || rights.License != "LicenseRef-Lantai-Private-Record" || rights.RedistributeRaw || !rights.NoAI || rights.Sensitivity != "normal" || exact.Manifest.Content.AssetType != "config" {
				t.Fatal("saved management record changed its explicit private rights")
			}
		}
		countsMu.Lock()
		createCount, commitCount := creates[row.State.CreateKey], commits[row.State.CommitKey]
		countsMu.Unlock()
		if createCount != 1 || commitCount != row.CommitPOST || (row.Name == "unicode-html" && commitCount != 2) || (row.Name != "unicode-html" && commitCount != 1) {
			t.Fatalf("%s changed its original create/commit operation: creates=%d commits=%d", row.Name, createCount, commitCount)
		}
		t.Logf("%s: HTTPS accepted one version; original-key create count=%d commit count=%d; Node/Go input hash and installed file SHA-256 agree", row.Name, createCount, commitCount)
	}
	t.Log("Node 24 strict TLS: trusted synthetic CA accepted; missing CA and wrong hostname rejected; no browser launched")
}
