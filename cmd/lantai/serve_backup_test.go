package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Exercise the actual CLI assembly, not only the controller constructor. A
// removable backup mount must never turn a healthy core into a restart loop.
func TestServeOptionalBackupConfigurationDoesNotBlockCore(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte("contract: lantai.config/v1\nstorage:\n  min_free_bytes: 1048576\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := interact(t, []string{"init", "-home", home, "-admin", "ada"}, []string{"synthetic startup password", "synthetic startup password"}, true); code != exitOK {
		t.Fatalf("fixture init failed: %d %s", code, stderr)
	}
	for _, mode := range []string{"unconfigured", "missing-root", "missing-marker"} {
		t.Run(mode, func(t *testing.T) {
			root := ""
			if mode != "unconfigured" {
				root = filepath.Join(t.TempDir(), "missing")
			}
			if mode == "missing-marker" {
				var err error
				root, err = filepath.EvalSymlinks(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("LANTAI_BACKUP_ROOT", root)
			t.Setenv("LANTAI_BACKUP_ROOT_ID", "synthetic-startup")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var out, stderr syncBuf
			done := make(chan int, 1)
			go func() {
				done <- runServe(ctx, []string{"-home", home, "-api-listen", "127.0.0.1:0", "-transfer-listen", "127.0.0.1:0", "-operations-listen", "127.0.0.1:0"}, &out, &stderr)
			}()
			var ready struct {
				Ready     bool `json:"ready"`
				Listeners struct {
					API string `json:"api"`
				} `json:"listeners"`
			}
			deadline := time.Now().Add(15 * time.Second)
			for !ready.Ready && time.Now().Before(deadline) {
				_ = json.Unmarshal([]byte(out.String()), &ready)
				select {
				case code := <-done:
					t.Fatalf("serve exited before ready: %d %s", code, stderr.String())
				default:
				}
				time.Sleep(5 * time.Millisecond)
			}
			if !ready.Ready {
				t.Fatalf("core did not become ready: %s", stderr.String())
			}
			client := &http.Client{Timeout: 3 * time.Second}
			resp, err := client.Get("http://" + ready.Listeners.API + "/api/v1/meta")
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("normal API unavailable: %d", resp.StatusCode)
			}
			if root != "" {
				if _, err := os.Lstat(filepath.Join(root, ".lantai-backup-root")); !os.IsNotExist(err) {
					t.Fatalf("unexpected marker creation: %v", err)
				}
			}
			cancel()
			select {
			case code := <-done:
				if code != exitOK {
					t.Fatalf("serve shutdown: %d %s", code, stderr.String())
				}
			case <-time.After(10 * time.Second):
				t.Fatal("serve did not stop")
			}
		})
	}
}
