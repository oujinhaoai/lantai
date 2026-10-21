package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
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

// TestWebMediaProfileRightsLiveHTTPS runs actual browser TypeScript adapters in
// the existing Node 24 runtime against a real temporary Go application. It is
// opt-in because normal Go checks do not require Node or web/node_modules.
// Node supplies a small in-memory browser transport, not a real browser/sandbox.
func TestWebMediaProfileRightsLiveHTTPS(t *testing.T) {
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
	for _, required := range []string{"unit/register.mjs", "tests/media-profile-rights.live.mjs", "node_modules/typescript/package.json"} {
		if _, err := os.Stat(filepath.Join(web, required)); err != nil {
			t.Fatalf("explicit web integration needs existing %s; no dependencies will be downloaded: %v", required, err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	cfg := storage.Config{PartSize: 64 << 10, SinglePartMax: 64 << 10, MinFreeBytes: 1 << 20}
	e := newEnv(t, cfg, transfer.Limits{})
	e.setRole(e.admin.Context.PrincipalID, identity.RoleOwner, true)
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
	pixels := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	pixels.Set(0, 0, color.NRGBA{R: 255, A: 255})
	pixels.Set(1, 1, color.NRGBA{G: 255, A: 255})
	var pngData, jpegData bytes.Buffer
	if err := png.Encode(&pngData, pixels); err != nil {
		t.Fatal(err)
	}
	if err := jpeg.Encode(&jpegData, pixels, nil); err != nil {
		t.Fatal(err)
	}
	fixtures := []map[string]any{
		{"name": "synthetic.png", "base64": base64.StdEncoding.EncodeToString(pngData.Bytes()), "width": 2, "height": 2},
		{"name": "synthetic.jpg", "base64": base64.StdEncoding.EncodeToString(jpegData.Bytes()), "width": 2, "height": 2},
		{"name": "synthetic.webp", "base64": "UklGRh4AAABXRUJQVlA4TBEAAAAvAAAAAAfQ//73v/+BiOh/AAA=", "width": 1, "height": 1},
		{"name": "synthetic.obj", "base64": base64.StdEncoding.EncodeToString([]byte("mtllib https://outside.synthetic.invalid/inert.mtl\nv 0 0 0\nv 1 0 0\nv 0 1 0\nf 1 2 3\n"))},
	}
	code := e.fresh()
	approvalCode := e.fresh()
	input, err := json.Marshal(map[string]any{
		"origin": front.URL, "project_id": e.project.ProjectID, "instance_id": a.Instance.InstanceID(),
		"name": "ada", "password": adminPassword, "code": code, "approval_code": approvalCode, "clock_ms": e.clk.Now().UnixMilli(), "fixtures": fixtures,
	})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, node, "--import", "./unit/register.mjs", "./tests/media-profile-rights.live.mjs")
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
		Name      string           `json:"name"`
		Input     client.PushInput `json:"input"`
		State     client.PushState `json:"state"`
		AssetID   ids.ID           `json:"asset_id"`
		VersionID ids.ID           `json:"version_id"`
	}
	var result struct {
		Schema            string        `json:"schema"`
		Cases             []observation `json:"cases"`
		Profile           bool          `json:"profile_unknown_extra_preserved"`
		Conflict          bool          `json:"profile_if_match_conflict"`
		MetadataNotRights bool          `json:"metadata_not_rights"`
		LoweringDenied    bool          `json:"ordinary_lowering_denied"`
		HumanRelease      bool          `json:"human_release"`
		Original          bool          `json:"original_manifest_retained"`
		Browser           bool          `json:"browser_executed"`
		TLS               struct {
			Trusted   bool `json:"trusted_certificate"`
			Untrusted bool `json:"untrusted_rejected"`
			WrongHost bool `json:"wrong_hostname_rejected"`
		} `json:"tls"`
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Schema != "lantai.web-media-profile-rights-live/v1" || len(result.Cases) != 4 || !result.Profile || !result.Conflict || !result.MetadataNotRights || !result.LoweringDenied || !result.HumanRelease || !result.Original || result.Browser || !result.TLS.Trusted || !result.TLS.Untrusted || !result.TLS.WrongHost {
		t.Fatal("missing actual media/profile/rights evidence")
	}
	seen := map[string]bool{}
	for _, row := range result.Cases {
		if seen[row.Name] || !row.AssetID.Valid() || !row.VersionID.Valid() {
			t.Fatal("invalid media identity")
		}
		seen[row.Name] = true
		raw, _ := json.Marshal(row.Input)
		sum := sha256.Sum256(raw)
		if hex.EncodeToString(sum[:]) != row.State.RequestHash {
			t.Fatal("Node/Go input digest disagrees")
		}
		versions, err := a.Ledger.AssetVersions(ctx, row.AssetID, 0, 10)
		if err != nil || len(versions) != 1 || versions[0].VersionID != row.VersionID {
			t.Fatal("unexpected media version set", err)
		}
		installed, err := a.Storage.ReadVersionFiles(ctx, versions[0], 256<<10)
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range row.Input.Content.Files {
			data, ok := installed[file.Path]
			sum := sha256.Sum256(data)
			if !ok || int64(len(data)) != file.Size || hex.EncodeToString(sum[:]) != file.SHA256 {
				t.Fatal("full media file hash mismatch")
			}
		}
		t.Logf("%s: actual TS adapter -> strict HTTPS -> Go grant and full byte SHA-256; one immutable version", row.Name)
	}
	t.Log("Structured profile unknown extra preserved; original-key replay and stale If-Match rejected; ordinary metadata did not authorize export; monotone rights restriction and new-evidence HumanGrant release passed; original manifest unchanged; no browser decoding/rendering/encoding claimed")
}
