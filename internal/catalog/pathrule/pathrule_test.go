package pathrule

import (
	"strings"
	"testing"

	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/schema"
)

func reason(t *testing.T, err error) string {
	t.Helper()
	e, ok := errcode.As(err)
	if !ok || len(e.Details) == 0 {
		t.Fatalf("want structured error with details, got %v", err)
	}
	return e.Details[0].Reason
}

func TestNormalizeAcceptsPortablePaths(t *testing.T) {
	for in, want := range map[string]string{
		"README.md":                      "README.md",
		"model/body.glb":                 "model/body.glb",
		".gitignore":                     ".gitignore",
		"..hidden/x":                     "..hidden/x",
		"console.txt":                    "console.txt",
		"comics/com10.png":               "comics/com10.png",
		"镜头/第3章 出口.mp4":                  "镜头/第3章 出口.mp4",
		"café/menu.txt":                 "café/menu.txt", // NFD 输入存成 NFC
		" leading-space-is-portable.txt": " leading-space-is-portable.txt",
	} {
		got, err := Normalize(in)
		if err != nil {
			t.Errorf("Normalize(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
		if err := Check(got); err != nil {
			t.Errorf("Check(%q) after normalize: %v", got, err)
		}
	}
}

func TestNormalizeRejects(t *testing.T) {
	long := strings.Repeat("a", MaxPathRunes+1)
	cases := map[string]string{
		"":                  ReasonEmpty,
		"/etc/passwd":       ReasonAbsolute,
		"dir/":              ReasonTrailingSlash,
		"a//b":              ReasonEmptySegment,
		"./a":               ReasonDotSegment,
		"a/../../b":         ReasonDotSegment,
		`dir\file`:          ReasonBackslash,
		"a\x00b":            ReasonControlCharacter,
		"a\tb":              ReasonControlCharacter,
		"a\u0085b":          ReasonControlCharacter,
		"evil\u202egpj.exe": ReasonBidiControl,
		"what?.txt":         ReasonReservedCharacter,
		"a:b":               ReasonReservedCharacter,
		`quote".txt`:        ReasonReservedCharacter,
		"CON":               ReasonReservedName,
		"sub/nul.tar.gz":    ReasonReservedName,
		"Com1.log":          ReasonReservedName,
		"lpt¹":              ReasonReservedName,
		"CON .txt":          ReasonReservedName,
		"name.":             ReasonTrailingDotOrSpace,
		"name ":             ReasonTrailingDotOrSpace,
		"dir./x":            ReasonTrailingDotOrSpace,
		long:                ReasonPathTooLong,
		strings.Repeat("字", 80) + "/" + strings.Repeat("字", 80): "",                   // 161 个字符、每段 240 字节：合法
		strings.Repeat("字", 86):                                 ReasonSegmentTooLong, // 86 个字符但 258 字节
	}
	for in, want := range cases {
		_, err := Normalize(in)
		if want == "" {
			if err != nil {
				t.Errorf("Normalize(%q) rejected: %v", in, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("Normalize(%q) accepted, want %s", in, want)
			continue
		}
		if errcode.CodeOf(err) != errcode.SchemaInvalid {
			t.Errorf("Normalize(%q) code %s", in, errcode.CodeOf(err))
		}
		if got := reason(t, err); got != want {
			t.Errorf("Normalize(%q) reason %s, want %s", in, got, want)
		}
	}
	if _, err := Normalize("bad\xff"); reason(t, err) != ReasonInvalidUTF8 {
		t.Fatal("invalid UTF-8 accepted")
	}
}

func TestCheckRequiresNFC(t *testing.T) {
	if err := Check("café"); reason(t, err) != ReasonNotNormalized {
		t.Fatalf("NFD path passed Check: %v", err)
	}
}

// 路径与 schema 中 relative_path 的规则一致：凡本包接受的路径，schema 也接受。
func TestAcceptedPathsSatisfySchema(t *testing.T) {
	reg, err := schema.Default()
	if err != nil {
		t.Fatal(err)
	}
	longCJK := strings.Repeat(strings.Repeat("字", 49)+"/", 3) + strings.Repeat("字", 50) // 恰好 200 个字符
	for _, p := range []string{"README.md", ".hidden/..x", "镜头/第3章 出口.mp4", longCJK} {
		n, err := Normalize(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := reg.Validate("lantai.common-defs/v1#/$defs/relative_path", n); err != nil {
			t.Errorf("schema rejects %q: %v", n, err)
		}
		if err := reg.Validate("lantai.common-defs/v1#/$defs/normalized_slug", n); err != nil {
			t.Errorf("slug schema rejects %q: %v", n, err)
		}
	}
}

func TestConflicts(t *testing.T) {
	ok := []string{"a/b.txt", "a/c.txt", "b", "straße/x"}
	if err := CheckSet(ok); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		paths  []string
		reason string
	}{
		{[]string{"Readme.md", "README.md"}, ReasonSameName},
		{[]string{"café", "CAFÉ"}, ReasonSameName},
		{[]string{"strasse", "STRASSE"}, ReasonSameName},
		{[]string{"straße", "strasse"}, ReasonSameName}, // 完全折叠更保守
		{[]string{"a", "a/b"}, ReasonFileDirectory},
		{[]string{"A/b/c", "a/B"}, ReasonFileDirectory},
	}
	for _, c := range cases {
		err := CheckSet(c.paths)
		if errcode.CodeOf(err) != errcode.PathConflict {
			t.Errorf("CheckSet(%q) = %v, want PATH_CONFLICT", c.paths, err)
			continue
		}
		if got := reason(t, err); got != c.reason {
			t.Errorf("CheckSet(%q) reason %s, want %s", c.paths, got, c.reason)
		}
	}
}

func TestKeyFoldsCaseAndForm(t *testing.T) {
	if Key("Café/Menu") != Key("CAFÉ/menu") {
		t.Fatal("case variants must share a key")
	}
	if Key("a/b") == Key("a/c") {
		t.Fatal("different names share a key")
	}
}

func TestSlugAndProjectKey(t *testing.T) {
	if s, err := NormalizeSlug("ch03/whitebox"); err != nil || s != "ch03/whitebox" {
		t.Fatalf("slug = %q %v", s, err)
	}
	if _, err := NormalizeSlug("ch03/white@box"); reason(t, err) != ReasonAtSign {
		t.Fatal("slug with @ accepted")
	}
	if err := CheckSlug("ch03/white@box"); reason(t, err) != ReasonAtSign {
		t.Fatal("CheckSlug accepted @")
	}
	for _, k := range []string{"pansi", "library", "a", "proj-2"} {
		if err := CheckProjectKey(k); err != nil {
			t.Errorf("CheckProjectKey(%q): %v", k, err)
		}
	}
	for _, k := range []string{"", "Pansi", "2proj", "a_b", "a/b", "proj-", strings.Repeat("a", 64)} {
		if err := CheckProjectKey(k); err == nil {
			t.Errorf("CheckProjectKey(%q) accepted", k)
		}
	}
}
