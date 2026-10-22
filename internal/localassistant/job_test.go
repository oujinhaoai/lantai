package localassistant

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
)

func workspace(t *testing.T) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"requests", "sources", "credentials"} {
		if err := os.Mkdir(filepath.Join(p, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

func writeJob(t *testing.T, root string, job Job) string {
	t.Helper()
	path := filepath.Join(root, "requests", job.JobID+".json")
	raw, err := json.Marshal(job)
	if err != nil || client.WritePrivate(path, raw) != nil {
		t.Fatal("cannot write synthetic job", err)
	}
	return path
}

func raw(t *testing.T, value any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func importJob(t *testing.T, root string) Job {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "sources", "selected.txt"), []byte("synthetic selected material"), 0600); err != nil {
		t.Fatal(err)
	}
	in := client.ImportInput{Schema: client.ImportInputSchema, Assets: []client.ImportAsset{{Key: "selected", Directory: ".", Classification: "final", Push: client.PushInput{ProjectID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Slug: "selected", Content: client.ContentInput{AssetType: "doc", Files: []client.InputFile{{Path: "selected.txt", Role: "primary"}}}}}}}
	return Job{Schema: JobSchema, JobID: "synthetic-import-plan", Action: "import.plan", Input: raw(t, in), Directory: "sources", Output: "reports/import-plan.json"}
}

func executeJob(t *testing.T, root string, job Job) (Result, error) {
	t.Helper()
	path := writeJob(t, root, job)
	inspection, err := InspectFile(path, root)
	if err != nil {
		t.Fatal(err)
	}
	return RunFile(t.Context(), path, Options{Workspace: root, ApprovalDigest: inspection.ApprovalDigest})
}

func TestAssistantInspectIsReadOnlyAndApprovalBindsExactJobAndWorkspace(t *testing.T) {
	root := workspace(t)
	job := importJob(t, root)
	path := writeJob(t, root, job)
	inspection, err := InspectFile(path, root)
	if err != nil || !digestPattern.MatchString(inspection.ApprovalDigest) || inspection.RemoteWrites || inspection.NetworkRequired {
		t.Fatalf("inspection=%+v err=%v", inspection, err)
	}
	if _, err = os.Stat(filepath.Join(root, "reports")); !os.IsNotExist(err) {
		t.Fatal("inspect created output directories")
	}
	if _, err = RunFile(t.Context(), path, Options{Workspace: root}); err == nil {
		t.Fatal("unapproved job ran")
	}
	job.Output = "reports/different.json"
	writeJob(t, root, job)
	if _, err = RunFile(t.Context(), path, Options{Workspace: root, ApprovalDigest: inspection.ApprovalDigest}); err == nil {
		t.Fatal("changed job retained old approval")
	}
	if _, err = os.Stat(filepath.Join(root, "reports")); !os.IsNotExist(err) {
		t.Fatal("rejected job created output directories")
	}
}

func TestAssistantImportPlanUsesOriginalPlannerAndPreservesSource(t *testing.T) {
	root := workspace(t)
	job := importJob(t, root)
	before, _ := os.ReadFile(filepath.Join(root, "sources/selected.txt"))
	result, err := executeJob(t, root, job)
	if err != nil || result.Status != "completed" || result.ExitCode != 0 || result.SourceDeleted || result.AutomaticRetry || result.CredentialIssued {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	var plan client.ImportPlan
	if json.Unmarshal(result.Result, &plan) != nil || plan.SourceRoot != filepath.Join(root, "sources") || plan.SelectedFiles != 1 || plan.Input.Assets[0].Push.Content.Files[0].SHA256 != hash(before) {
		t.Fatalf("unexpected original plan: %s", result.Result)
	}
	after, _ := os.ReadFile(filepath.Join(root, "sources/selected.txt"))
	if string(after) != string(before) {
		t.Fatal("source was modified")
	}
	var saved Result
	if client.LoadState(filepath.Join(root, job.Output), &saved) != nil || saved.ApprovalDigest != result.ApprovalDigest || saved.JobDigest != result.JobDigest || !digestPattern.MatchString(result.JobDigest) {
		t.Fatal("result was not privately saved and bound")
	}
	second, err := executeJob(t, root, job)
	if err != nil || second.ApprovalDigest != result.ApprovalDigest || string(second.Result) != string(result.Result) {
		t.Fatalf("same explicit job could not replay: %v", err)
	}
}

func TestAssistantJobDigestBindsOriginalCanonicalJSONAndApprovalBindsExactBytes(t *testing.T) {
	root := workspace(t)
	path := writeJob(t, root, importJob(t, root))
	first, err := InspectFile(path, root)
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := canonjson.Canonicalize(original)
	if err != nil || first.JobDigest != hash(canonical) || first.JobDigest == first.ApprovalDigest {
		t.Fatalf("job digest is not the original JCS JSON binding: %+v %v", first, err)
	}
	var indented bytes.Buffer
	if err := json.Indent(&indented, original, "", "  "); err != nil {
		t.Fatal(err)
	}
	if err := client.WritePrivate(path, indented.Bytes()); err != nil {
		t.Fatal(err)
	}
	second, err := InspectFile(path, root)
	if err != nil || first.JobDigest != second.JobDigest || first.ApprovalDigest == second.ApprovalDigest {
		t.Fatalf("canonical and exact-byte bindings were confused: first=%+v second=%+v err=%v", first, second, err)
	}
	if _, err := RunFile(t.Context(), path, Options{Workspace: root, ApprovalDigest: first.ApprovalDigest}); err == nil {
		t.Fatal("old exact-byte approval authorized reformatted job")
	}
	result, err := RunFile(t.Context(), path, Options{Workspace: root, ApprovalDigest: second.ApprovalDigest})
	if err != nil || result.JobDigest != second.JobDigest || result.ApprovalDigest != second.ApprovalDigest {
		t.Fatalf("result did not preserve both bindings: %+v %v", result, err)
	}
}

func TestAssistantRejectsUnknownFieldsCommandsPathsAndAmbiguousJSON(t *testing.T) {
	root := workspace(t)
	base := importJob(t, root)
	for _, action := range []string{"shell", "serve", "identity.execute", "import.move", "rm"} {
		job := base
		job.Action = action
		if _, err := InspectFile(writeJob(t, root, job), root); err == nil {
			t.Errorf("accepted arbitrary action %s", action)
		}
	}
	for _, path := range []string{"../escape", "/tmp/outside", "sources/../outside", "sources\\outside", ".", "C:/outside", "AUX", "sources/secret.lantai-part"} {
		job := base
		job.Output = path
		if _, err := InspectFile(writeJob(t, root, job), root); err == nil {
			t.Errorf("accepted unsafe output %q", path)
		}
	}
	for _, suffix := range []string{`,"argv":["sh","-c","touch bad"]}`, `,"action":"import.apply"}`, `} {}`} {
		rawJob := raw(t, base)
		path := filepath.Join(root, "requests/bad.json")
		if err := client.WritePrivate(path, append(rawJob[:len(rawJob)-1], []byte(suffix)...)); err != nil {
			t.Fatal(err)
		}
		if _, err := InspectFile(path, root); err == nil {
			t.Errorf("accepted malformed/unknown/duplicate fields: %s", suffix)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "bad")); !os.IsNotExist(err) {
		t.Fatal("job content was executed")
	}
}

func TestAssistantRejectsOutputOverlapAliasesAndUnknownExistingFile(t *testing.T) {
	root := workspace(t)
	base := importJob(t, root)
	for _, output := range []string{"sources/plan.json", "SOURCES/plan.json", "requests/synthetic-import-plan.json"} {
		job := base
		job.Output = output
		if _, err := InspectFile(writeJob(t, root, job), root); err == nil {
			t.Errorf("accepted output overlap %s", output)
		}
	}
	if err := client.WritePrivate(filepath.Join(root, "unrelated.json"), []byte(`{"private":"keep"}`)); err != nil {
		t.Fatal(err)
	}
	base.Output = "unrelated.json"
	if _, err := InspectFile(writeJob(t, root, base), root); err == nil {
		t.Fatal("accepted replacement of unrelated evidence")
	}
	got, _ := os.ReadFile(filepath.Join(root, "unrelated.json"))
	if string(got) != `{"private":"keep"}` {
		t.Fatal("rejected existing output changed")
	}
}

func TestAssistantPrivateFilesAndSymlinksAreRejectedWithoutRepair(t *testing.T) {
	root := workspace(t)
	job := importJob(t, root)
	path := writeJob(t, root, job)
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectFile(path, root); err == nil && runtime.GOOS != "windows" {
		t.Fatal("accepted broad job file")
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0644 && runtime.GOOS != "windows" {
		t.Fatal("assistant repaired permissions")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "requests/linked.json")
	if err := os.Symlink(path, link); err != nil {
		t.Skipf("symlink fixture unavailable: %v", err)
	}
	if _, err := InspectFile(link, root); err == nil {
		t.Fatal("accepted linked job")
	}
	outside := filepath.Join(root, "sources-link")
	if err := os.Symlink(filepath.Join(root, "sources"), outside); err != nil {
		t.Fatal(err)
	}
	job.Directory = "sources-link"
	if _, err := InspectFile(writeJob(t, root, job), root); err == nil {
		t.Fatal("accepted linked source")
	}
}

func TestAssistantRejectsCredentialFieldsAndShortLivedURLsBeforeExecution(t *testing.T) {
	root := workspace(t)
	base := importJob(t, root)
	for _, field := range []string{"token", "password", "csrf_token", "part_url_template", "download_url"} {
		job := base
		var input map[string]any
		if json.Unmarshal(job.Input, &input) != nil {
			t.Fatal("input")
		}
		input[field] = "synthetic-do-not-export"
		job.Input = raw(t, input)
		if _, err := InspectFile(writeJob(t, root, job), root); err == nil {
			t.Errorf("accepted credential field %s", field)
		}
	}
	for _, value := range []string{"https://gateway.example/xfer/v1/reads/ephemeral", "https://storage.example/file?sig=synthetic", "Bearer synthetic-private-token"} {
		job := base
		var in client.ImportInput
		if json.Unmarshal(job.Input, &in) != nil {
			t.Fatal("input")
		}
		in.Assets[0].Push.Content.Metadata = raw(t, map[string]string{"source_url": value})
		job.Input = raw(t, in)
		if _, err := InspectFile(writeJob(t, root, job), root); err == nil {
			t.Errorf("accepted short-lived credential URL %s", value)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "reports")); !os.IsNotExist(err) {
		t.Fatal("credential rejection created output")
	}
}

func TestAssistantInputFileBindingAndOutputCannotOverwritePriorInput(t *testing.T) {
	root := workspace(t)
	job := importJob(t, root)
	if err := client.WritePrivate(filepath.Join(root, "requests/input.json"), job.Input); err != nil {
		t.Fatal(err)
	}
	job.Input, job.InputFile = nil, "requests/input.json"
	path := writeJob(t, root, job)
	inspection, err := InspectFile(path, root)
	if err != nil {
		t.Fatal(err)
	}
	var input client.ImportInput
	if client.LoadState(filepath.Join(root, "requests/input.json"), &input) != nil {
		t.Fatal("missing input")
	}
	input.Assets[0].Classification = "unknown"
	if err := client.SaveState(filepath.Join(root, "requests/input.json"), input); err != nil {
		t.Fatal(err)
	}
	if _, err := RunFile(t.Context(), path, Options{Workspace: root, ApprovalDigest: inspection.ApprovalDigest}); err == nil {
		t.Fatal("changed input_file retained approval")
	}
	job.Output = job.InputFile
	if _, err := InspectFile(writeJob(t, root, job), root); err == nil {
		t.Fatal("output could overwrite input")
	}
}

func TestAssistantReferencePlanCopyAndRollbackKeepSourcesAndNewRefs(t *testing.T) {
	root := workspace(t)
	before := "# Scene\n[image](image.png)\n"
	if err := os.WriteFile(filepath.Join(root, "sources/note.md"), []byte(before), 0600); err != nil {
		t.Fatal(err)
	}
	fixed := "lantai://01ARZ3NDEKTSV4RRFFQ69G5FAV/assets/01ARZ3NDEKTSV4RRFFQ69G5FAW/versions/01ARZ3NDEKTSV4RRFFQ69G5FAX"
	in := client.ReferenceInput{Schema: "lantai.reference-input/v1", Notes: []string{"note.md"}, Mappings: []client.ReferenceMapping{{OldTarget: "image.png", FixedRef: fixed}}}
	job := Job{Schema: JobSchema, JobID: "reference-plan", Action: "references.plan", Input: raw(t, in), Directory: "sources", Output: "reports/reference-plan.json"}
	planned, err := executeJob(t, root, job)
	if err != nil || planned.Status != "completed" {
		t.Fatalf("plan: %s %v", planned.Result, err)
	}
	var plan client.ReferencePlan
	if json.Unmarshal(planned.Result, &plan) != nil || !plan.Complete {
		t.Fatal("reference plan unavailable")
	}
	job.JobID, job.Action, job.Input, job.Destination, job.Output = "reference-copy", "references.copy", planned.Result, "note-copy", "reports/reference-copy.json"
	copied, err := executeJob(t, root, job)
	if err != nil || copied.Status != "completed" {
		t.Fatalf("copy: %s %v", copied.Result, err)
	}
	copyBytes, _ := os.ReadFile(filepath.Join(root, "note-copy/note.md"))
	if !strings.Contains(string(copyBytes), fixed) {
		t.Fatal("explicit mapping was not copied")
	}
	job.JobID, job.Action, job.Directory, job.Destination, job.Output, job.PostCutoverRefs = "reference-rollback", "references.rollback", "note-copy", "rollback-copy", "reports/reference-rollback.json", []string{fixed, fixed}
	rolled, err := executeJob(t, root, job)
	if err != nil || rolled.Status != "completed" {
		t.Fatalf("rollback: %s %v", rolled.Result, err)
	}
	var receipt client.ReferenceReceipt
	if json.Unmarshal(rolled.Result, &receipt) != nil || len(receipt.PostCutoverRefs) != 2 {
		t.Fatal("rollback lost caller-declared later references")
	}
	for _, path := range []string{"sources/note.md", "rollback-copy/note.md"} {
		got, _ := os.ReadFile(filepath.Join(root, path))
		if string(got) != before {
			t.Errorf("original bytes not retained at %s", path)
		}
	}
	afterCopy, _ := os.ReadFile(filepath.Join(root, "note-copy/note.md"))
	if string(afterCopy) != string(copyBytes) {
		t.Fatal("rollback changed managed copy")
	}
	job.JobID, job.Output, job.Destination, job.PostCutoverRefs = "rollback-empty-inventory", "reports/rollback-empty.json", "rollback-empty", []string{}
	emptyResult, err := executeJob(t, root, job)
	if err != nil || emptyResult.Status != "completed" {
		t.Fatalf("explicit empty inventory was not preserved: %+v %v", emptyResult, err)
	}
}

func TestAssistantCancellationNeverClaimsCompletionOrRollback(t *testing.T) {
	root := workspace(t)
	job := importJob(t, root)
	path := writeJob(t, root, job)
	inspection, err := InspectFile(path, root)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	result, err := RunFile(ctx, path, Options{Workspace: root, ApprovalDigest: inspection.ApprovalDigest})
	if !errors.Is(err, context.Canceled) || result.Status != "cancelled" || result.ExitCode != 130 || result.SourceDeleted || result.AutomaticRetry {
		t.Fatalf("cancelled result=%+v err=%v", result, err)
	}
	if _, err := os.Stat(filepath.Join(root, "sources/selected.txt")); err != nil {
		t.Fatal("cancel removed source")
	}
}

func TestAssistantSyntheticClaimsAreBoundExpiredSingleUseAndNeverCredentials(t *testing.T) {
	now := time.Now()
	s := NewSyntheticClaims(func() time.Time { return now })
	digest := strings.Repeat("a", 64)
	if _, err := s.Begin("https://real-device.example", "fixture", digest); err == nil {
		t.Fatal("synthetic fixture accepted non-loopback target")
	}
	challenge, err := s.Begin("https://localhost:4180", "fixture", digest)
	if err != nil || !challenge.SyntheticOnly {
		t.Fatal(err)
	}
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			r, err := s.Confirm(challenge.ChallengeID, challenge.ConfirmationCode, "fixture", digest)
			if err == nil {
				accepted.Add(1)
				if r.AuthorizesExecution || r.CredentialIssued || !r.SyntheticOnly {
					t.Error("fixture issued execution authority")
				}
			}
		})
	}
	wg.Wait()
	if accepted.Load() != 1 {
		t.Fatalf("concurrent confirmations=%d", accepted.Load())
	}
	challenge, _ = s.Begin("https://127.0.0.1:4180", "fixture", digest)
	now = now.Add(3 * time.Minute)
	if _, err := s.Confirm(challenge.ChallengeID, challenge.ConfirmationCode, "fixture", digest); err == nil {
		t.Fatal("expired challenge accepted")
	}
	challenge, _ = s.Begin("https://localhost:4180", "fixture", digest)
	if _, err := s.Confirm(challenge.ChallengeID, challenge.ConfirmationCode, "other", digest); err == nil {
		t.Fatal("challenge changed device")
	}
	if _, err := s.Confirm(challenge.ChallengeID, challenge.ConfirmationCode, "fixture", digest); err == nil {
		t.Fatal("first mismatched attempt did not consume challenge")
	}
}

func TestAssistantActualContainmentRejectsDistinctCaseSiblingWorkspace(t *testing.T) {
	parent := workspace(t)
	selected := filepath.Join(parent, "selected")
	outside := filepath.Join(parent, "SELECTED")
	for _, root := range []string{selected, outside} {
		for _, dir := range []string{"sources", "requests", "credentials"} {
			if e := os.MkdirAll(filepath.Join(root, dir), 0700); e != nil {
				t.Fatal(e)
			}
		}
	}
	a, _ := os.Stat(selected)
	b, _ := os.Stat(outside)
	if os.SameFile(a, b) {
		t.Skip("requires case-sensitive filesystem")
	}
	job := importJob(t, selected)
	path := writeJob(t, outside, job)
	if _, e := InspectFile(path, selected); e == nil {
		t.Fatal("absolute operator input in distinct case sibling escaped workspace")
	}
	if inside(selected, path) || within(filepath.Join(selected, "sources"), filepath.Join(selected, "SOURCES/file")) {
		t.Fatal("actual containment reused case-folded conflict semantics")
	}
	if !overlap(filepath.Join(selected, "sources"), filepath.Join(selected, "SOURCES/file")) {
		t.Fatal("case folding no longer conservatively rejects alias conflicts")
	}
}
