package client

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestReferenceV1CopyV2InventoryAndRollbackPreserveAllSources(t *testing.T) {
	root, in, before := referenceFixture(t)
	p := mustReferencePlan(t, root, in)
	copyRoot := referenceOutput(t)
	r, err := CopyReferenceNotes(context.Background(), p, root, copyRoot)
	if err != nil || !r.Complete || r.Mode != "copy" {
		t.Fatalf("copy: %+v %v", r, err)
	}
	assertReferenceBytes(t, root, in.Notes[0], before)
	assertReferenceBytes(t, copyRoot, in.Notes[0], p.Notes[0].After)
	second, err := CopyReferenceNotes(context.Background(), p, root, copyRoot)
	if err != nil || !second.Complete || second.Results[0].State != "present" {
		t.Fatalf("repeat: %+v %v", second, err)
	}
	// These are explicit caller declarations of synthetic post-cutover receipts;
	// the offline tool does not claim that it queried or enumerated the server.
	v2, v3 := referenceURI(t), referenceURI(t)
	inventory := []string{v2, v3, v2}
	rollbackRoot := referenceOutput(t)
	r, err = RollbackReferenceNotes(context.Background(), p, copyRoot, rollbackRoot, inventory)
	if err != nil || !r.Complete || r.Mode != "rollback" || !reflect.DeepEqual(r.PostCutoverRefs, inventory) || r.InventoryScope != "caller_declared_only_not_whole_library" {
		t.Fatalf("rollback: %+v %v", r, err)
	}
	assertReferenceBytes(t, rollbackRoot, in.Notes[0], before)
	assertReferenceBytes(t, copyRoot, in.Notes[0], p.Notes[0].After)
	assertReferenceBytes(t, root, in.Notes[0], before)
	encoded, _ := json.Marshal(r)
	for _, ref := range inventory {
		if !strings.Contains(string(encoded), ref) {
			t.Fatal("lost new version inventory")
		}
	}
	again, err := RollbackReferenceNotes(context.Background(), p, copyRoot, rollbackRoot, inventory)
	if err != nil || !again.Complete || again.Results[0].State != "present" {
		t.Fatalf("repeat rollback: %+v %v", again, err)
	}
	if _, err = RollbackReferenceNotes(context.Background(), p, copyRoot, rollbackRoot, []string{}); err == nil {
		t.Fatal("blindly changed bound inventory")
	}
	if _, err = RollbackReferenceNotes(context.Background(), p, copyRoot, referenceOutput(t), nil); err == nil {
		t.Fatal("implicit inventory accepted")
	}
	if _, err = RollbackReferenceNotes(context.Background(), p, copyRoot, referenceOutput(t), []string{"https://example.test/latest"}); err == nil {
		t.Fatal("non-fixed inventory accepted")
	}
}
func assertReferenceBytes(t *testing.T, root, path, want string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
	if err != nil || string(b) != want {
		t.Fatalf("unexpected bytes %s: %v", path, err)
	}
}

func TestReferenceChangedSourceAndRehashedPlanRejectBeforeCopy(t *testing.T) {
	root, in, _ := referenceFixture(t)
	p := mustReferencePlan(t, root, in)
	target := referenceOutput(t)
	importWrite(t, root, in.Notes[0], "concurrent source edit")
	if _, err := CopyReferenceNotes(context.Background(), p, root, target); err == nil {
		t.Fatal("changed original copied")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("wrote before source preflight")
	}
	root, in, _ = referenceFixture(t)
	p = mustReferencePlan(t, root, in)
	copyRoot := referenceOutput(t)
	if _, err := CopyReferenceNotes(context.Background(), p, root, copyRoot); err != nil {
		t.Fatal(err)
	}
	p.Notes[0].After += "\nInjected"
	p.Notes[0].AfterSHA256 = importSHA([]byte(p.Notes[0].After))
	p.Digest, _ = referencePlanDigest(p)
	if _, err := RollbackReferenceNotes(context.Background(), p, copyRoot, referenceOutput(t), []string{}); err == nil {
		t.Fatal("self-rehashed plan accepted by rollback")
	}
}
func TestReferenceEditedCopyAndForeignTargetsArePreserved(t *testing.T) {
	root, in, _ := referenceFixture(t)
	p := mustReferencePlan(t, root, in)
	target := referenceOutput(t)
	if _, err := CopyReferenceNotes(context.Background(), p, root, target); err != nil {
		t.Fatal(err)
	}
	importWrite(t, target, in.Notes[0], "user continued editing the copy")
	if _, err := CopyReferenceNotes(context.Background(), p, root, target); err == nil {
		t.Fatal("edited copy accepted")
	}
	if _, err := RollbackReferenceNotes(context.Background(), p, target, referenceOutput(t), []string{}); err == nil {
		t.Fatal("rollback ignored user edits")
	}
	assertReferenceBytes(t, target, in.Notes[0], "user continued editing the copy")
	foreign := referenceOutput(t)
	if err := importPrivateDirectory(foreign); err != nil {
		t.Fatal(err)
	}
	importWrite(t, foreign, "keep.md", "foreign bytes")
	if _, err := CopyReferenceNotes(context.Background(), p, root, foreign); err == nil {
		t.Fatal("adopted foreign destination")
	}
	assertReferenceBytes(t, foreign, "keep.md", "foreign bytes")
	entries, err := os.ReadDir(foreign)
	if err != nil || len(entries) != 1 {
		t.Fatal("rejected foreign root was mutated", err)
	}
	for _, bad := range []string{root, filepath.Join(root, "copy"), filepath.Dir(root), strings.ToUpper(root)} {
		if _, err := CopyReferenceNotes(context.Background(), p, root, bad); err == nil {
			t.Fatal("source/target overlap accepted", bad)
		}
	}
}
func TestReferenceBoundPartialResumesWithoutReplacingModifiedBytes(t *testing.T) {
	root, in, _ := referenceFixture(t)
	p := mustReferencePlan(t, root, in)
	target := referenceOutput(t)
	if _, err := CopyReferenceNotes(context.Background(), p, root, target); err != nil {
		t.Fatal(err)
	}
	n := p.Notes[0]
	file := filepath.Join(target, filepath.FromSlash(n.Path))
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	importWrite(t, target, n.Path+referencePartSuffix, n.After[:15])
	r, err := CopyReferenceNotes(context.Background(), p, root, target)
	if err != nil || !r.Complete {
		t.Fatalf("partial resume: %+v %v", r, err)
	}
	assertReferenceBytes(t, target, n.Path, n.After)
	if err = os.Remove(file); err != nil {
		t.Fatal(err)
	}
	importWrite(t, target, n.Path+referencePartSuffix, "user modified partial")
	if _, err = CopyReferenceNotes(context.Background(), p, root, target); err == nil {
		t.Fatal("overwrote edited partial")
	}
	assertReferenceBytes(t, target, n.Path+referencePartSuffix, "user modified partial")
}

func TestReferencePublishedFinalAndPartCrashResidueCanResume(t *testing.T) {
	for _, hardlink := range []bool{false, true} {
		t.Run(fmt.Sprint(hardlink), func(t *testing.T) {
			root, in, _ := referenceFixture(t)
			p := mustReferencePlan(t, root, in)
			target := referenceOutput(t)
			if _, err := CopyReferenceNotes(context.Background(), p, root, target); err != nil {
				t.Fatal(err)
			}
			n := p.Notes[0]
			final := filepath.Join(target, filepath.FromSlash(n.Path))
			part := final + referencePartSuffix
			if hardlink {
				if err := os.Link(final, part); err != nil {
					t.Fatal(err)
				}
			} else {
				importWrite(t, target, n.Path+referencePartSuffix, n.After)
			}
			r, err := CopyReferenceNotes(context.Background(), p, root, target)
			if err != nil || !r.Complete || r.Results[0].State != "present" {
				t.Fatalf("Link-before-Remove crash did not recover: %+v %v", r, err)
			}
			if _, err = os.Lstat(part); !os.IsNotExist(err) {
				t.Fatal("identical published residue remains")
			}
			assertReferenceBytes(t, target, n.Path, n.After)
			rollback := referenceOutput(t)
			if _, err = RollbackReferenceNotes(context.Background(), p, target, rollback, []string{}); err != nil {
				t.Fatal("recovered copy cannot roll back", err)
			}
		})
	}
}

func TestReferencePublishedFinalNeverDeletesChangedOrShortenedPart(t *testing.T) {
	for _, partial := range []string{"different content", "prefix"} {
		root, in, _ := referenceFixture(t)
		p := mustReferencePlan(t, root, in)
		target := referenceOutput(t)
		if _, err := CopyReferenceNotes(context.Background(), p, root, target); err != nil {
			t.Fatal(err)
		}
		n := p.Notes[0]
		content := strings.Repeat("x", len(n.After))
		if partial == "prefix" {
			content = n.After[:15]
		}
		importWrite(t, target, n.Path+referencePartSuffix, content)
		if _, err := CopyReferenceNotes(context.Background(), p, root, target); err == nil {
			t.Fatal("incompatible published partial accepted")
		}
		assertReferenceBytes(t, target, n.Path, n.After)
		assertReferenceBytes(t, target, n.Path+referencePartSuffix, content)
	}
}
func manyReferenceNotes(t *testing.T) (string, ReferencePlan) {
	t.Helper()
	root, err := importAbsolute(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	in := ReferenceInput{Schema: ReferenceInputSchema, Mappings: []ReferenceMapping{{OldTarget: "media/hero.png", FixedRef: referenceURI(t)}}}
	for i := 0; i < 100; i++ {
		name := fmt.Sprintf("notes/n%03d.md", i)
		in.Notes = append(in.Notes, name)
		importWrite(t, root, name, strings.Repeat("Synthetic paragraph.\n", 40)+"[Hero](media/hero.png)\n")
	}
	return root, mustReferencePlan(t, root, in)
}
func watchFirstReference(t *testing.T, target string, action func()) <-chan bool {
	t.Helper()
	done := make(chan bool, 1)
	go func() {
		deadline := time.NewTimer(10 * time.Second)
		defer deadline.Stop()
		tick := time.NewTicker(time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-deadline.C:
				done <- false
				return
			case <-tick.C:
				if _, err := os.Stat(filepath.Join(target, "notes/n000.md")); err == nil {
					action()
					done <- true
					return
				}
			}
		}
	}()
	return done
}
func TestReferenceCanceledPartialBatchResumesSamePlan(t *testing.T) {
	root, p := manyReferenceNotes(t)
	target := referenceOutput(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := watchFirstReference(t, target, cancel)
	r, err := CopyReferenceNotes(ctx, p, root, target)
	if !<-done {
		t.Fatal("copy did not publish first note")
	}
	if err == nil || r.Complete {
		t.Fatal("canceled batch falsely complete")
	}
	copied := 0
	for _, x := range r.Results {
		if x.State == "copied" || x.State == "present" {
			copied++
		}
	}
	if copied == 0 || copied >= len(p.Notes) {
		t.Fatal("partial receipt lost progress", copied)
	}
	r, err = CopyReferenceNotes(context.Background(), p, root, target)
	if err != nil || !r.Complete {
		t.Fatalf("resume failed: %+v %v", r, err)
	}
}
func TestReferenceConcurrentSourceEditStopsBatch(t *testing.T) {
	root, p := manyReferenceNotes(t)
	target := referenceOutput(t)
	done := watchFirstReference(t, target, func() { importWrite(t, root, "notes/n099.md", "source edited during copy") })
	r, err := CopyReferenceNotes(context.Background(), p, root, target)
	if !<-done {
		t.Fatal("no first note")
	}
	if err == nil || r.Complete {
		t.Fatal("concurrent source change accepted")
	}
	assertReferenceBytes(t, root, "notes/n099.md", "source edited during copy")
}
func TestReferencePinnedDestinationRejectsRootReplacement(t *testing.T) {
	root, p := manyReferenceNotes(t)
	target := referenceOutput(t)
	outside, err := importAbsolute(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	probe := filepath.Join(t.TempDir(), "probe")
	if err = os.Symlink(outside, probe); err != nil {
		t.Skip(err)
	}
	if err = os.Remove(probe); err != nil {
		t.Fatal(err)
	}
	var replacementBlocked atomic.Bool
	done := watchFirstReference(t, target, func() {
		if err := os.Rename(target, target+"-original"); err != nil {
			if runtime.GOOS == "windows" && os.IsPermission(err) {
				replacementBlocked.Store(true) // The pinned directory handle denies rename.
				return
			}
			t.Error(err)
			return
		}
		if err := os.Symlink(outside, target); err != nil {
			t.Error(err)
		}
	})
	r, err := CopyReferenceNotes(context.Background(), p, root, target)
	if !<-done {
		t.Fatal("no first note")
	}
	if replacementBlocked.Load() {
		if err != nil || !r.Complete {
			t.Fatalf("OS-blocked replacement interrupted safe copy: %+v %v", r, err)
		}

		if err := os.Rename(target, target+"-after-close"); err != nil {
			t.Fatalf("directory remained locked after copy returned: %v", err)
		}
	} else if err == nil || r.Complete {
		t.Fatal("root replacement accepted")
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatal("wrote outside pinned root", err)
	}
}

func TestReferenceSameNoteBasenamesRemainExplicitlyScoped(t *testing.T) {
	root, in, before := referenceFixture(t)
	importWrite(t, root, "other/scene.md", "Other scope\n"+before)
	in.Notes = append(in.Notes, "other/scene.md")
	p := mustReferencePlan(t, root, in)
	target := referenceOutput(t)
	r, err := CopyReferenceNotes(context.Background(), p, root, target)
	if err != nil || !r.Complete || len(r.Results) != 2 {
		t.Fatalf("explicit same-name copies: %+v %v", r, err)
	}
	assertReferenceBytes(t, target, "notes/scene.md", p.Notes[0].After)
	assertReferenceBytes(t, target, "other/scene.md", p.Notes[1].After)
	assertReferenceBytes(t, root, "notes/scene.md", before)
	assertReferenceBytes(t, root, "other/scene.md", "Other scope\n"+before)
}
