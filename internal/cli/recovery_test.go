package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

func TestRecoveryCLIUsesOnlyExplicitInstallationClaim(t *testing.T) {
	root := t.TempDir()
	claim := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{3}, 32))
	claimFile := filepath.Join(root, "claim.json")
	if err := client.SaveState(claimFile, map[string]string{"schema": "lantai.first-start/v1", "instance_id": string(ids.New()), "code": claim}); err != nil {
		t.Fatal(err)
	}
	pack := filepath.Join(root, "selected.lantai")
	data := []byte("explicit synthetic package")
	if err := client.WritePrivate(pack, data); err != nil {
		t.Fatal(err)
	}
	hash, size, err := client.HashRecoveryPackage(t.Context(), pack)
	if err != nil {
		t.Fatal(err)
	}
	var origin string
	var paths []string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.Header.Get("Authorization") != "Bearer "+claim || r.Header.Get("Origin") != origin {
			t.Error("ordinary session replaced installation claim")
		}
		if r.URL.Path == "/setup/recovery/upload" {
			raw, err := io.ReadAll(r.Body)
			if err != nil || !bytes.Equal(raw, data) || r.ContentLength != size {
				t.Error("uploaded different package")
			}
			_ = json.NewEncoder(w).Encode(map[string]bool{"verified": true})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"phase": "uploaded", "package": map[string]any{"sha256": hash, "size": size}})
	}))
	defer s.Close()
	origin = s.URL
	t.Setenv("LANTAI_SESSION_TOKEN", "ordinary-agent-must-not-be-used")
	call := func(action string, extra ...string) (int, string, string) {
		t.Helper()
		var out, stderr bytes.Buffer
		args := []string{"recovery", action, "--server", origin, "--allow-http", "--claim-file", claimFile}
		args = append(args, extra...)
		exit := Run(t.Context(), args, &out, &stderr)
		return exit, out.String(), stderr.String()
	}
	if exit, out, err := call("upload", "--package", pack, "--package-sha256", hash); exit != 0 || !strings.Contains(out, hash) {
		t.Fatal(exit, out, err)
	}
	if len(paths) != 2 || paths[0] != "/setup/recovery/upload" || paths[1] != "/setup/recovery/status" {
		t.Fatal(paths)
	}
	for _, action := range []string{"admin", "admin-confirm", "begin"} {
		if exit, _, _ := call(action); exit == 0 {
			t.Fatal("factor/claim action exposed", action)
		}
	}
	if exit, _, _ := call("status", "--output", claimFile); exit == 0 {
		t.Fatal("claim overwritten")
	}
	raw, err := client.ReadPrivate(claimFile)
	if err != nil || !bytes.Contains(raw, []byte(claim)) {
		t.Fatal("original claim changed", err)
	}
}
