package integration

// This opt-in fixture exists only in a Go test binary. It creates a fresh,
// disposable real instance; no production data, key, config or credentials are
// read. Never expose its loopback listener or use it as a deployment server.
import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/catalog"
	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
	"github.com/oujinhaoai/lantai/internal/transport/httpapi"
)

func TestWebBrowserFixture(t *testing.T) {
	if os.Getenv("LANTAI_WEB_FIXTURE") != "1" {
		t.Skip("opt-in disposable browser fixture")
	}
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
	// Align with the real browser's clock after seeding, retaining full TOTP
	// verification/replay protection. Test expiry advances this same clock.
	e.clk.Set(time.Now().Add(time.Minute))
	a := reopenApplication(t, e)
	h, err := httpapi.New(httpapi.Deps{Identity: a.Identity, Catalog: a.Catalog, Storage: a.Storage, Query: a.Query, Operations: a}, httpapi.Config{InstanceID: a.Instance.InstanceID(), AllowedOrigins: []string{"https://localhost:4180"}})
	if err != nil {
		t.Fatal(err)
	}
	var control sync.Mutex
	stop := make(chan struct{})
	var stopped sync.Once
	mux := http.NewServeMux()
	mux.Handle("/api/", h.API())
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
		case "/__fixture/expire":
			e.clk.Advance(13 * time.Hour)
			w.WriteHeader(http.StatusNoContent)
		case "/__fixture/roles":
			e.setRole(e.admin.Context.PrincipalID, identity.RoleOwner, r.URL.Query().Get("enabled") == "true")
			w.WriteHeader(http.StatusNoContent)
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
		if r.URL.Path != "/" && !strings.HasPrefix(r.URL.Path, "/assets/") {
			http.NotFound(w, r)
			return
		}
		files.ServeHTTP(w, r)
	})
	listener, err := net.Listen("tcp", "127.0.0.1:4180")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(mux)
	_ = server.Listener.Close()
	server.Listener = listener
	server.StartTLS()
	defer server.Close()
	t.Log("Disposable HTTPS browser fixture ready on https://localhost:4180")
	select {
	case <-stop:
	case <-t.Context().Done():
	}
}
