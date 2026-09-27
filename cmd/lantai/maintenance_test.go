package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/operations"
)

type brokenOutput struct{}

func (brokenOutput) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestLocalBackupRestoreCommandLifecycle(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte("contract: lantai.config/v1\nstorage:\n  min_free_bytes: 1048576\n"), 0600); err != nil {
		t.Fatal(err)
	}
	code, out, stderr := interact(t, []string{"init", "-home", home, "-admin", "ada"}, []string{"initial password secret", "initial password secret"}, true)
	if code != exitOK {
		t.Fatalf("init: %d %s", code, stderr)
	}
	match := regexp.MustCompile(`administrator\s+ada \(([0-9A-Z]{26})\)`).FindStringSubmatch(out)
	if len(match) != 2 {
		t.Fatal("missing administrator ID")
	}
	archive := filepath.Join(t.TempDir(), "archive")
	if code, _, stderr = runCLI(t, "backup", "-home", home, "-destination", archive); code != exitOK {
		t.Fatalf("backup: %d %s", code, stderr)
	}
	for _, args := range [][]string{{"backup-verify", "-backup", archive}, {"backup", "-home", home, "-status"}, {"fsck", "-home", home}, {"recover", "-home", home}, {"reindex", "-home", home}} {
		if code, _, stderr = runCLI(t, args...); code != exitOK {
			t.Fatalf("%s: %d %s", args[0], code, stderr)
		}
	}
	target := filepath.Join(t.TempDir(), "restored")
	if code, _, stderr = runCLI(t, "restore", "-home", target, "-backup", archive, "-key-dir", filepath.Join(home, "secrets")); code != exitOK {
		t.Fatalf("restore: %d %s", code, stderr)
	}
	if code, out, _ = runCLI(t, "doctor", "-home", target); code != exitInvalid || !strings.Contains(out, "restore_incomplete") {
		t.Fatalf("premature readiness: %d %s", code, out)
	}
	marker, err := operations.ReadMarker(operations.Layout{Home: target})
	if err != nil {
		t.Fatal(err)
	}
	proof := []byte("Synthetic empty-instance rehearsal: all old credentials revoked, no newer deletions or unresolved domain operations.")
	evidence := filepath.Join(t.TempDir(), "review.txt")
	if err = os.WriteFile(evidence, proof, 0600); err != nil {
		t.Fatal(err)
	}
	r := marker.Restore
	review := operations.RecoveryReview{Contract: "lantai.recovery-review/v1", RunID: r.RunID, BackupID: r.BackupID, ManifestDigest: r.ManifestDigest, RecoveryEpoch: marker.RecoveryEpoch, Administrator: match[1], RevocationsReconciled: true, DeletionsReconciled: true, OpenOperationsReconciled: true, EvidenceDigest: digest.Of(proof), Note: "synthetic command lifecycle validation"}
	file := filepath.Join(t.TempDir(), "review.json")
	raw, err := json.Marshal(review)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	complete := []string{"restore-complete", "-home", target, "-review", file, "-evidence", evidence}
	if code, _, _ = runCLI(t, complete...); code == exitOK {
		t.Fatal("completed without new admin setup")
	}
	code, _, stderr = interact(t, []string{"recover-admin", "-home", target, "-admin", "ada"}, []string{"RESET ada", "restored password secret", "restored password secret"}, true)
	if code != exitOK {
		t.Fatalf("recover-admin: %d %s", code, stderr)
	}
	for range 2 {
		if code, _, stderr = runCLI(t, complete...); code != exitOK {
			t.Fatalf("complete/replay: %d %s", code, stderr)
		}
	}
	if code, out, _ = runCLI(t, "doctor", "-home", target); code != exitOK {
		t.Fatalf("completed doctor: %d %s", code, out)
	}
	stored, err := os.ReadFile(filepath.Join(target, "logs", "restore-"+string(r.RunID)+"-evidence.txt"))
	if err != nil || !bytes.Equal(stored, proof) {
		t.Fatalf("bound evidence not preserved: %v", err)
	}
	// A completed backup remains resumable even if the output pipe fails; the
	// command must report an output failure instead of telling scripts success.
	failedOutput := filepath.Join(t.TempDir(), "output-failed")
	var errors bytes.Buffer
	code = run(t.Context(), []string{"backup", "-home", target, "-destination", failedOutput}, brokenOutput{}, &errors)
	if code != exitInternal || !strings.Contains(errors.String(), "state is durable") {
		t.Fatalf("false stdout success: %d %s", code, errors.String())
	}
	if _, err = operations.VerifyBackup(t.Context(), failedOutput); err != nil {
		t.Fatal("output failure undid durable backup", err)
	}
}
