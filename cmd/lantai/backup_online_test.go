package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/oujinhaoai/lantai/internal/application"
	"github.com/oujinhaoai/lantai/internal/operations"
)

func TestOnlineBackupCLIUsesRunningCoreAndPrivateSocket(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("Unix socket authority")
	}
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte("contract: lantai.config/v1\nstorage:\n  min_free_bytes: 1048576\n"), 0600); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := interact(t, []string{"init", "-home", home, "-admin", "ada"}, []string{"initial password secret", "initial password secret"}, true)
	if code != exitOK {
		t.Fatal(code, stderr)
	}
	a, err := application.Open(t.Context(), application.Options{Instance: operations.Options{Home: home}})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(context.Background())
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, application.BackupRootMarker), []byte("test-volume\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = a.EnableBackups(t.Context(), root, "test-volume"); err != nil {
		t.Fatal(err)
	}
	cfg := a.Instance.Config()
	cfg.Listen.API = "127.0.0.1:0"
	cfg.Listen.Transfer = "127.0.0.1:0"
	cfg.Listen.Operations = "127.0.0.1:0"
	srv, err := a.StartHTTP(t.Context(), cfg, application.WithAccessLog(io.Discard))
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown(context.Background())
	st, err := os.Lstat(application.BackupSocketPath(a.Instance.InstanceID()))
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatal(st, err)
	}
	if code, _, _ = runCLI(t, "backup", "-home", home, "-status"); code == exitOK {
		t.Fatal("offline command bypassed live instance lock")
	}
	code, out, stderr := runCLI(t, "backup-online", "-home", home)
	if code != exitOK {
		t.Fatal(code, stderr, out)
	}
	dec := json.NewDecoder(strings.NewReader(out))
	var start, done application.BackupJob
	if err = dec.Decode(&start); err != nil {
		t.Fatal(err)
	}
	if err = dec.Decode(&done); err != nil {
		t.Fatal(err)
	}
	if done.State != "complete" || done.ID != start.ID {
		t.Fatal(out)
	}
	if _, err = operations.VerifyBackup(t.Context(), filepath.Join(root, string(done.ID))); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr = runCLI(t, "backup-online", "-home", home, "-status", string(done.ID)); code != exitOK {
		t.Fatal(code, stderr)
	}
	// Ordinary TCP operations/API listeners never expose the backup control API.
	for _, addr := range []string{srv.Addresses.API, srv.Addresses.Operations} {
		resp, e := http.Get("http://" + addr + "/backup")
		if e != nil {
			t.Fatal(e)
		}
		_ = resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			t.Fatal("backup exposed on TCP")
		}
	}
	client := application.BackupClient(a.Instance.InstanceID())
	defer client.CloseIdleConnections()
	req, _ := http.NewRequest("GET", "http://lantai-backup/backup/"+string(done.ID), nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatal("missing instance authority accepted", resp.StatusCode)
	}
	// The client also refuses a locally replaced or shared endpoint before any
	// request is sent. Server-only permission checks cannot protect an idle core.
	socketPath := application.BackupSocketPath(a.Instance.InstanceID())
	if err = os.Chmod(socketPath, 0666); err != nil {
		t.Fatal(err)
	}
	unsafeClient := application.BackupClient(a.Instance.InstanceID())
	_, err = unsafeClient.Get("http://lantai-backup/backup/" + string(done.ID))
	unsafeClient.CloseIdleConnections()
	if restoreErr := os.Chmod(socketPath, 0600); restoreErr != nil {
		t.Fatal(restoreErr)
	}
	if err == nil {
		t.Fatal("client accepted a shared local endpoint")
	}

}
