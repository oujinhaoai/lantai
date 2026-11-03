package integration

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oujinhaoai/lantai/internal/cli"
	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

// The browser freezes real Push bytes, the official CLI archives them in a
// canonical management envelope, and the browser imports those same bytes.
func TestMigrationBrowserOriginalStateCLICanonicalRoundTrip(t *testing.T) {
	if os.Getenv("LANTAI_WEB_NODE_INTEGRATION") != "1" {
		t.Skip("set LANTAI_WEB_NODE_INTEGRATION=1 for real browser-model/CLI canonical roundtrip")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("explicit web integration requires Node 24:", err)
	}
	version, err := exec.CommandContext(t.Context(), node, "--version").Output()
	if err != nil || !strings.HasPrefix(string(version), "v24.") {
		t.Fatalf("explicit web integration requires Node 24, found %q: %v", version, err)
	}
	web, err := filepath.Abs("../../web")
	if err != nil {
		t.Fatal(err)
	}
	runNode := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), node, append([]string{"--import", "./unit/register.mjs", "tests/migration-archive-roundtrip.mjs"}, args...)...)
		cmd.Dir = web
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("real web-model roundtrip: %v\n%s", err, out)
		}
		t.Log(strings.TrimSpace(string(out)))
	}
	e := newEnv(t, storage.Config{}, transfer.Limits{})
	_, agent := e.agent("migration-lexical@synthetic", identity.RoleOwner)
	a := reopenApplication(t, e)
	front, ca, _ := recordsHTTPS(t, a)
	origin := front.URL
	c, err := client.New(client.Config{BaseURL: origin, SessionToken: agent.Token, CAFile: ca})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	root := t.TempDir()
	spec, prepared := filepath.Join(root, "spec.json"), filepath.Join(root, "prepared.json")
	source, read := filepath.Join(root, "source"), filepath.Join(root, "read")
	for _, dir := range []string{source, read} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	runNode("freeze", string(e.project.ProjectID), string(e.inst.InstanceID()), origin, spec)
	session := filepath.Join(root, "session.json")
	writeJSON(t, session, map[string]any{"schema": "lantai.client-session/v1", "origin": origin, "token": agent.Token})
	call := func(args ...string) map[string]any {
		t.Helper()
		var stdout, stderr bytes.Buffer
		args = append(args, "--server", origin, "--ca-file", ca, "--session-file", session)
		if code := cli.Run(t.Context(), args, &stdout, &stderr); code != 0 {
			t.Fatalf("official CLI: %d %s", code, stderr.String())
		}
		return consistencyJSON(t, stdout.Bytes())
	}
	call("management", "prepare", "--input", spec, "--output", prepared)
	receipt := call("management", "save", "--input", prepared, "--directory", source, "--state", filepath.Join(root, "push-state.json"))
	record, err := c.ReadManagementRecord(t.Context(), receipt["asset_id"].(string), receipt["version_id"].(string), read)
	if err != nil {
		t.Fatal(err)
	}
	bytesOnDisk, err := os.ReadFile(filepath.Join(read, "record.json"))
	if err != nil || !bytes.Equal(bytesOnDisk, record.Data) {
		t.Fatal("verifier must consume the actual read-verified canonical record.json", err)
	}
	runNode("verify", spec, filepath.Join(read, "record.json"))
}
