package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/client"
)

const referenceCLIv1 = "lantai://01J0000000000000000000000A/assets/01J0000000000000000000000B/versions/01J0000000000000000000000C"
const referenceCLIv2 = "lantai://01J0000000000000000000000A/assets/01J0000000000000000000000B/versions/01J0000000000000000000000D"

func TestAssetReferencesOfflineCopyAndRollbackRetainNewVersion(t *testing.T) {
	t.Setenv("LANTAI_SERVER", "")
	t.Setenv("LANTAI_SESSION_TOKEN", "")
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "notes")
	if err = os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	before := "# Synthetic note\r\n\r\n![[legacy/image.png|Preview]]\r\n[Other note](other.md)\r\n`[example](legacy/image.png)`\r\n"
	if err = os.WriteFile(filepath.Join(source, "note.md"), []byte(before), 0600); err != nil {
		t.Fatal(err)
	}
	in, planPath := filepath.Join(dir, "input.json"), filepath.Join(dir, "plan.json")
	assetDocument(t, in, client.ReferenceInput{Schema: client.ReferenceInputSchema, Notes: []string{"note.md"}, Mappings: []client.ReferenceMapping{{OldTarget: "legacy/image.png", FixedRef: referenceCLIv1}}})
	if code, out, stderr := assetInvoke(t, "references", "plan", "--input", in, "--directory", source, "--output", planPath); code != 0 || stderr != "" || !strings.Contains(out, "before_sha256") {
		t.Fatalf("plan=%d %s %s", code, out, stderr)
	}
	var plan client.ReferencePlan
	if err = client.LoadState(planPath, &plan); err != nil || !plan.Complete || len(plan.Notes) != 1 || len(plan.Notes[0].Edits) != 1 {
		t.Fatalf("plan: %+v %v", plan, err)
	}
	copyRoot, copyReceipt := filepath.Join(dir, "copy"), filepath.Join(dir, "copy-receipt.json")
	copyArgs := []string{"references", "copy", "--input", planPath, "--directory", source, "--destination", copyRoot, "--output", copyReceipt}
	for range 2 {
		if code, out, stderr := assetInvoke(t, copyArgs...); code != 0 || stderr != "" || !strings.Contains(out, `"complete": true`) {
			t.Fatalf("copy=%d %s %s", code, out, stderr)
		}
	}
	after, err := os.ReadFile(filepath.Join(copyRoot, "note.md"))
	if err != nil || !strings.Contains(string(after), "![Preview]("+referenceCLIv1+")") || !strings.Contains(string(after), "[Other note](other.md)\r\n") || !strings.Contains(string(after), "`[example](legacy/image.png)`") {
		t.Fatalf("copy bytes=%q %v", after, err)
	}
	refs, rollbackRoot := filepath.Join(dir, "post-cutover.json"), filepath.Join(dir, "rollback")
	assetDocument(t, refs, []string{referenceCLIv2})
	if code, out, stderr := assetInvoke(t, "references", "rollback", "--input", planPath, "--directory", copyRoot, "--destination", rollbackRoot, "--post-cutover-refs", refs, "--output", filepath.Join(source, "note.md")); code != 2 || out != "" || !strings.Contains(stderr, "outside") {
		t.Fatalf("rollback source output accepted=%d %s %s", code, out, stderr)
	}
	code, out, stderr := assetInvoke(t, "references", "rollback", "--input", planPath, "--directory", copyRoot, "--destination", rollbackRoot, "--post-cutover-refs", refs, "--output", filepath.Join(dir, "rollback-receipt.json"))
	if code != 0 || stderr != "" || !strings.Contains(out, referenceCLIv2) || !strings.Contains(out, `"complete": true`) {
		t.Fatalf("rollback=%d %s %s", code, out, stderr)
	}
	for _, path := range []string{filepath.Join(source, "note.md"), filepath.Join(rollbackRoot, "note.md")} {
		raw, err := os.ReadFile(path)
		if err != nil || string(raw) != before {
			t.Fatalf("original/rollback changed %s: %q %v", path, raw, err)
		}
	}
	stillAfter, err := os.ReadFile(filepath.Join(copyRoot, "note.md"))
	if err != nil || string(stillAfter) != string(after) {
		t.Fatalf("rollback modified managed copy: %q %v", stillAfter, err)
	}
	if err = os.WriteFile(filepath.Join(copyRoot, "note.md"), append(after, []byte("local edit\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := assetInvoke(t, copyArgs...); code == 0 {
		t.Fatal("copy silently overwrote a local edit")
	}
}

func TestAssetReferencesPartialPlanAndOutputGuards(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "notes")
	if err = os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	before := []byte("[other](other.md)\n")
	if err = os.WriteFile(filepath.Join(source, "note.md"), before, 0600); err != nil {
		t.Fatal(err)
	}
	in, plan := filepath.Join(dir, "input.json"), filepath.Join(dir, "plan.json")
	assetDocument(t, in, client.ReferenceInput{Schema: client.ReferenceInputSchema, Notes: []string{"note.md"}, Mappings: []client.ReferenceMapping{{OldTarget: "absent.png", FixedRef: referenceCLIv1}}})
	code, out, stderr := assetInvoke(t, "references", "plan", "--input", in, "--directory", source, "--output", plan)
	var value client.ReferencePlan
	if code == 0 || json.Unmarshal([]byte(out), &value) != nil || value.Complete || len(value.Unresolved) == 0 || !strings.Contains(stderr, "unresolved") {
		t.Fatalf("partial=%d %s %s", code, out, stderr)
	}
	if _, err = client.ReadPrivate(plan); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"references", "plan", "--input", in, "--directory", source, "--output", filepath.Join(source, "note.md")},
		{"references", "copy", "--input", plan, "--directory", source, "--destination", filepath.Join(dir, "future"), "--output", filepath.Join(dir, "FUTURE", "receipt.json")},
		{"references", "rollback", "--input", plan, "--directory", source, "--destination", filepath.Join(dir, "rollback"), "--post-cutover-refs", in, "--output", in},
	} {
		if code, out, _ := assetInvoke(t, args...); code != 2 || out != "" {
			t.Fatalf("unsafe output %v = %d %s", args, code, out)
		}
	}
	raw, err := os.ReadFile(filepath.Join(source, "note.md"))
	if err != nil || string(raw) != string(before) {
		t.Fatalf("source changed=%q %v", raw, err)
	}
}

func TestAssetReferencesOutputParentSwapDoesNotOverwriteSource(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source, reports, target := filepath.Join(dir, "source"), filepath.Join(dir, "reports"), filepath.Join(dir, "copy")
	for _, root := range []string{source, reports} {
		if err = os.Mkdir(root, 0700); err != nil {
			t.Fatal(err)
		}
	}
	probe := filepath.Join(dir, "symlink-probe")
	if err = os.Symlink(source, probe); err != nil {
		t.Skip("symlink unavailable", err)
	}
	if err = os.Remove(probe); err != nil {
		t.Fatal(err)
	}
	before := "[image](legacy/image.png)\n"
	in := client.ReferenceInput{Schema: client.ReferenceInputSchema, Mappings: []client.ReferenceMapping{{OldTarget: "legacy/image.png", FixedRef: referenceCLIv1}}}
	for i := 0; i < 100; i++ {
		name := fmt.Sprintf("n%03d.md", i)
		in.Notes = append(in.Notes, name)
		if err = os.WriteFile(filepath.Join(source, name), []byte(before), 0600); err != nil {
			t.Fatal(err)
		}
	}
	plan, err := client.PrepareReferencePlan(t.Context(), in, source)
	if err != nil {
		t.Fatal(err)
	}
	planFile := filepath.Join(dir, "plan.json")
	assetDocument(t, planFile, plan)
	swapped := make(chan error, 1)
	go func() {
		deadline := time.NewTimer(15 * time.Second)
		defer deadline.Stop()
		tick := time.NewTicker(time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-deadline.C:
				swapped <- fmt.Errorf("first copied note not observed")
				return
			case <-tick.C:
				if _, err := os.Stat(filepath.Join(target, "n000.md")); err != nil {
					continue
				}
				if err := os.Rename(reports, reports+"-original"); err != nil {
					swapped <- err
					return
				}
				swapped <- os.Symlink(source, reports)
				return
			}
		}
	}()
	code, out, stderr := assetInvoke(t, "references", "copy", "--input", planFile, "--directory", source, "--destination", target, "--output", filepath.Join(reports, "n000.md"))
	if err = <-swapped; err != nil {
		t.Fatal(err)
	}
	if code == 0 || out != "" || !strings.Contains(stderr, "output parent") {
		t.Fatalf("retargeted output accepted=%d %s %s", code, out, stderr)
	}
	for _, name := range in.Notes {
		got, err := os.ReadFile(filepath.Join(source, name))
		if err != nil || string(got) != before {
			t.Fatalf("source changed %s=%q %v", name, got, err)
		}
	}
	entries, err := os.ReadDir(source)
	if err != nil || len(entries) != len(in.Notes) {
		t.Fatalf("source received temporary outputs=%d %v", len(entries), err)
	}
}
