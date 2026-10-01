package integration

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/application"
	"github.com/oujinhaoai/lantai/internal/catalog"
	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

// 运行中的实例在后台清扫到期上传会话：关闭会话、删除暂存、释放 upload pin、
// 撤销未消费的复用授权；未到期的会话不动，已提交版本与内容库原件不受影响
// （BUG-20260930-03）。
func TestServeSweepsExpiredUploadsInBackground(t *testing.T) {
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	_, session := e.agent("sweep@node-a", identity.RoleContributor)
	who := session.Context
	ctx := t.Context()

	committed := []byte("committed content survives the sweep")
	v, err := e.catalog.CommitVersion(ctx, catalog.VersionRequest{Who: who, IdempotencyKey: e.key(), UploadID: e.upload(who, committed), Slug: "sweep/kept",
		Content: catalog.ContentInput{AssetType: manifest.TypeDoc, Rights: rightsOwned(), Files: []manifest.InputFile{{Path: "kept.txt", Role: "primary", SHA256: shaOf(committed), Size: int64(len(committed))}}}})
	if err != nil {
		t.Fatal(err)
	}
	// 放弃的会话一：只写了分片，文件未完成（暂存字节）。
	partial := []byte("abandoned staging bytes")
	pending, err := e.storage.CreateUpload(ctx, storage.CreateUploadRequest{Who: who, IdempotencyKey: e.key(), ProjectID: e.project.ProjectID, Files: []storage.FileSpec{{SHA256: shaOf(partial), Size: int64(len(partial))}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.storage.PutPart(ctx, storage.PartRequest{Who: who, UploadID: pending.UploadID, SHA256: shaOf(partial), PartNumber: 1, PartSHA256: shaOf(partial), Size: int64(len(partial)), Body: bytes.NewReader(partial)}); err != nil {
		t.Fatal(err)
	}
	// 放弃的会话二：文件已核验入库，签发了复用授权并由 upload pin 保留。
	verified := []byte("abandoned verified content")
	abandoned := e.upload(who, verified)

	a := reopenApplicationWith(t, e, application.Options{Storage: storage.Config{MinFreeBytes: 1 << 20}, UploadSweepInterval: 20 * time.Millisecond})
	stats := func() storage.Stats {
		t.Helper()
		s, err := a.Storage.Stats(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	time.Sleep(200 * time.Millisecond) // 多轮清扫：未到期的会话保持开放
	if s := stats(); s.OpenUploads != 2 || s.UploadStagingBytes != int64(len(partial)) || a.UploadSweep().Expired != 0 {
		t.Fatalf("unexpired uploads changed: %+v %+v", s, a.UploadSweep())
	}

	e.clk.Advance(25 * time.Hour) // 超过默认 24 小时空闲到期
	deadline := time.Now().Add(10 * time.Second)
	for s := stats(); s.OpenUploads != 0 || s.UploadStagingBytes != 0; s = stats() {
		if time.Now().After(deadline) {
			t.Fatalf("expired uploads were not swept: %+v %+v", s, a.UploadSweep())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if sw := a.UploadSweep(); sw.Expired != 2 || sw.Failures != 0 || sw.LastSuccess.IsZero() {
		t.Fatalf("sweep stats: %+v", sw)
	}
	for _, id := range []string{string(pending.UploadID), string(abandoned)} {
		var state string
		db := a.Instance.DB(ownership.Runtime)
		if err := db.QueryRowContext(ctx, `SELECT state FROM storage_uploads WHERE upload_id = ?`, id).Scan(&state); err != nil || state != "expired" {
			t.Fatalf("upload %s state %q %v", id, state, err)
		}
		var pins, grants int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM storage_pins WHERE owner_kind = 'upload_session' AND owner_id = ? AND released_at IS NULL`, id).Scan(&pins); err != nil || pins != 0 {
			t.Fatalf("upload %s keeps %d pins %v", id, pins, err)
		}
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM storage_blob_grants g JOIN storage_uploads u ON u.operation_id = g.operation_id WHERE u.upload_id = ? AND g.revoked_at IS NULL`, id).Scan(&grants); err != nil || grants != 0 {
			t.Fatalf("upload %s keeps %d grants %v", id, grants, err)
		}
		if _, err := os.Stat(filepath.Join(a.Instance.Layout().Home, "staging", "uploads", id)); !os.IsNotExist(err) {
			t.Fatalf("staging of %s not removed: %v", id, err)
		}
	}
	got, err := os.ReadFile(filepath.Join(a.Storage.Layout().VersionDir(v.ProjectID, v.AssetID, v.VersionNumber), storage.FilesDir, "kept.txt"))
	if err != nil || !bytes.Equal(got, committed) {
		t.Fatalf("committed version changed: %q %v", got, err)
	}
	if _, err := a.Ledger.Version(ctx, v.AssetID, v.VersionID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a.Storage.Layout().BlobPath(shaOf(verified))); err != nil {
		t.Fatalf("the sweep must not delete content-addressed originals: %v", err)
	}
}
