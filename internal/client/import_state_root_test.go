package client

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Only synthetic temporary files and the existing upload protocol peer are
// used. A request callback moves the original state root and substitutes a
// symlink to the selected source before the server responds. Windows may reject
// the move while a private lock is open; that branch verifies safe completion
// and release without weakening the successful-substitution assertions.
func TestImportApplyPinnedStateParentCannotWriteSourceAndResumesOriginalKeys(t *testing.T) {
	for _, phase := range []string{"create", "commit"} {
		t.Run(phase, func(t *testing.T) {
			root, in := importFixture(t)
			root, err := importAbsolute(root)
			if err != nil {
				t.Fatal(err)
			}
			probe := filepath.Join(t.TempDir(), "symlink-probe")
			if err = os.Symlink(root, probe); err != nil {
				t.Skip("symlink unavailable", err)
			}
			if err = os.Remove(probe); err != nil {
				t.Fatal(err)
			}
			sentinels := map[string]string{
				"import.json":     "synthetic source import sentinel\n",
				"scene.push.json": "synthetic source push sentinel\n",
			}
			beforeInfo := map[string]os.FileInfo{}
			for name, body := range sentinels {
				importWrite(t, root, name, body)
				beforeInfo[name], err = os.Stat(filepath.Join(root, name))
				if err != nil {
					t.Fatal(err)
				}
			}
			plan := importMustPlan(t, root, in)
			stateDir := importStateDir(t)
			movedDir := stateDir + "-original"
			c, peer := newImportPeer(t)
			type observed struct {
				state   PushState
				raw     []byte
				err     error
				blocked bool
			}
			observations := make(chan observed, 1)
			swap := func() {
				var got observed
				path := filepath.Join(stateDir, "scene.push.json")
				if got.err = LoadState(path, &got.state); got.err == nil {
					got.raw, got.err = os.ReadFile(path)
				}
				if got.err == nil {
					got.err = os.Rename(stateDir, movedDir)
					if runtime.GOOS == "windows" && os.IsPermission(got.err) {
						got.blocked, got.err = true, nil
					}
				}
				if got.err == nil && !got.blocked {
					got.err = os.Symlink(root, stateDir)
				}
				observations <- got
			}
			if phase == "create" {
				peer.onCreate = swap
			} else {
				peer.onCommit = swap
			}
			report, err := c.ApplyImport(context.Background(), plan, root, stateDir)
			var first observed
			select {
			case first = <-observations:
			default:
				t.Fatal("synthetic request never reached its state-substitution point")
			}
			if first.err != nil {
				t.Fatal(first.err)
			}
			if first.blocked {
				if err != nil || !report.Complete || len(report.Results) != 1 || report.Results[0].Status != "committed" {
					t.Fatalf("OS-blocked state replacement interrupted safe import: %+v %v", report, err)
				}
			} else if err == nil || report.Complete || len(report.Results) != 1 || report.Results[0].Status != "unknown" {
				t.Fatalf("state substitution did not return a partial error: %+v %v", report, err)
			}
			if first.state.CreateKey == "" || first.state.CommitKey == "" || first.state.RequestHash == "" {
				t.Fatal("original keys were not durable before the request")
			}
			if phase == "commit" && (!first.state.Committing || first.state.OperationID == "") {
				t.Fatal("commit was attempted without durable original operation")
			}
			for name, body := range sentinels {
				path := filepath.Join(root, name)
				got, readErr := os.ReadFile(path)
				info, statErr := os.Stat(path)
				if readErr != nil || statErr != nil || string(got) != body || !os.SameFile(beforeInfo[name], info) || !beforeInfo[name].ModTime().Equal(info.ModTime()) {
					t.Fatalf("source sentinel %s was modified: read=%v stat=%v", name, readErr, statErr)
				}
				if _, statErr = os.Lstat(path + ".lock"); !os.IsNotExist(statErr) {
					t.Fatalf("source received a state lock: %s %v", name, statErr)
				}
			}
			if err = ValidateImportPlan(context.Background(), plan, root); err != nil {
				t.Fatalf("source directory changed: %v", err)
			}
			if first.blocked {
				// The state directory must become movable once all private lock
				// handles have been released. No source substitution took place.
				if err = os.Rename(stateDir, movedDir); err != nil {
					t.Fatalf("state remained locked after import returned: %v", err)
				}
				if err = os.Rename(movedDir, stateDir); err != nil {
					t.Fatal(err)
				}
			} else {
				raw, err := os.ReadFile(filepath.Join(movedDir, "scene.push.json"))
				if err != nil || !bytes.Equal(raw, first.raw) {
					t.Fatalf("detached original state was changed after substitution: %v", err)
				}
				var savedReport ImportReport
				if err = LoadState(filepath.Join(movedDir, "import.json"), &savedReport); err != nil || savedReport.Complete || len(savedReport.Results) != 1 || savedReport.Results[0].Status != "unknown" {
					t.Fatalf("durable batch lost original partial state: %+v %v", savedReport, err)
				}
				// Restore only the synthetic symlink and its original state directory.
				if st, statErr := os.Lstat(stateDir); statErr != nil || st.Mode()&os.ModeSymlink == 0 {
					t.Fatalf("expected synthetic substituted symlink: %v", statErr)
				}
				if err = os.Remove(stateDir); err != nil {
					t.Fatal(err)
				}
				if err = os.Rename(movedDir, stateDir); err != nil {
					t.Fatal(err)
				}
			}
			peer.mu.Lock()
			peer.onCreate, peer.onCommit = nil, nil
			peer.mu.Unlock()
			report, err = c.ApplyImport(context.Background(), plan, root, stateDir)
			if err != nil || !report.Complete || len(report.Results) != 1 || report.Results[0].Status != "committed" {
				t.Fatalf("restored original state did not resume: %+v %v", report, err)
			}
			var after PushState
			if err = LoadState(filepath.Join(stateDir, "scene.push.json"), &after); err != nil {
				t.Fatal(err)
			}
			if first.state.CreateKey != after.CreateKey || first.state.CommitKey != after.CommitKey || first.state.RequestHash != after.RequestHash || (first.state.OperationID != "" && first.state.OperationID != after.OperationID) {
				t.Fatal("recovery chose replacement keys or an operation")
			}
			peer.mu.Lock()
			counts := [4]int{peer.createCount, peer.commitCount, len(peer.createKeys), len(peer.receipts)}
			calls := peer.commitCalls
			peer.mu.Unlock()
			expectedCalls := 1
			if phase == "commit" && !first.blocked {
				expectedCalls = 2
			}
			if counts != [4]int{1, 1, 1, 1} || calls != expectedCalls {
				t.Fatalf("recovery duplicated a server operation: %v calls=%d", counts, calls)
			}
			if _, err = c.ApplyImport(context.Background(), plan, root, stateDir); err != nil {
				t.Fatal(err)
			}
			peer.mu.Lock()
			defer peer.mu.Unlock()
			if peer.commitCalls != calls || peer.createCount != 1 || peer.commitCount != 1 {
				t.Fatal("completed batch sent another mutation")
			}
		})
	}
}
