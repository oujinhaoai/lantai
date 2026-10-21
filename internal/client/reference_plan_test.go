package client

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

func referenceURI(t *testing.T) string {
	t.Helper()
	u, err := (ids.PermanentRef{InstanceID: ids.New(), AssetID: ids.New(), VersionID: ids.New()}).URI()
	if err != nil {
		t.Fatal(err)
	}
	return u
}
func referenceFixture(t *testing.T) (string, ReferenceInput, string) {
	t.Helper()
	root, err := importAbsolute(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	before := "# Synthetic note\r\n[Hero](media/hero.png \"cover\")\r\n[[media/hero.png|Alias]]\r\n![[media/hero.png|300]]\r\n"
	importWrite(t, root, "notes/scene.md", before)
	in := ReferenceInput{Schema: ReferenceInputSchema, Notes: []string{"notes/scene.md"}, Mappings: []ReferenceMapping{{OldTarget: "media/hero.png", FixedRef: referenceURI(t)}}}
	return root, in, before
}
func mustReferencePlan(t *testing.T, root string, in ReferenceInput) ReferencePlan {
	t.Helper()
	p, err := PrepareReferencePlan(context.Background(), in, root)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Complete {
		t.Fatalf("unresolved: %+v", p.Unresolved)
	}
	return p
}
func referenceOutput(t *testing.T) string {
	t.Helper()
	dir, err := importAbsolute(filepath.Join(t.TempDir(), "copy"))
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestReferencePlanExactLinksLabelsAliasesAndCRLF(t *testing.T) {
	root, in, _ := referenceFixture(t)
	fixed := in.Mappings[0].FixedRef
	before := "---\r\nexample: '[[media/hero.png]]'\r\n---\r\n" +
		"[Hero](media/hero.png \"cover\")\r\n![Preview](<media/hero.png>)\r\n[[media/hero.png|Alias]]\r\n![[media/hero.png|300]]\r\n" +
		"`[[media/hero.png]]`[Again](media/hero.png)\r\n\\[[media/hero.png]] \\[literal](media/hero.png)\r\n" +
		"```md\r\n[[media/hero.png]]\r\n```\r\n<!-- [[media/hero.png]] -->\r\n%% [[media/hero.png]] %%\r\n" +
		"[[Other Note]] [unknown](other/hero.png) [model](models/base.safetensors) [tool](tools/run.py) [cache](cache/output.png)\r\n"
	importWrite(t, root, in.Notes[0], before)
	in.Preserve = []ReferencePreserve{{Target: "models/base.safetensors", Kind: "model", Reason: "public model remains local"}, {Target: "tools/run.py", Kind: "tool", Reason: "tool path remains local"}, {Target: "cache/output.png", Kind: "intermediate", Reason: "cache is not archived"}}
	p := mustReferencePlan(t, root, in)
	if len(p.Notes[0].Edits) != 5 {
		t.Fatalf("wrong edits: %+v", p.Notes[0].Edits)
	}
	want := strings.Replace(before, "[Hero](media/hero.png \"cover\")", "[Hero]("+fixed+" \"cover\")", 1)
	want = strings.Replace(want, "![Preview](<media/hero.png>)", "![Preview](<"+fixed+">)", 1)
	want = strings.Replace(want, "[[media/hero.png|Alias]]", "[Alias]("+fixed+")", 1)
	want = strings.Replace(want, "![[media/hero.png|300]]", "![300]("+fixed+")", 1)
	want = strings.Replace(want, "[Again](media/hero.png)", "[Again]("+fixed+")", 1)
	if p.Notes[0].After != want {
		t.Fatalf("changed a protected byte or label:\n%s", p.Notes[0].After)
	}
	if importSHA([]byte(before)) != p.Notes[0].BeforeSHA256 || importSHA([]byte(want)) != p.Notes[0].AfterSHA256 || !strings.Contains(p.Notes[0].Diff, "@@ bytes ") {
		t.Fatal("missing reversible evidence")
	}
	for _, reason := range []string{"not_selected", "preserved_model", "preserved_tool", "preserved_intermediate", "code_fence", "inline_code", "html_comment", "obsidian_comment", "frontmatter"} {
		found := false
		for _, x := range p.Skipped {
			found = found || x.Reason == reason
		}
		if !found {
			t.Fatal("missing skip classification", reason)
		}
	}
	for i := 0; i < 4; i++ {
		again := mustReferencePlan(t, root, in)
		if again.Digest != p.Digest {
			t.Fatal("non-deterministic plan")
		}
	}
	got, _ := os.ReadFile(filepath.Join(root, filepath.FromSlash(in.Notes[0])))
	if string(got) != before {
		t.Fatal("plan wrote source")
	}
}

func TestReferencePlanNeverGuessesVersionsOrSuffixes(t *testing.T) {
	root, in, _ := referenceFixture(t)
	in.Mappings[0].OldTarget = "hero.png"
	p, err := PrepareReferencePlan(context.Background(), in, root)
	if err != nil || p.Complete || len(p.Unresolved) == 0 || len(p.Notes[0].Edits) != 0 {
		t.Fatalf("suffix/basename was guessed: %+v %v", p, err)
	}
	for _, bad := range []string{"https://example.test/latest", "lantai://anything/assets/a/versions/b", referenceURI(t) + "?latest=1"} {
		in.Mappings[0].FixedRef = bad
		if _, err = PrepareReferencePlan(context.Background(), in, root); err == nil {
			t.Fatal("invalid fixed reference accepted")
		}
	}
}
func TestReferenceUnsupportedMappedSyntaxRefusesComplete(t *testing.T) {
	for _, text := range []string{"[x][hero]\n[hero]: media/hero.png\n", "<media/hero.png>", "<img src=\"media/hero.png\">", "[[media/hero.png|a|b]]", "[x](media/hero.png invalid title)", "`[[media/hero.png]]`", "\\[[media/hero.png]] \\[x](media/hero.png)"} {
		t.Run(text, func(t *testing.T) {
			root, in, _ := referenceFixture(t)
			importWrite(t, root, in.Notes[0], text)
			p, err := PrepareReferencePlan(context.Background(), in, root)
			if err != nil {
				t.Fatal(err)
			}
			if p.Complete || len(p.Unresolved) == 0 {
				t.Fatal("unsupported mapped occurrence falsely complete")
			}
			target := referenceOutput(t)
			if _, err = CopyReferenceNotes(context.Background(), p, root, target); err == nil {
				t.Fatal("unresolved plan copied")
			}
			if _, err = os.Stat(target); !os.IsNotExist(err) {
				t.Fatal("created output for unresolved plan")
			}
		})
	}
}

func TestReferenceNestedImageCannotHideMappedTargetAfterAnotherMatch(t *testing.T) {
	for _, nested := range []string{
		"[![thumb](media/hero.png)](https://example.test/full)",
		"[See [[media/hero.png|Hero]]](https://example.test/full)",
		"[![thumb](media/hero.png)]",
	} {
		t.Run(nested, func(t *testing.T) {
			root, in, _ := referenceFixture(t)
			before := "[x](media/hero.png)\n" + nested + "\n"
			importWrite(t, root, in.Notes[0], before)
			p, err := PrepareReferencePlan(context.Background(), in, root)
			if err != nil {
				t.Fatal(err)
			}
			if p.Complete || len(p.Notes[0].Edits) != 1 {
				t.Fatalf("nested mapped target hidden by outer link: %+v", p)
			}
			found := false
			for _, x := range p.Unresolved {
				if x.Target == in.Mappings[0].OldTarget && x.Reason == "unsupported_nested_markdown_label" {
					found = true
				}
			}
			if !found {
				t.Fatalf("missing nested-label unresolved evidence: %+v", p.Unresolved)
			}
			if !strings.Contains(p.Notes[0].After, nested) {
				t.Fatal("rewrote unsupported nested content")
			}
			output := referenceOutput(t)
			if _, err = CopyReferenceNotes(context.Background(), p, root, output); err == nil {
				t.Fatal("copied incomplete nested-label plan")
			}
			if _, err = os.Stat(output); !os.IsNotExist(err) {
				t.Fatal("created copy for incomplete plan")
			}
			assertReferenceBytes(t, root, in.Notes[0], before)
		})
	}
}

func TestReferenceUnselectedNestedImageDoesNotBlockSelectedBatch(t *testing.T) {
	root, in, _ := referenceFixture(t)
	nested := "[![thumb](media/unselected.png)](https://example.test/full)"
	importWrite(t, root, in.Notes[0], "[x](media/hero.png)\n"+nested+"\n")
	p := mustReferencePlan(t, root, in)
	if len(p.Notes[0].Edits) != 1 || !strings.Contains(p.Notes[0].After, nested) {
		t.Fatal("changed unrelated nested image or expanded selected scope")
	}
}

func TestReferenceNestedWikiAliasCannotHideMappedTarget(t *testing.T) {
	root, in, _ := referenceFixture(t)
	nested := "[[Other|![thumb](media/hero.png)]]"
	importWrite(t, root, in.Notes[0], "[one](media/hero.png)\n"+nested+"\n")
	p, err := PrepareReferencePlan(context.Background(), in, root)
	if err != nil {
		t.Fatal(err)
	}
	if p.Complete || len(p.Notes[0].Edits) != 1 || !strings.Contains(p.Notes[0].After, nested) {
		t.Fatal("nested wiki alias hid selected occurrence")
	}
	found := false
	for _, x := range p.Unresolved {
		found = found || (x.Target == in.Mappings[0].OldTarget && x.Reason == "unsupported_nested_wikilink")
	}
	if !found {
		t.Fatal("missing nested wiki evidence")
	}
}

func TestReferenceInlineRunsDoNotMaskLaterSelectedLinksAsFences(t *testing.T) {
	for _, line := range []string{
		"Text ```code``` [two](media/hero.png)",
		"Text ~~~code~~~ [two](media/hero.png)",
		"```code``` [two](media/hero.png)",
	} {
		root, in, _ := referenceFixture(t)
		before := "[one](media/hero.png)\n" + line + "\n"
		importWrite(t, root, in.Notes[0], before)
		p := mustReferencePlan(t, root, in)
		if len(p.Notes[0].Edits) != 2 {
			t.Fatalf("inline run hid selected link: %q %+v", line, p.Notes[0].Edits)
		}
		if strings.Contains(p.Notes[0].After, "[two](media/hero.png)") {
			t.Fatal("second mapped link unchanged")
		}
	}
	root, in, _ := referenceFixture(t)
	before := "[one](media/hero.png)\n   ```md\n[example](media/hero.png)\nText ``` is not a closer\n   ```\n[two](media/hero.png)\n"
	importWrite(t, root, in.Notes[0], before)
	p := mustReferencePlan(t, root, in)
	if len(p.Notes[0].Edits) != 2 || !strings.Contains(p.Notes[0].After, "[example](media/hero.png)") {
		t.Fatal("real indented fence not preserved exactly")
	}
}

func TestReferenceMultilineDestinationCannotHideBehindAnotherMatch(t *testing.T) {
	for _, nested := range []string{
		"[two](\nmedia/hero.png\n)",
		"[two](\r\n<media/hero.png>\r\n)",
		"[two](media/hero.png\n)",
	} {
		root, in, _ := referenceFixture(t)
		before := "[one](media/hero.png)\n" + nested + "\n"
		importWrite(t, root, in.Notes[0], before)
		p, err := PrepareReferencePlan(context.Background(), in, root)
		if err != nil {
			t.Fatal(err)
		}
		if p.Complete || len(p.Notes[0].Edits) != 1 || !strings.Contains(p.Notes[0].After, nested) {
			t.Fatal("unsupported multiline occurrence hidden")
		}
		found := false
		for _, x := range p.Unresolved {
			found = found || (x.Target == in.Mappings[0].OldTarget && x.Reason == "unsupported_or_ambiguous_markdown")
		}
		if !found {
			t.Fatal("missing multiline unresolved evidence")
		}
		if _, err = CopyReferenceNotes(context.Background(), p, root, referenceOutput(t)); err == nil {
			t.Fatal("copied partially rewritten multiline plan")
		}
	}
}

func TestReferenceMultilineLabelsAndHTMLCannotHideMappedOccurrences(t *testing.T) {
	for _, unsupported := range []string{
		"[two\nline](media/hero.png)",
		"[two\r\nline](<media/hero.png>)",
		"[two `code`\nline](media/hero.png)",
		"[[two\nmedia/hero.png]]",
		"<img\n src=\"media/hero.png\">",
		"<img\r\n alt=`code`\r\n src=\"media/hero.png\">",
		"<img alt=\"> is quoted\"\n src=\"media/hero.png\">",
		"[id]:\n media/hero.png",
		"[unclosed\n media/hero.png",
		"<img\n src=\"media/hero.png\"",
	} {
		t.Run(unsupported, func(t *testing.T) {
			root, in, _ := referenceFixture(t)
			before := "[one](media/hero.png)\n" + unsupported + "\n"
			importWrite(t, root, in.Notes[0], before)
			p, err := PrepareReferencePlan(context.Background(), in, root)
			if err != nil {
				t.Fatal(err)
			}
			if p.Complete || len(p.Notes[0].Edits) != 1 || !strings.Contains(p.Notes[0].After, unsupported) {
				t.Fatalf("multiline structure hid mapped occurrence: %+v", p)
			}
			found := false
			for _, x := range p.Unresolved {
				found = found || x.Target == in.Mappings[0].OldTarget
			}
			if !found {
				t.Fatalf("no target evidence: %+v", p.Unresolved)
			}
			output := referenceOutput(t)
			if _, err = CopyReferenceNotes(context.Background(), p, root, output); err == nil {
				t.Fatal("copied incomplete multiline plan")
			}
			if _, err = os.Stat(output); !os.IsNotExist(err) {
				t.Fatal("created output for incomplete multiline plan")
			}
			assertReferenceBytes(t, root, in.Notes[0], before)
		})
	}
}

func TestReferenceUnselectedMultilineStructureDoesNotExpandBatch(t *testing.T) {
	root, in, _ := referenceFixture(t)
	unselected := "[two\nline](media/unselected.png)\n<img\n src=\"media/unselected.png\">\n"
	importWrite(t, root, in.Notes[0], "[one](media/hero.png)\n"+unselected)
	p := mustReferencePlan(t, root, in)
	if len(p.Notes[0].Edits) != 1 || !strings.Contains(p.Notes[0].After, unselected) {
		t.Fatal("expanded scope into unrelated multiline targets")
	}
}
func TestReferenceInputStrictAndProtectedSelections(t *testing.T) {
	for _, mutate := range []func(*ReferenceInput){
		func(in *ReferenceInput) { in.Notes = append(in.Notes, "notes/scene.md") },
		func(in *ReferenceInput) { in.Notes = []string{"../scene.md"} },
		func(in *ReferenceInput) { in.Notes = []string{"scene.canvas"} },
		func(in *ReferenceInput) { in.Notes = []string{"A/a.md", "a/b.md"} },
		func(in *ReferenceInput) { in.Notes = []string{"cafe\u0301.md"} },
		func(in *ReferenceInput) { in.Mappings = append(in.Mappings, in.Mappings[0]) },
		func(in *ReferenceInput) { in.Mappings[0].OldTarget = "models/base.safetensors" },
		func(in *ReferenceInput) { in.Mappings[0].OldTarget = "tools/run.py" },
		func(in *ReferenceInput) { in.Mappings[0].OldTarget = "cache/image.png" },
		func(in *ReferenceInput) {
			in.Preserve = []ReferencePreserve{{Target: in.Mappings[0].OldTarget, Kind: "model", Reason: "protected"}}
		},
	} {
		root, in, _ := referenceFixture(t)
		mutate(&in)
		if _, err := PrepareReferencePlan(context.Background(), in, root); err == nil {
			t.Fatal("unsafe input accepted", in)
		}
	}
	for _, raw := range []string{`{"schema":"lantai.reference-input/v1","notes":[],"mapping":[]}`, `{"schema":"wrong","schema":"lantai.reference-input/v1","notes":[],"mappings":[]}`} {
		var in ReferenceInput
		if json.Unmarshal([]byte(raw), &in) == nil {
			t.Fatal("unknown/duplicate JSON accepted")
		}
	}
}
func TestReferenceSnapshotRegenerationRejectsRehashedAfterEdits(t *testing.T) {
	root, in, _ := referenceFixture(t)
	p := mustReferencePlan(t, root, in)
	for _, mutation := range []string{"after", "edits", "diff", "skipped", "complete"} {
		raw, _ := json.Marshal(p)
		var changed ReferencePlan
		_ = json.Unmarshal(raw, &changed)
		switch mutation {
		case "after":
			changed.Notes[0].After += "\nInjected"
			changed.Notes[0].AfterSHA256 = importSHA([]byte(changed.Notes[0].After))
		case "edits":
			changed.Notes[0].Edits[0].After = "wrong"
		case "diff":
			changed.Notes[0].Diff = "wrong"
		case "skipped":
			changed.Skipped = append(changed.Skipped, ReferenceIssue{Reason: "fake"})
		case "complete":
			changed.Complete = false
		}
		changed.Digest, _ = referencePlanDigest(changed)
		if validateReferenceSnapshot(changed) == nil {
			t.Fatal("self-rehashed tampering accepted", mutation)
		}
	}
}
func TestReferencePlanRejectsSymlinkAndExactCaseMismatch(t *testing.T) {
	root, in, before := referenceFixture(t)
	outside := t.TempDir()
	importWrite(t, outside, "scene.md", before)
	if err := os.Remove(filepath.Join(root, filepath.FromSlash(in.Notes[0]))); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "scene.md"), filepath.Join(root, filepath.FromSlash(in.Notes[0]))); err == nil {
		if _, err := PrepareReferencePlan(context.Background(), in, root); err == nil {
			t.Fatal("symlink source accepted")
		}
	}
	root, in, _ = referenceFixture(t)
	in.Notes[0] = "NOTES/scene.md"
	if _, err := PrepareReferencePlan(context.Background(), in, root); err == nil {
		t.Fatal("case alias source accepted")
	}
}

func TestReferenceLongMalformedInputIsBoundedAndUnresolved(t *testing.T) {
	root, in, _ := referenceFixture(t)
	for _, bad := range []string{strings.Repeat("[", MaxReferenceNoteBytes-32) + "media/hero.png", strings.Repeat("[a", 150000) + "media/hero.png" + strings.Repeat("]", 150000), strings.Repeat("\\", MaxReferenceNoteBytes-33) + "[x](media/hero.png)", strings.Repeat("<", MaxReferenceNoteBytes-32) + "media/hero.png"} {
		importWrite(t, root, in.Notes[0], bad)
		start := time.Now()
		p, err := PrepareReferencePlan(context.Background(), in, root)
		if err != nil && !strings.Contains(err.Error(), "exceeds") {
			t.Fatal(err)
		}
		if err == nil && p.Complete {
			t.Fatal("malformed selected scope claimed complete")
		}
		if time.Since(start) > 5*time.Second {
			t.Fatal("malformed input exceeded bounded scanner budget")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := PrepareReferencePlan(ctx, in, root); err == nil {
		t.Fatal("canceled plan continued")
	}
}
