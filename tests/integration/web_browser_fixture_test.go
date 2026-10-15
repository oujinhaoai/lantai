package integration

// This opt-in fixture exists only in a Go test binary. It creates a fresh,
// disposable real instance; no production data, key, config or credentials are
// read. Never expose its loopback listener or use it as a deployment server.
import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/application"
	"github.com/oujinhaoai/lantai/internal/catalog"
	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/operations"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

func TestWebBrowserFixture(t *testing.T) {
	if os.Getenv("LANTAI_WEB_FIXTURE") != "1" {
		t.Skip("opt-in disposable browser fixture")
	}
	port := os.Getenv("LANTAI_WEB_PORT")
	if port == "" {
		port = "4180"
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1024 || portNumber > 65535 {
		t.Fatal("LANTAI_WEB_PORT must be an unprivileged TCP port")
	}
	origin := "https://localhost:" + port
	dist, err := filepath.Abs("../../web/dist")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(dist, "index.html")); err != nil {
		t.Fatal("build web/dist before starting the browser fixture")
	}
	e := newEnv(t, storage.Config{}, transfer.Limits{})
	e.setRole(e.admin.Context.PrincipalID, identity.RoleOwner, true)
	for i := 0; i < 27; i++ {
		titles := []string{"镜头语言与场景参考", "角色设计说明", "声音素材索引"}
		title := titles[i%len(titles)]
		if i > 2 {
			title = "归档文档 " + string(rune('A'+i-3))
		}
		data := []byte("synthetic archive content")
		fileName := "content.txt"
		if i == 2 {
			fileName = "参考资料/跨团队声音素材与多语言环境声归档说明-very-long-multilingual-filename-for-responsive-layout.txt"
		}
		rights := manifest.Rights{Usage: "production", License: "CC0-1.0", RedistributeRaw: true, Sensitivity: "normal"}
		v, err := e.catalog.CommitVersion(t.Context(), catalog.VersionRequest{Who: e.admin.Context, IdempotencyKey: e.key(), UploadID: e.upload(e.admin.Context, data), Slug: "document-" + strconv.Itoa(i), Content: catalog.ContentInput{AssetType: manifest.TypeDoc, Rights: &rights, Files: []manifest.InputFile{{Path: fileName, Role: "primary", SHA256: shaOf(data), Size: int64(len(data))}}}})
		if err != nil {
			t.Fatal(err)
		}
		summary := "合成演示素材，用于验证中文检索、权限过滤与固定版本浏览。"
		tags := []string{"分镜", "参考", "合成数据"}
		if _, err := e.catalog.PatchAsset(t.Context(), e.admin.Context, e.key(), v.AssetID, 1, catalog.AssetPatch{Title: &title, Summary: &summary, Tags: &tags}); err != nil {
			t.Fatal(err)
		}
	}
	empty, err := e.catalog.CreateProject(t.Context(), catalog.ProjectRequest{Who: e.admin.Context, IdempotencyKey: e.key(), Key: "empty-studio", Name: "空白工作室", Summary: "已创建的项目，还没有资源。"})
	if err != nil {
		t.Fatal(err)
	}
	_, rev, err := e.id.Members(t.Context(), e.admin.Context, empty.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	e.sudo(&identity.SetProjectRole{ProjectID: empty.ProjectID, PrincipalID: e.admin.Context.PrincipalID, Role: identity.RoleViewer, ExpectedRevision: rev, Grant: true})
	baseProject := e.project
	mediaProject, err := e.catalog.CreateProject(t.Context(), catalog.ProjectRequest{Who: e.admin.Context, IdempotencyKey: e.key(), Key: "preview-lab", Name: "预览实验室"})
	if err != nil {
		t.Fatal(err)
	}
	e.project = mediaProject
	e.setRole(e.admin.Context.PrincipalID, identity.RoleOwner, true)
	mediaAssets := map[string]string{}
	seed := func(name string, kind manifest.AssetType, files map[string][]byte) {
		specs := []storage.FileSpec{}
		inputs := []manifest.InputFile{}
		for path, body := range files {
			specs = append(specs, storage.FileSpec{SHA256: shaOf(body), Size: int64(len(body))})
			inputs = append(inputs, manifest.InputFile{Path: path, Role: "primary", SHA256: shaOf(body), Size: int64(len(body))})
		}
		u, err := e.storage.CreateUpload(t.Context(), storage.CreateUploadRequest{Who: e.admin.Context, IdempotencyKey: e.key(), ProjectID: mediaProject.ProjectID, Files: specs})
		if err != nil {
			t.Fatal(err)
		}
		for _, body := range files {
			if _, err = e.storage.PutPart(t.Context(), storage.PartRequest{Who: e.admin.Context, UploadID: u.UploadID, SHA256: shaOf(body), PartNumber: 1, PartSHA256: shaOf(body), Size: int64(len(body)), Body: bytes.NewReader(body)}); err != nil {
				t.Fatal(err)
			}
			if _, err = e.storage.CompleteFile(t.Context(), e.admin.Context, u.UploadID, shaOf(body)); err != nil {
				t.Fatal(err)
			}
		}
		rights := manifest.Rights{Usage: "production", License: "CC0-1.0", RedistributeRaw: name != "restricted", Sensitivity: "normal"}
		v, err := e.catalog.CommitVersion(t.Context(), catalog.VersionRequest{Who: e.admin.Context, IdempotencyKey: e.key(), UploadID: u.UploadID, Slug: name, Content: catalog.ContentInput{AssetType: kind, Rights: &rights, Files: inputs}})
		if err != nil {
			t.Fatal(err)
		}
		mediaAssets[name] = string(v.AssetID)
	}
	for _, item := range []struct {
		name, file string
		kind       manifest.AssetType
	}{{"image", "sample.png", manifest.TypeImage}, {"audio", "sample.wav", manifest.TypeAudio}, {"video", "sample.mp4", manifest.TypeVideo}, {"restricted", "sample.png", manifest.TypeImage}} {
		body, err := os.ReadFile(filepath.Join("../fixtures/media", item.file))
		if err != nil {
			t.Fatal(err)
		}
		seed(item.name, item.kind, map[string][]byte{item.file: body})
	}
	seed("unsupported", manifest.TypeDoc, map[string][]byte{"archive.zip": []byte("PK synthetic archive"), "binary.bin": {0, 1, 2, 3}})
	seed("text", manifest.TypeDoc, map[string][]byte{
		"readme.md":     []byte("# 安全阅读\n\n**中文 Markdown**\n\n![外部图片](https://example.invalid/tracker.png)\n\n<script>window.previewExecuted=true</script>\n\n[危险链接](javascript:alert(1))\n"),
		"source.py":     []byte("# 合成代码，不执行\nprint('只读预览')\n"),
		"main.go":       []byte("package main\nfunc main() { println(\"只读\") }\n"),
		"index.html":    []byte("<script>window.previewExecuted=true</script><h1>不应执行</h1>\n"),
		"config.json":   []byte("{\"name\":\"合成项目\",\"enabled\":true}\n"),
		"config.yaml":   []byte("name: 合成项目\nenabled: true\n"),
		"config.toml":   []byte("name = \"合成项目\"\nenabled = true\n"),
		"long.txt":      []byte(strings.Repeat("这是一行可安全阅读的合成文本。\n", 15000)),
		"boundary.txt":  append(bytes.Repeat([]byte("a"), 65535), []byte("中\n")...),
		"unknown.txt":   {0xff, 0xfe, 0x80},
		"disguised.txt": []byte("PK synthetic renamed archive, never shown"),
		"archive.zip":   []byte("PK synthetic archive, never expanded"),
		"binary.bin":    {0, 1, 2, 3, 4},
		"broken.png":    []byte("synthetic invalid image, decoder must fail safely"),
	})
	e.setRole(e.admin.Context.PrincipalID, identity.RoleOwner, false)
	e.project = baseProject
	// Align with the real browser's clock after seeding, retaining full TOTP
	// verification/replay protection. Test expiry advances this same clock.
	e.clk.Set(time.Now().Add(time.Minute))
	a := reopenApplication(t, e)
	backupRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(backupRoot, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(backupRoot, application.BackupRootMarker), []byte("browser-fixture\n"), 0600); err != nil {
		t.Fatal(err)
	}
	a.ConfigureBackups(t.Context(), backupRoot, "browser-fixture")
	cfg := a.Instance.Config()
	cfg.Listen = operations.ListenConfig{API: "127.0.0.1:0", Transfer: "127.0.0.1:0", Operations: "127.0.0.1:0", Merged: true}
	cfg.HTTP.AllowedOrigins = []string{origin}
	servers, err := a.StartHTTP(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = servers.Shutdown(ctx)
	})
	upstream, err := url.Parse("http://" + servers.Addresses.API)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	var control sync.Mutex
	var backupHold *commands.Held
	t.Cleanup(func() {
		control.Lock()
		defer control.Unlock()
		if backupHold != nil {
			backupHold.Release()
		}
	})
	mediaEnabled := false
	stop := make(chan struct{})
	var stopped sync.Once
	mux := http.NewServeMux()
	mux.Handle("/api/", proxy)
	mux.Handle("/xfer/", proxy)
	mux.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("/__fixture/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("X-Lantai-Fixture") != "1" {
			http.NotFound(w, r)
			return
		}
		control.Lock()
		defer control.Unlock()
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/__fixture/login":
			_ = json.NewEncoder(w).Encode(map[string]string{"name": "ada", "password": adminPassword, "code": e.fresh(), "project": string(e.project.ProjectID)})
		case "/__fixture/code":
			_ = json.NewEncoder(w).Encode(map[string]string{"code": e.fresh()})
		case "/__fixture/backup-hold":
			// Hold the same shared barrier as a real in-flight background write.
			// Backup must drain this lease; no fake backup result is injected.
			if backupHold == nil {
				_, backupHold, err = a.Instance.Gate().Acquire(r.Context(), commands.Request{})
				if err != nil {
					http.Error(w, "fixture cannot acquire write lease", http.StatusConflict)
					return
				}
			}
			w.WriteHeader(http.StatusNoContent)
		case "/__fixture/backup-release":
			if backupHold != nil {
				backupHold.Release()
				backupHold = nil
			}
			w.WriteHeader(http.StatusNoContent)
		case "/__fixture/expire":
			e.clk.Advance(13 * time.Hour)
			w.WriteHeader(http.StatusNoContent)
		case "/__fixture/roles":
			e.setRole(e.admin.Context.PrincipalID, identity.RoleOwner, r.URL.Query().Get("enabled") == "true")
			w.WriteHeader(http.StatusNoContent)
		case "/__fixture/media", "/__fixture/media-off":
			wanted := r.URL.Path == "/__fixture/media"
			if mediaEnabled != wanted {
				_, rev, err := e.id.Members(t.Context(), e.login().Context, mediaProject.ProjectID)
				if err != nil {
					t.Fatal(err)
				}
				e.sudo(&identity.SetProjectRole{ProjectID: mediaProject.ProjectID, PrincipalID: e.admin.Context.PrincipalID, Role: identity.RoleOwner, ExpectedRevision: rev, Grant: wanted})
				mediaEnabled = wanted
			}
			_ = json.NewEncoder(w).Encode(mediaAssets)
		case "/__fixture/stop":
			stopped.Do(func() { close(stop) })
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	})
	files := http.FileServer(http.Dir(dist))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		if r.URL.Path != "/" && !strings.HasPrefix(r.URL.Path, "/assets/") && r.URL.Path != "/help" && !strings.HasPrefix(r.URL.Path, "/help/") {
			http.NotFound(w, r)
			return
		}
		files.ServeHTTP(w, r)
	})
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(mux)
	_ = server.Listener.Close()
	server.Listener = listener
	server.StartTLS()
	defer server.Close()
	t.Log("Disposable HTTPS browser fixture ready on " + origin)
	select {
	case <-stop:
	case <-t.Context().Done():
	}
}
