package integration

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/oujinhaoai/lantai/internal/cli"
	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

func TestAssetImportCLIPlanApplyPendingResumeVerify(t *testing.T) {
	e := newEnv(t, storage.Config{}, transfer.Limits{})
	p, maker := e.agent("import-maker@synthetic", identity.RoleContributor)
	principals, err := e.id.ListPrincipals(t.Context(), e.login().Context)
	if err != nil {
		t.Fatal(err)
	}
	for _, current := range principals {
		if current.ID == p.ID {
			p = current
		}
	}
	issued := e.sudo(&identity.IssueCredential{PrincipalID: p.ID, ExpectedRevision: p.Revision, Scopes: []identity.Scope{identity.ScopeRead, identity.ScopeIngest}})
	limited, err := e.id.ExchangeToken(t.Context(), issued.Secret, identity.SessionRequest{Channel: identity.ChannelCLI})
	if err != nil {
		t.Fatal(err)
	}
	a := reopenApplication(t, e)
	servers := assetRemoteServers(t, a)
	baseURL := "http://" + servers.Addresses.API
	root := assetRealTempDir(t)
	src, state, dst := filepath.Join(root, "source"), filepath.Join(root, "state"), filepath.Join(root, "verified")
	if err = os.MkdirAll(filepath.Join(src, "textures"), 0700); err != nil {
		t.Fatal(err)
	}
	sourceFiles := map[string]string{"scene.txt": "synthetic scene uses textures/color.txt\n", "textures/color.txt": "synthetic color\n", "not-selected.txt": "unknown, retain locally\n"}
	for path, body := range sourceFiles {
		if err = os.WriteFile(filepath.Join(src, filepath.FromSlash(path)), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	push, err := client.ApplyAssetProfile(client.PushInput{ProjectID: string(e.project.ProjectID), Slug: "synthetic/import-cli", Content: client.ContentInput{AssetType: "model", Rights: mustJSON(rightsOwned()), Files: []client.InputFile{{Path: "scene.txt", Role: "primary"}, {Path: "textures/color.txt", Role: "texture"}}}}, client.AssetProfile{Characters: []string{"hero-a"}, Source: "synthetic", BusinessState: "active"})
	if err != nil {
		t.Fatal(err)
	}
	inputPath, planPath, reportPath := filepath.Join(root, "input.json"), filepath.Join(root, "plan.json"), filepath.Join(root, "report.json")
	input := client.ImportInput{Schema: client.ImportInputSchema, Assets: []client.ImportAsset{{Key: "scene", Directory: ".", Classification: "final", Push: push}}}
	if err = os.WriteFile(inputPath, mustJSON(input), 0600); err != nil {
		t.Fatal(err)
	}
	invoke := func(args ...string) (int, []byte, string) {
		t.Helper()
		var out, stderr bytes.Buffer
		code := cli.Run(t.Context(), append([]string{"asset"}, args...), &out, &stderr)
		return code, out.Bytes(), stderr.String()
	}
	t.Setenv("LANTAI_SERVER", "")
	t.Setenv("LANTAI_SESSION_TOKEN", "")
	if code, out, stderr := invoke("import", "plan", "--input", inputPath, "--directory", src, "--output", planPath); code != 0 || !bytes.Contains(out, []byte("not-selected.txt")) {
		t.Fatalf("offline plan=%d %s %s", code, out, stderr)
	}
	var plan client.ImportPlan
	if err = client.LoadState(planPath, &plan); err != nil || plan.SelectedFiles != 2 {
		t.Fatalf("plan: %+v %v", plan, err)
	}
	common := []string{"--server", baseURL, "--allow-http", "--input", planPath, "--directory", src, "--state", state, "--json"}
	applyArgs := append([]string{"import", "apply", "--output", reportPath}, common...)
	t.Setenv("LANTAI_SESSION_TOKEN", limited.Token)
	code, out, stderr := invoke(applyArgs...)
	var pending client.ImportReport
	if json.Unmarshal(out, &pending) != nil || code == 0 || pending.Complete || len(pending.Results) != 1 || pending.Results[0].Status != "description_pending" || pending.Results[0].VersionID == "" {
		t.Fatalf("pending apply=%d %s %s", code, out, stderr)
	}
	if strings.Contains(string(out)+stderr, limited.Token) {
		t.Fatal("session credential leaked")
	}
	t.Setenv("LANTAI_SESSION_TOKEN", maker.Token)
	code, out, stderr = invoke(applyArgs...)
	var report client.ImportReport
	if json.Unmarshal(out, &report) != nil || code != 0 || !report.Complete || report.Results[0].Status != "committed" || report.Results[0].AssetID != pending.Results[0].AssetID || report.Results[0].VersionID != pending.Results[0].VersionID || report.Results[0].OperationID != pending.Results[0].OperationID {
		t.Fatalf("resume apply=%d %s %s", code, out, stderr)
	}
	code, out, stderr = invoke(applyArgs...)
	var repeated client.ImportReport
	if json.Unmarshal(out, &repeated) != nil || code != 0 || !reflect.DeepEqual(report, repeated) {
		t.Fatalf("repeated apply=%d %s %s", code, out, stderr)
	}
	verifyArgs := append([]string{"import", "verify", "--report", reportPath, "--destination", dst}, common...)
	for range 2 {
		code, out, stderr = invoke(verifyArgs...)
		var verified client.ImportVerifyReport
		if json.Unmarshal(out, &verified) != nil || code != 0 || !verified.Complete || len(verified.Results) != 1 || verified.Results[0].Files != 2 {
			t.Fatalf("verify=%d %s %s", code, out, stderr)
		}
	}
	for path, body := range sourceFiles {
		got, readErr := os.ReadFile(filepath.Join(src, filepath.FromSlash(path)))
		if readErr != nil || string(got) != body {
			t.Fatalf("source changed %s: %q %v", path, got, readErr)
		}
	}
	if _, err = os.Stat(filepath.Join(dst, "scene", "not-selected.txt")); !os.IsNotExist(err) {
		t.Fatalf("unselected source copied: %v", err)
	}
	for _, path := range []string{"scene.txt", "textures/color.txt"} {
		got, readErr := os.ReadFile(filepath.Join(dst, "scene", filepath.FromSlash(path)))
		if readErr != nil || string(got) != sourceFiles[path] {
			t.Fatalf("verification differs %s: %q %v", path, got, readErr)
		}
	}
}
