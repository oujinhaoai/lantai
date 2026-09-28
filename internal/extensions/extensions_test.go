package extensions

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/execution"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	"github.com/oujinhaoai/lantai/internal/platform/sqlite"
	"github.com/oujinhaoai/lantai/internal/platform/sqlite/migrations"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/plugins"
)

func packageFixture(t *testing.T) fstest.MapFS {
	t.Helper()
	f, e := fs.Sub(plugins.Files, "corecheck")
	if e != nil {
		t.Fatal(e)
	}
	out := fstest.MapFS{}
	e = fs.WalkDir(f, ".", func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			return nil
		}
		b, e := fs.ReadFile(f, p)
		out[p] = &fstest.MapFile{Data: b, Mode: 0600}
		return e
	})
	if e != nil {
		t.Fatal(e)
	}
	return out
}
func basePackage(t *testing.T) Package {
	t.Helper()
	p, e := ReadPackage(packageFixture(t))
	if e != nil {
		t.Fatal(e)
	}
	return p
}
func mustCode(t *testing.T, e error, c errcode.Code) {
	t.Helper()
	if c == "" {
		if e != nil {
			t.Fatal(e)
		}
		return
	}
	if e == nil || errcode.CodeOf(e) != c {
		t.Fatalf("want %s got %v", c, e)
	}
}
func TestManifestAndReservedCapabilities(t *testing.T) {
	cases := []struct {
		name  string
		alter func(*Manifest)
		code  errcode.Code
	}{
		{"builtin", func(*Manifest) {}, ""},
		{"future_host", func(m *Manifest) { m.Compatibility.HostAPI = ">=2.0.0 <3.0.0" }, errcode.ExtensionPointUnsupported},
		{"point_version", func(m *Manifest) { m.Compatibility.RequiredPoints[0].Version = 2 }, errcode.ExtensionPointUnsupported},
		{"unknown_point", func(m *Manifest) { m.Contributes[0].Point = "custom.unknown" }, errcode.ExtensionPointUnsupported},
		{"point_contract", func(m *Manifest) { m.Contributes[0].OutputSchema = "lantai.agent-result/v1" }, errcode.ExtensionPointUnsupported},
		{"web_activation", func(m *Manifest) { m.Targets["web"] = m.Targets["server"] }, errcode.ExtensionPointUnsupported},
		{"cli_activation", func(m *Manifest) { m.Targets["cli"] = m.Targets["server"] }, errcode.ExtensionPointUnsupported},
		{"external_process", func(m *Manifest) {
			x := m.Targets["server"]
			x.Runtime = "exec"
			x.Lifecycle = "oneshot"
			m.Targets["server"] = x
		}, errcode.ExtensionPointUnsupported},
		{"requires_not_di_or_path", func(m *Manifest) {
			x := m.Targets["server"]
			x.Processor.Requires = []string{"fake-tool"}
			m.Targets["server"] = x
		}, errcode.ExtensionPointUnsupported},
		{"duplicate_contribution", func(m *Manifest) { x := m.Contributes[0]; x.Target = "node"; m.Contributes = append(m.Contributes, x) }, errcode.IdempotencyConflict},
		{"namespace_escape", func(m *Manifest) { m.Contributes[0].ID = "org.other.check" }, errcode.SchemaInvalid},
		{"new_permissions", func(m *Manifest) { m.Permissions.API = []string{"identity.issue_credential"} }, errcode.ExtensionPointUnsupported},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { m := basePackage(t).Manifest; c.alter(&m); mustCode(t, CheckM1(m, nil), c.code) })
	}
	m := basePackage(t).Manifest
	raw, _ := json.Marshal(m)
	var v map[string]any
	json.Unmarshal(raw, &v)
	v["dependencies"] = []any{map[string]any{"id": "org.example.other", "version": "1.0.0"}}
	raw, _ = json.Marshal(v)
	m, e := Parse(raw)
	mustCode(t, e, "")
	mustCode(t, CheckM1(m, nil), errcode.ExtensionPointUnsupported)
	v["contract"] = "lantai.extension/v2"
	raw, _ = json.Marshal(v)
	_, e = Parse(raw)
	mustCode(t, e, errcode.SchemaInvalid)
}
func TestPackageRejectsLegacyReplacementAndUnsafeFiles(t *testing.T) {
	for _, legacy := range []string{"plugin.yaml", "module.yaml"} {
		f := packageFixture(t)
		f[legacy] = &fstest.MapFile{Data: []byte("contract: old")}
		_, e := ReadPackage(f)
		mustCode(t, e, errcode.SchemaInvalid)
		delete(f, "extension.yaml")
		_, e = ReadPackage(f)
		mustCode(t, e, errcode.SchemaInvalid)
	}
	f := packageFixture(t)
	f["check.go"].Data = append(f["check.go"].Data, ' ')
	_, e := ReadPackage(f)
	mustCode(t, e, errcode.HashMismatch)
	f = packageFixture(t)
	f["check.go"].Mode = os.ModeSymlink
	_, e = ReadPackage(f)
	mustCode(t, e, errcode.SchemaInvalid)
	f = packageFixture(t)
	f["extra.txt"] = &fstest.MapFile{Data: []byte("unlisted")}
	_, e = ReadPackage(f)
	mustCode(t, e, errcode.SchemaInvalid)
	f = packageFixture(t)
	raw := string(f["extension.yaml"].Data)
	f["extension.yaml"].Data = []byte(strings.Replace(raw, "check.go", "../escape.go", 1))
	_, e = ReadPackage(f)
	mustCode(t, e, errcode.SchemaInvalid)
}

func TestPackageRejectsDeclaredFileLineEndingMutation(t *testing.T) {
	for _, entry := range basePackage(t).Manifest.Files {
		t.Run(entry.Path, func(t *testing.T) {
			f := packageFixture(t)
			original := f[entry.Path].Data
			changed := bytes.ReplaceAll(original, []byte("\n"), []byte("\r\n"))
			if bytes.Equal(original, changed) {
				t.Fatal("fixture has no line endings to mutate")
			}
			f[entry.Path].Data = changed
			_, err := ReadPackage(f)
			mustCode(t, err, errcode.HashMismatch)
		})
	}
}

func TestBuiltinPackageCheckoutPreservesByteIdentity(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("Git is required to exercise checkout line-ending conversion")
	}
	attrs, err := os.ReadFile(filepath.Join("..", "..", ".gitattributes"))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	packageRoot := filepath.Join(root, "plugins", "corecheck")
	if err := os.MkdirAll(packageRoot, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".gitattributes"), attrs, 0600); err != nil {
		t.Fatal(err)
	}
	files := packageFixture(t)
	for name, file := range files {
		if err := os.WriteFile(filepath.Join(packageRoot, name), file.Data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	// Exercise actual Git clean/smudge behavior on every platform, including
	// Unix CI. No commit or change to the source repository is needed.
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(git, append([]string{"-c", "core.autocrlf=true", "-c", "core.eol=crlf", "-c", "core.safecrlf=false"}, args...)...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "--quiet")
	run("add", "--", ".gitattributes", "plugins/corecheck")
	run("checkout-index", "--force", "--all")
	for name, file := range files {
		got, err := os.ReadFile(filepath.Join(packageRoot, name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, file.Data) {
			t.Fatalf("checkout changed embedded %s bytes (%d -> %d)", name, len(file.Data), len(got))
		}
	}
	got, err := ReadPackage(os.DirFS(packageRoot))
	mustCode(t, err, "")
	if want := basePackage(t); got.Digest != want.Digest || got.ManifestDigest != want.ManifestDigest {
		t.Fatal("checkout changed builtin package identity")
	}
}

func newRegistry(t *testing.T) (*Registry, context.Context) {
	t.Helper()
	db, e := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "main.db"), sqlite.Options{})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { db.Close() })
	for _, m := range migrations.For(ownership.Main) {
		if _, e = db.ExecContext(t.Context(), m.SQL); e != nil {
			t.Fatal(e)
		}
	}
	gate := commands.NewGate(commands.NewCoordinator())
	ctx, h, e := gate.Maintain(t.Context(), commands.ReasonStarting)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(h.Release)
	r, e := New(Deps{db, gate, digest.Of([]byte("release one"))})
	if e != nil {
		t.Fatal(e)
	}
	return r, ctx
}
func count(t *testing.T, r *Registry, table string) int {
	t.Helper()
	var n int
	if e := r.db.QueryRow("SELECT count(*) FROM " + table).Scan(&n); e != nil {
		t.Fatal(e)
	}
	return n
}
func TestDurableStaticRegistrationAndReleaseBinding(t *testing.T) {
	r, ctx := newRegistry(t)
	mustCode(t, r.RegisterBuiltins(context.Background()), errcode.MaintenanceMode)
	mustCode(t, r.RegisterBuiltins(ctx), "")
	mustCode(t, r.RegisterBuiltins(ctx), "")
	if count(t, r, "extensions_registrations") != 1 || count(t, r, "extensions_release_bindings") != 1 {
		t.Fatal("duplicate registration")
	}
	p, e := r.Producer(ctx, "org.lantai.corecheck.manifest")
	mustCode(t, e, "")
	mustCode(t, r.VerifyProducer(ctx, p), "")
	copy, e := New(Deps{r.db, r.gate, r.release})
	mustCode(t, e, "")
	mustCode(t, copy.VerifyProducer(ctx, p), "") // restart uses durable records
	next, e := New(Deps{r.db, r.gate, digest.Of([]byte("release two"))})
	mustCode(t, e, "")
	mustCode(t, next.VerifyProducer(ctx, p), errcode.ExtensionActivationStale)
	mustCode(t, next.RegisterBuiltins(ctx), "")
	q, e := next.Producer(ctx, p.ContributionID)
	mustCode(t, e, "")
	if q.PackageDigest != p.PackageDigest || q.CoreReleaseDigest == p.CoreReleaseDigest || count(t, r, "extensions_release_bindings") != 2 {
		t.Fatal("package identity or release history changed")
	}
	if count(t, r, "extensions_registrations") != 1 {
		t.Fatal("core rebuild copied package")
	}
	list, e := next.List(ctx)
	mustCode(t, e, "")
	if len(list) != 1 || list[0].CoreReleaseDigest != next.release {
		t.Fatal(list)
	}
	p.Source = "package"
	p.CoreReleaseDigest = ""
	mustCode(t, next.VerifyProducer(ctx, p), errcode.ExtensionActivationStale)
	p = q
	p.CoreReleaseDigest = ""
	// v1 historical producer shape remains readable; it cannot prove a current
	// builtin identity and must not be accepted as a new result.
	mustCode(t, validate("lantai.common-defs/v1#/$defs/producer_ref", p), "")
	mustCode(t, next.VerifyProducer(ctx, p), errcode.ExtensionActivationStale)
	p = q
	p.PackageDigest = digest.Of([]byte("substituted"))
	mustCode(t, next.VerifyProducer(ctx, p), errcode.ExtensionActivationStale)
	p = q
	p.ContributionID = "org.lantai.corecheck.unknown"
	mustCode(t, next.VerifyProducer(ctx, p), errcode.ExtensionPointUnsupported)
}
func TestRegistrationConflictAndAtomicRollback(t *testing.T) {
	r, ctx := newRegistry(t)
	p := basePackage(t)
	mustCode(t, r.register(ctx, []Package{p, p}), errcode.IdempotencyConflict)
	if count(t, r, "extensions_registrations") != 0 {
		t.Fatal("partial duplicate")
	}
	// Force failure after package/contribution insert, proving all three tables rollback.
	if _, e := r.db.Exec(`CREATE TRIGGER fail_binding BEFORE INSERT ON extensions_release_bindings BEGIN SELECT RAISE(ABORT,'synthetic failure'); END`); e != nil {
		t.Fatal(e)
	}
	if e := r.RegisterBuiltins(ctx); e == nil {
		t.Fatal("trigger did not fail")
	}
	for _, table := range []string{"extensions_registrations", "extensions_contributions", "extensions_release_bindings"} {
		if count(t, r, table) != 0 {
			t.Fatal("partial record", table)
		}
	}
	r.db.Exec(`DROP TRIGGER fail_binding`)
	mustCode(t, r.RegisterBuiltins(ctx), "")
	p.Digest = digest.Of([]byte("other artifact"))
	mustCode(t, r.register(ctx, []Package{p}), errcode.IdempotencyConflict)
	if count(t, r, "extensions_registrations") != 1 {
		t.Fatal("replacement altered rows")
	}
}
func TestBuiltinCheckProducesTraceableEvidenceOnly(t *testing.T) {
	r, ctx := newRegistry(t)
	mustCode(t, r.RegisterBuiltins(ctx), "")
	b, e := os.ReadFile("../../schemas/examples/catalog/v1/manifest/valid.json")
	if e != nil {
		t.Fatal(e)
	}
	var w struct {
		Document map[string]any `json:"document"`
	}
	json.Unmarshal(b, &w)
	content, _ := canonjson.CanonicalizeValue(w.Document["content"])
	expected := digest.Of(content)
	w.Document["manifest_digest"] = expected
	raw, _ := json.Marshal(w.Document)
	ref := ids.PermanentRef{InstanceID: ids.ID(w.Document["instance_id"].(string)), AssetID: ids.ID(w.Document["asset_id"].(string)), VersionID: ids.ID(w.Document["version_id"].(string))}
	good, e := r.ValidateManifest(ctx, raw, ref, expected)
	mustCode(t, e, "")
	if good.Verdict != execution.VerdictPass {
		t.Fatal(good)
	}
	mustCode(t, r.VerifyProducer(ctx, good.Producer), "")
	bad, e := r.ValidateManifest(ctx, raw, ref, digest.Of([]byte("wrong")))
	mustCode(t, e, "")
	if bad.Verdict != execution.VerdictFail {
		t.Fatal("invalid input passed")
	}
}
func TestCurrentExecutableDigestStable(t *testing.T) {
	a, e := CurrentReleaseDigest()
	mustCode(t, e, "")
	b, e := CurrentReleaseDigest()
	mustCode(t, e, "")
	if a != b || !a.Valid() {
		t.Fatal("unstable release digest")
	}
}

func TestDeclaredNodeProcessorAndHostToolMetadata(t *testing.T) {
	m := basePackage(t).Manifest
	m.Compatibility.RequiredPoints[0].ID = "artifact.processor"
	c := &m.Contributes[0]
	c.Point, c.Target = "artifact.processor", "node"
	c.InputSchema, c.OutputSchema = "lantai.processor-input/v1", "lantai.processor-result/v1"
	target := m.Targets["server"]
	target.Processor.Requires = []string{"declared-tool"}
	m.Targets = map[string]Target{"node": target}
	tool := HostTool{ID: "declared-tool", Version: "1.0", Path: filepath.Join(t.TempDir(), "not-executed"), Digest: digest.Of([]byte("declared tool"))}
	// The path deliberately does not exist: static metadata checking never probes
	// or executes capabilities, and does not claim the tool is runnable.
	mustCode(t, CheckM1(m, []HostTool{tool}), "")
	tool.Path = "from-PATH"
	mustCode(t, CheckM1(m, []HostTool{tool}), errcode.SchemaInvalid)
}

func TestConfigSchemaCannotLoadExternalResources(t *testing.T) {
	f := packageFixture(t)
	m, e := Parse(f["extension.yaml"].Data)
	mustCode(t, e, "")
	b := []byte(`{"$ref":"https://invalid.example/untrusted-schema.json"}`)
	f["config.schema.json"].Data = b
	for i := range m.Files {
		if m.Files[i].Path == "config.schema.json" {
			m.Files[i].Size = int64(len(b))
			m.Files[i].SHA256 = strings.TrimPrefix(digest.Of(b).String(), "sha256:")
		}
	}
	f["extension.yaml"].Data, e = json.Marshal(m)
	mustCode(t, e, "")
	_, e = ReadPackage(f)
	mustCode(t, e, errcode.SchemaInvalid)
}

func TestRegistrationSurvivesDatabaseReopen(t *testing.T) {
	r, ctx := newRegistry(t)
	mustCode(t, r.RegisterBuiltins(ctx), "")
	p, e := r.Producer(ctx, "org.lantai.corecheck.manifest")
	mustCode(t, e, "")
	var seq int
	var name, path string
	if e = r.db.QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &path); e != nil {
		t.Fatal(e)
	}
	if e = r.db.Close(); e != nil {
		t.Fatal(e)
	}
	db, e := sqlite.Open(t.Context(), path, sqlite.Options{})
	mustCode(t, e, "")
	t.Cleanup(func() { db.Close() })
	next, e := New(Deps{db, commands.NewGate(commands.NewCoordinator()), r.release})
	mustCode(t, e, "")
	mustCode(t, next.VerifyProducer(t.Context(), p), "")
	list, e := next.List(t.Context())
	mustCode(t, e, "")
	if len(list) != 1 || list[0].PackageDigest != p.PackageDigest {
		t.Fatal("registration did not survive reopen", list)
	}
}

func TestRegistrationCannotSelfPromoteDatabaseRows(t *testing.T) {
	r, ctx := newRegistry(t)
	p := basePackage(t)
	p.Manifest.ID = "org.example.selfdeclared"
	p.Manifest.Contributes[0].ID = p.Manifest.ID + ".manifest"
	p.ManifestJSON, _ = canonjson.CanonicalizeValue(p.Manifest)
	p.ManifestDigest = digest.Of(p.ManifestJSON)
	p.Digest = digest.Of([]byte("external package"))
	// Simulate a forged database record, bypassing the private compiled registrar.
	_, e := r.db.Exec(`INSERT INTO extensions_registrations VALUES(?,?,?,?,?,'builtin_release')`, p.Manifest.ID, p.Manifest.Version, p.Digest, p.ManifestDigest, []byte(p.ManifestJSON))
	mustCode(t, e, "")
	_, e = r.db.Exec(`INSERT INTO extensions_release_bindings VALUES(?,?,?,?)`, p.Manifest.ID, p.Manifest.Version, r.release, p.Digest)
	mustCode(t, e, "")
	producer := storage.Producer{ExtensionID: p.Manifest.ID, ExtensionVersion: p.Manifest.Version, PackageDigest: p.Digest, CoreReleaseDigest: r.release, Source: "builtin_release", ContributionID: p.Manifest.Contributes[0].ID}
	mustCode(t, r.VerifyProducer(ctx, producer), errcode.ExtensionActivationStale)
	_, e = r.List(ctx)
	mustCode(t, e, errcode.ExtensionActivationStale)
}
