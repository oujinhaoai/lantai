package storage

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

func TestEvidenceRecordsAreAppendOnlyAndBound(t *testing.T) {
	f := newFixture(t, testConfig(), nil)
	c := f.commitVersion(f.who, f.project, "", "records/asset", "", map[string][]byte{"a.txt": []byte("a")})
	rec := Record{
		RecordID: ids.New(), ProjectID: c.ProjectID, AssetID: c.AssetID, VersionID: c.VersionID, ManifestDigest: c.ManifestDigest,
		Kind: "check_result", PayloadSchema: "lantai.synthetic-check/v1", Payload: json.RawMessage(`{"result":"pass","check":"decode"}`),
		Producer: &Producer{ExtensionID: "org.lantai.builtin", ExtensionVersion: "0.1.0",
			CoreReleaseDigest: digest.Of([]byte("core-release")), PackageDigest: digest.Of([]byte("core-release")), Source: "builtin_release", ContributionID: "org.lantai.builtin.probe"},
		AuthorID: f.who.PrincipalID, SessionID: f.who.SessionID, CreatedAt: clock.Format(f.clk.Now()),
	}
	ref, err := f.svc.AppendRecord(t.Context(), rec)
	if err != nil || !ref.Created {
		t.Fatalf("append: %+v %v", ref, err)
	}
	again, err := f.svc.AppendRecord(t.Context(), rec)
	if err != nil || again.Created || again.Digest != ref.Digest {
		t.Fatalf("replay: %+v %v", again, err)
	}
	changed := rec
	changed.Payload = json.RawMessage(`{"result":"fail","check":"decode"}`)
	_, err = f.svc.AppendRecord(t.Context(), changed)
	wantCode(t, err, errcode.IdempotencyConflict)
	raw, err := f.svc.ReadRecord(t.Context(), c.ProjectID, c.AssetID, c.VersionID, rec.RecordID)
	if err != nil || digest.Of(raw) != ref.Digest {
		t.Fatalf("read back: %v", err)
	}

	// 更正追加新记录并指向旧记录；旧记录保持不变。
	correction := rec
	correction.RecordID, correction.Kind, correction.Supersedes = ids.New(), "correction", rec.RecordID
	correction.Payload = json.RawMessage(`{"note":"re-run with the fixed decoder"}`)
	if _, err := f.svc.AppendRecord(t.Context(), correction); err != nil {
		t.Fatal(err)
	}
	dangling := correction
	dangling.RecordID, dangling.Supersedes = ids.New(), ids.New()
	_, err = f.svc.AppendRecord(t.Context(), dangling)
	wantReason(t, err, errcode.SchemaInvalid, "supersedes_unknown")

	// 旧检查不能用于新清单；未提交或不存在的版本不能追加记录。
	stale := rec
	stale.RecordID, stale.ManifestDigest = ids.New(), digest.Of([]byte("another manifest"))
	_, err = f.svc.AppendRecord(t.Context(), stale)
	wantReason(t, err, errcode.PreconditionFailed, "manifest_digest_mismatch")
	orphan := rec
	orphan.RecordID, orphan.VersionID = ids.New(), ids.New()
	_, err = f.svc.AppendRecord(t.Context(), orphan)
	wantCode(t, err, errcode.NotFound)
	// 调用方只按所给项目授权：版本属于别的项目时与不存在一样，不泄露存在性。
	foreign := rec
	foreign.RecordID, foreign.ProjectID = ids.New(), ids.New()
	_, err = f.svc.AppendRecord(t.Context(), foreign)
	wantCode(t, err, errcode.NotFound)
	_, err = f.svc.ReadRecord(t.Context(), ids.New(), c.AssetID, c.VersionID, rec.RecordID)
	wantCode(t, err, errcode.NotFound)

	// 生产者身份必须完整（扩展 ID、版本与包摘要）。
	anonymous := rec
	anonymous.RecordID = ids.New()
	anonymous.Producer = &Producer{ExtensionID: "org.lantai.builtin", Source: "builtin_release"}
	_, err = f.svc.AppendRecord(t.Context(), anonymous)
	wantCode(t, err, errcode.SchemaInvalid)

	// 先于 encoding/json 拒绝非法 UTF-8，不能让替换字符折叠掉原始内容差异。
	malformed := rec
	malformed.RecordID = ids.New()
	malformed.Producer = &Producer{ExtensionID: "org.lantai.builtin", ExtensionVersion: "version-\xff",
		CoreReleaseDigest: digest.Of([]byte("core-release")), PackageDigest: digest.Of([]byte("core-release")), Source: "builtin_release"}
	_, err = f.svc.AppendRecord(t.Context(), malformed)
	wantCode(t, err, errcode.SchemaInvalid)
}

func TestLegacyProducerEvidencePreservesBytesAndDigest(t *testing.T) {
	f := newFixture(t, testConfig(), nil)
	c := f.commitVersion(f.who, f.project, "", "records/legacy", "", map[string][]byte{"a.txt": []byte("legacy")})
	rec := Record{RecordID: ids.New(), ProjectID: c.ProjectID, AssetID: c.AssetID, VersionID: c.VersionID, ManifestDigest: c.ManifestDigest,
		Kind: "check_result", Payload: json.RawMessage(`{"result":"pass"}`), AuthorID: f.who.PrincipalID, CreatedAt: clock.Format(f.clk.Now()),
		Producer: &Producer{ExtensionID: "org.lantai.legacy", ExtensionVersion: "0.1.0", PackageDigest: digest.Of([]byte("legacy package")), Source: "builtin_release"}}
	before, err := rec.canonical()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(before, []byte("core_release_digest")) {
		t.Fatal("legacy source was silently upgraded")
	}
	// Storage is only the append-only byte adapter. New domain acceptance must
	// separately call the trusted registry and reject an unbound producer.
	written, err := f.svc.AppendRecord(t.Context(), rec)
	if err != nil {
		t.Fatal(err)
	}
	after, err := f.svc.ReadRecord(t.Context(), c.ProjectID, c.AssetID, c.VersionID, rec.RecordID)
	if err != nil || !bytes.Equal(before, after) || digest.Of(before) != written.Digest {
		t.Fatalf("historical record changed: %v", err)
	}
	var roundtrip Record
	if err = json.Unmarshal(after, &roundtrip); err != nil {
		t.Fatal(err)
	}
	again, err := roundtrip.canonical()
	if err != nil || !bytes.Equal(before, again) {
		t.Fatalf("new Go field changed old canonical representation: %v", err)
	}
}
