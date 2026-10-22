package operations

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

func TestManagementInfoProjectsCompletedRecoveryWithoutPrivateNotes(t *testing.T) {
	i := create(t, t.TempDir(), Options{})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := i.Close(ctx); err != nil {
			t.Errorf("close instance: %v", err)
		}
	})
	i.marker.RecoveryEpoch = 4
	run := ids.New()
	r := RecoveryReview{Contract: "lantai.recovery-review/v2", RunID: run, BackupID: ids.New(), ManifestDigest: digest.Of([]byte("snapshot")), RecoveryEpoch: 3, Administrator: "private-admin", EvidenceDigest: digest.Of([]byte("evidence")), Note: "private path /home/private/secret", AcceptRecoveryPoint: true, UnknownAfterCapture: true}
	if err := os.MkdirAll(filepath.Join(i.layout.Home, "logs"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeBackupJSON(filepath.Join(i.layout.Home, "logs", "restore-"+string(run)+".json"), r); err != nil {
		t.Fatal(err)
	}
	info, err := i.ManagementInfo(t.Context())
	if err != nil || len(info.RecoveryReceipts) != 1 || info.RecoveryReceipts[0].RunID != run || !info.RecoveryReceipts[0].UnknownAfterCapture || info.RecoveryReceipts[0].AcceptanceBasis != "accepted_capture_point" {
		t.Fatalf("%+v %v", info, err)
	}
	raw, _ := json.Marshal(info)
	for _, private := range []string{i.layout.Home, "private-admin", "private path", "administrator", "secret"} {
		if strings.Contains(string(raw), private) {
			t.Fatalf("private detail leaked: %s", private)
		}
	}
	i.marker.Restore = &RestoreRun{RunID: run}
	info, err = i.ManagementInfo(t.Context())
	if err != nil || len(info.RecoveryReceipts) != 0 || info.RecoveryError != "recovery_receipt_invalid" {
		t.Fatalf("incomplete restore promoted: %+v %v", info, err)
	}
}

func TestManagementInfoRejectsForeignEpochMalformedAndLinkedReceipts(t *testing.T) {
	i := create(t, t.TempDir(), Options{})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := i.Close(ctx); err != nil {
			t.Errorf("close instance: %v", err)
		}
	})
	i.marker.RecoveryEpoch = 2
	logs := filepath.Join(i.layout.Home, "logs")
	if err := os.MkdirAll(logs, 0700); err != nil {
		t.Fatal(err)
	}
	future := RecoveryReview{Contract: "lantai.recovery-review/v2", BackupID: ids.New(), ManifestDigest: digest.Of([]byte("snapshot")), RecoveryEpoch: 3, Administrator: "synthetic", EvidenceDigest: digest.Of([]byte("proof")), Note: "synthetic future receipt", AcceptRecoveryPoint: true, UnknownAfterCapture: true}
	for _, tc := range []struct {
		name   string
		body   []byte
		linked bool
	}{
		{"future_epoch", nil, false},
		{"malformed", []byte(`{"contract":"lantai.recovery-review/v2","run_id":"wrong"}`), false},
		{"linked", []byte(`{"secret":"not returned"}`), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := ids.New()
			p := filepath.Join(logs, "restore-"+string(id)+".json")
			if tc.name == "future_epoch" {
				future.RunID = id
				tc.body, _ = json.Marshal(future)
			}
			if tc.linked {
				target := filepath.Join(t.TempDir(), "private.json")
				if err := os.WriteFile(target, tc.body, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, p); err != nil {
					t.Skip("symlink unavailable")
				}
			} else if err := os.WriteFile(p, tc.body, 0600); err != nil {
				t.Fatal(err)
			}
			info, err := i.ManagementInfo(t.Context())
			if err != nil || len(info.RecoveryReceipts) != 0 || info.RecoveryError == "" {
				t.Fatalf("unsafe receipt accepted: %+v %v", info, err)
			}
		})
	}
}
