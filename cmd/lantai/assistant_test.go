package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/localassistant"
)

func TestAssistantCLIInspectApproveAndStructuredResult(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "sources"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sources/selected.txt"), []byte("synthetic CLI material"), 0600); err != nil {
		t.Fatal(err)
	}
	job := map[string]any{"schema": localassistant.JobSchema, "job_id": "cli-plan", "action": "import.plan", "directory": "sources", "output": "reports/result.json", "input": map[string]any{"schema": client.ImportInputSchema, "assets": []any{map[string]any{"key": "selected", "directory": ".", "classification": "final", "push": map[string]any{"project_id": "01ARZ3NDEKTSV4RRFFQ69G5FAV", "slug": "selected", "content": map[string]any{"asset_type": "doc", "files": []any{map[string]any{"path": "selected.txt", "role": "primary"}}}}}}}}
	jobPath := filepath.Join(root, "requests/job.json")
	if client.SaveState(jobPath, job) != nil {
		t.Fatal("cannot save job")
	}
	var stdout, stderr bytes.Buffer
	if code := runAssistant(t.Context(), []string{"inspect", "--workspace", root, "--job", "requests/job.json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("inspect code=%d: %s", code, stderr.String())
	}
	var inspection localassistant.Inspection
	if json.Unmarshal(stdout.Bytes(), &inspection) != nil || len(inspection.ApprovalDigest) != 64 {
		t.Fatal("missing approval digest")
	}
	stdout.Reset()
	stderr.Reset()
	if code := runAssistant(t.Context(), []string{"run", "--workspace", root, "--job", "requests/job.json"}, &stdout, &stderr); code == 0 || strings.Contains(stderr.String(), "synthetic CLI material") {
		t.Fatal("unapproved CLI execution or payload leak")
	}
	stdout.Reset()
	stderr.Reset()
	if code := runAssistant(t.Context(), []string{"run", "--workspace", root, "--job", "requests/job.json", "--approve", inspection.ApprovalDigest}, &stdout, &stderr); code != 0 {
		t.Fatalf("run code=%d: %s", code, stderr.String())
	}
	var result localassistant.Result
	if json.Unmarshal(stdout.Bytes(), &result) != nil || result.Status != "completed" || result.JobID != "cli-plan" || result.CredentialIssued {
		t.Fatalf("bad result: %s", stdout.Bytes())
	}
	if _, err := os.Stat(filepath.Join(root, "reports/result.json")); err != nil {
		t.Fatal("result not saved")
	}
}

func TestAssistantCLIRejectsArbitraryFlagsAndNoSilentCredentialEnvironment(t *testing.T) {
	for _, args := range [][]string{{"daemon"}, {"run", "--shell", "sh"}, {"inspect", "--workspace", "/tmp", "--job", "nothing.json", "--approve", strings.Repeat("a", 64)}, {"inspect", "--workspace", "/tmp", "--job", "nothing.json", "--session-file", "secret.json"}} {
		var stdout, stderr bytes.Buffer
		if code := runAssistant(t.Context(), args, &stdout, &stderr); code != 2 {
			t.Fatalf("args=%v code=%d", args, code)
		}
	}
}

func TestAssistantCLIPrivateResultReviewWithoutCredentialsOrNetwork(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	jobID := "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	body := json.RawMessage(`{"schema":"lantai.reference-plan/v1","source_root":"private-original","complete":true}`)
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	out := localassistant.Result{Schema: localassistant.ResultSchema, JobID: "local-plan", Action: "references.plan", Result: body, Status: "completed", ExitCode: 0}
	if err = client.SaveState(filepath.Join(root, ".lantai-assistant", "artifact-"+jobID+".json"), map[string]any{"schema": "lantai.assistant-worker-artifact/v1", "job_id": jobID, "config_digest": "synthetic", "project_id": jobID, "workspace_alias": "selected", "result": out}); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := runAssistant(t.Context(), []string{"result", "--workspace", root, "--job-id", jobID, "--result-sha256", digest}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "private-original") {
		t.Fatalf("local review code=%d %s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := runAssistant(t.Context(), []string{"result", "--workspace", root, "--job-id", jobID, "--result-sha256", strings.Repeat("0", 64)}, &stdout, &stderr); code == 0 || stdout.Len() != 0 {
		t.Fatal("wrong digest review produced original result")
	}
	for _, args := range [][]string{{"worker", "--workspace", root, "--config", "requests/worker.json", "--shell", "sh"}, {"enroll", "--workspace", root, "--config", "requests/worker.json", "--server", "https://other.invalid"}, {"result", "--workspace", root, "--job-id", jobID, "--result-sha256", digest, "--session-file", "secret.json"}} {
		if code := runAssistant(t.Context(), args, &stdout, &stderr); code != 2 {
			t.Fatalf("arbitrary device/result arguments accepted %v", args)
		}
	}
}
