package cli

import (
	"bytes"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCAFileCLIFlagAndEnvironment(t *testing.T) {
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		respond(w, map[string]string{"instance_id": "synthetic"})
	}))
	t.Cleanup(s.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, fromEnv := range []bool{false, true} {
		var out, stderr bytes.Buffer
		args := []string{"meta", "--server", s.URL}
		if fromEnv {
			t.Setenv("LANTAI_CA_FILE", ca)
		} else {
			t.Setenv("LANTAI_CA_FILE", "")
			args = append(args, "--ca-file", ca)
		}
		if code := Run(t.Context(), args, &out, &stderr); code != 0 || !strings.Contains(out.String(), "synthetic") {
			t.Fatal(code, stderr.String())
		}
	}
	t.Setenv("LANTAI_CA_FILE", filepath.Join(t.TempDir(), "missing"))
	for _, args := range [][]string{{"meta", "--server", s.URL}, {"ext", "install", "--server", s.URL, "--package", "unused", "--registry", "unused"}} {
		var out, stderr bytes.Buffer
		if code := Run(t.Context(), args, &out, &stderr); code != 2 || !strings.Contains(stderr.String(), "CA bundle unavailable") {
			t.Fatal(code, stderr.String())
		}
	}
}
