package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func TestMCPCAFileFlagAndEnvironmentBeforeSession(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing-ca.pem")
	for _, fromEnv := range []bool{false, true} {
		args := []string{"--server", "https://127.0.0.1"}
		if fromEnv {
			t.Setenv("LANTAI_CA_FILE", missing)
		} else {
			t.Setenv("LANTAI_CA_FILE", "")
			args = append(args, "--ca-file", missing)
		}
		var out, stderr bytes.Buffer
		if code := runMCP(t.Context(), args, &out, &stderr); code != 2 || !strings.Contains(stderr.String(), "CA bundle unavailable") || out.Len() != 0 {
			t.Fatal(code, stderr.String())
		}
	}
}
