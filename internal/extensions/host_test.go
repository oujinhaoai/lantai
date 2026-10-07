package extensions_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/execution"
	"github.com/oujinhaoai/lantai/internal/extensions"
	"github.com/oujinhaoai/lantai/internal/extensions/exttest"
)

type hostCase struct {
	pkg extensions.Package
	dir string
}

func newHostCase(t *testing.T, s exttest.Spec) hostCase {
	t.Helper()
	dir := t.TempDir()
	return hostCase{pkg: exttest.Write(t, dir, s), dir: dir}
}

func (h hostCase) job(t *testing.T, config map[string]any) []byte {
	t.Helper()
	producer := map[string]any{"extension_id": h.pkg.Manifest.ID, "extension_version": h.pkg.Manifest.Version, "package_digest": h.pkg.Digest, "source": "package", "contribution_id": h.pkg.Manifest.ID + ".check"}
	ref := map[string]any{"instance_id": "01K00000000000000000000000", "asset_id": "01K00000000000000000000001", "version_id": "01K00000000000000000000002"}
	b, err := json.Marshal(map[string]any{"contract": "lantai.processor-input/v1", "operation_id": "01K00000000000000000000009", "job_fence": map[string]any{"job_id": "01K00000000000000000000020", "job_attempt_id": "01K00000000000000000000019", "lease_fence": 1, "recovery_epoch": 1}, "input_refs": []any{ref}, "input_digest": "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", "deadline": "2026-09-28T12:00:00.000Z", "producer": producer, "config": config})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (h hostCase) spec(t *testing.T, config map[string]any, timeout time.Duration) extensions.RunSpec {
	t.Helper()
	manifest := []byte(`{"instance_id":"01K00000000000000000000000","asset_id":"01K00000000000000000000001","version_id":"01K00000000000000000000002","manifest_digest":"sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"}`)
	return extensions.RunSpec{Package: h.pkg, Files: os.DirFS(h.dir), Target: "server", Job: h.job(t, config), Inputs: []extensions.InputFile{{Name: "manifest.yaml", Data: manifest}}, Deadline: time.Now().Add(timeout), MaxOutputFiles: h.pkg.Manifest.ResourceLimits.MaxOutputFiles}
}
func (h hostCase) run(t *testing.T, ctx context.Context, config map[string]any, timeout time.Duration) extensions.RunResult {
	t.Helper()
	out, err := extensions.RunOneShot(ctx, h.spec(t, config, timeout))
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestOneShotFileProtocolAndFaultClasses(t *testing.T) {
	h := newHostCase(t, exttest.Spec{Server: true})
	ctx := t.Context()
	ok := h.run(t, ctx, map[string]any{"mode": "env"}, 20*time.Second)
	if execution.ClassifyInvocation(ok.Observation) != execution.InvocationCompleted || len(ok.Result) == 0 {
		t.Fatalf("legal run: %+v stderr=%s", ok, ok.Stderr)
	}
	var res struct {
		Checks []struct{ Verdict string } `json:"checks"`
	}
	if err := json.Unmarshal(ok.Result, &res); err != nil || len(res.Checks) != 1 || res.Checks[0].Verdict != "pass" {
		t.Fatal(string(ok.Result), err)
	}
	fail := h.run(t, ctx, map[string]any{"mode": "fail"}, 20*time.Second)
	if execution.ClassifyInvocation(fail.Observation) != execution.InvocationCompleted {
		t.Fatal("legal fail must be a completed invocation", fail)
	}
	for mode, reason := range map[string]string{"crash": "", "exit1": "", "no_result": "result_missing", "bad_schema": "", "extra_file": "output_limit_exceeded", "big_output": "output_limit_exceeded", "symlink": "output_symlink"} {
		out := h.run(t, ctx, map[string]any{"mode": mode}, 20*time.Second)
		got := execution.ClassifyInvocation(out.Observation)
		if mode == "bad_schema" {
			// The host only verifies file-level protocol; schema is the caller's gate.
			if got != execution.InvocationCompleted || len(out.Result) == 0 {
				t.Fatal(mode, out)
			}
			continue
		}
		if mode == "symlink" && runtime.GOOS == "windows" && got == execution.InvocationRuntimeFault {
			continue // unprivileged Windows accounts may not create the symlink at all
		}
		if got != execution.InvocationRuntimeFault || !out.Observation.StopConfirmed || out.Failure != reason {
			t.Fatalf("%s: %v %+v", mode, got, out)
		}
	}
}

func TestOneShotTimeoutAndCancelReclaimProcessTree(t *testing.T) {
	h := newHostCase(t, exttest.Spec{Server: true})
	run := func(cancelAfterStart bool) (extensions.RunResult, string) {
		t.Helper()
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		beat := filepath.Join(t.TempDir(), "heartbeat")
		type response struct {
			out extensions.RunResult
			err error
		}
		result := make(chan response, 1)
		spec := h.spec(t, map[string]any{"mode": "hang_child", "heartbeat": beat}, 20*time.Second)
		go func() { out, err := extensions.RunOneShot(ctx, spec); result <- response{out, err} }()
		// A cold Windows executable can take longer than the former 1.5s timeout
		// to start its grandchild. Prove the descendant exists before testing its
		// reclamation; this is a bounded fixture readiness wait, not a product SLA.
		startup := time.NewTimer(10 * time.Second)
		defer startup.Stop()
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			if _, err := os.Stat(beat); err == nil {
				if cancelAfterStart {
					cancel()
				}
				r := <-result
				if r.err != nil {
					t.Fatal(r.err)
				}
				return r.out, beat
			}
			select {
			case out := <-result:
				t.Fatalf("host finished before descendant readiness: %+v", out.out.Observation)
			case <-startup.C:
				cancel()
				out := <-result
				t.Fatalf("descendant did not become ready; reclaimed fixture: %+v", out.out.Observation)
			case <-ticker.C:
			}
		}
	}
	out, beat := run(false)
	if !out.Observation.TimedOut || !out.Observation.StopConfirmed || execution.ClassifyInvocation(out.Observation) != execution.InvocationRuntimeFault {
		t.Fatal(out.Observation)
	}
	assertStopped(t, beat)
	out, beat = run(true)
	if !out.Observation.Cancelled || !out.Observation.StopConfirmed || execution.ClassifyInvocation(out.Observation) != execution.InvocationCancelled {
		t.Fatal(out.Observation)
	}
	assertStopped(t, beat)
}

// assertStopped proves the grandchild stopped writing after the host returned.
func assertStopped(t *testing.T, beat string) {
	t.Helper()
	before, err := os.Stat(beat)
	if err != nil {
		t.Fatal("grandchild never started", err)
	}
	time.Sleep(400 * time.Millisecond)
	after, err := os.Stat(beat)
	if err != nil || after.Size() != before.Size() {
		t.Fatal("descendant survived host reclaim", before.Size(), after.Size(), err)
	}
}

func TestOneShotRejectsReplacedEntryBeforeDispatch(t *testing.T) {
	h := newHostCase(t, exttest.Spec{Server: true})
	entry := filepath.Join(h.dir, filepath.FromSlash(exttest.EntryPath()))
	if err := os.Chmod(entry, 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(entry, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.Write([]byte{0})
	f.Close()
	out, err := extensions.RunOneShot(t.Context(), extensions.RunSpec{Package: h.pkg, Files: os.DirFS(h.dir), Target: "server", Job: h.job(t, nil), Deadline: time.Now().Add(time.Minute)})
	if errcode.CodeOf(err) != errcode.HashMismatch || out.Observation.Dispatched {
		t.Fatal(out, err)
	}
	if _, err = extensions.RunOneShot(t.Context(), extensions.RunSpec{Package: h.pkg, Files: os.DirFS(h.dir), Target: "cli", Job: h.job(t, nil), Deadline: time.Now().Add(time.Minute)}); errcode.CodeOf(err) != errcode.ExtensionPointUnsupported {
		t.Fatal("undeclared target dispatched", err)
	}
}

func TestOneShotDeliversVerifiedCommandOutputs(t *testing.T) {
	h := newHostCase(t, exttest.Spec{CLI: true})
	src := filepath.Join(t.TempDir(), "note.txt")
	if err := os.WriteFile(src, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	producer := map[string]any{"extension_id": h.pkg.Manifest.ID, "extension_version": h.pkg.Manifest.Version, "package_digest": h.pkg.Digest, "source": "package", "contribution_id": h.pkg.Manifest.ID + ".copy"}
	job, _ := json.Marshal(map[string]any{"contract": "lantai.cli-command-input/v1", "operation_id": "01K00000000000000000000009", "command": h.pkg.Manifest.ID + ".copy", "args": []string{}, "inputs": []any{map[string]any{"name": "note.txt", "sha256": "0000000000000000000000000000000000000000000000000000000000000000", "size": 5}}, "deadline": "2026-09-28T12:00:00.000Z", "producer": producer})
	dst := t.TempDir()
	out, err := extensions.RunOneShot(t.Context(), extensions.RunSpec{Package: h.pkg, Files: os.DirFS(h.dir), Target: "cli", Job: job, Inputs: []extensions.InputFile{{Name: "note.txt", Path: src}}, Deadline: time.Now().Add(20 * time.Second), MaxOutputFiles: 16, OutputDir: dst})
	if err != nil || execution.ClassifyInvocation(out.Observation) != execution.InvocationCompleted || len(out.Files) != 1 {
		t.Fatalf("%+v %v %s", out, err, out.Stderr)
	}
	b, err := os.ReadFile(filepath.Join(dst, "copies", "note.txt"))
	if err != nil || string(b) != "HELLO" {
		t.Fatal(string(b), err)
	}
	if b, _ = os.ReadFile(src); string(b) != "hello" {
		t.Fatal("input mutated")
	}
}
