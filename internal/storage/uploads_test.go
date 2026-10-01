package storage

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"io/fs"
	"math"
	"os"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/pin"
	"github.com/oujinhaoai/lantai/internal/storage/fileop"
)

type readOnceFunc struct {
	r    io.Reader
	once func()
}

func (r *readOnceFunc) Read(p []byte) (int, error) {
	if r.once != nil {
		fn := r.once
		r.once = nil
		fn()
	}
	return r.r.Read(p)
}

// 网络读取结束前已经返回的撤权必须在分片的最终接受处生效。
func TestPartRechecksPermissionAfterReceivingBody(t *testing.T) {
	f := newFixture(t, testConfig(), nil)
	content := []byte("permission changes during a transfer")
	u := f.createUpload(f.who, f.project, content)
	_, err := f.svc.PutPart(t.Context(), PartRequest{Who: f.who, UploadID: u.UploadID,
		SHA256: shaOf(content), PartNumber: 1, PartSHA256: shaOf(content), Size: int64(len(content)),
		Body: &readOnceFunc{r: bytes.NewReader(content), once: func() { f.az.Revoke(f.who.PrincipalID, f.project) }}})
	wantCode(t, err, errcode.TokenRevoked)
	if count, err := f.svc.receivedCount(t.Context(), u.UploadID, shaOf(content)); err != nil || count != 0 {
		t.Fatalf("revoked transfer recorded a part: %d, %v", count, err)
	}
}

func TestCompleteFileCopyDoesNotBlockRevocation(t *testing.T) {
	faults := &fileop.Faults{}
	f := newFixture(t, testConfig(), faults)
	content := []byte("copy fallback cannot hold the security lock")
	u := f.createUpload(f.who, f.project, content)
	f.putAll(f.who, u, content)
	faults.Link = func(_, _ string) error {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		_, held, err := f.inst.Gate().Coordinator().Acquire(ctx, commands.Request{Security: commands.ModeExclusive})
		if err != nil {
			t.Errorf("content copying held the final acceptance lock: %v", err)
			return err
		}
		f.az.Revoke(f.who.PrincipalID, f.project)
		held.Release()
		return syscall.EXDEV
	}
	_, err := f.svc.CompleteFile(t.Context(), f.who, u.UploadID, shaOf(content))
	wantCode(t, err, errcode.TokenRevoked)
	_, err = f.svc.uploadedGrant(t.Context(), u.OperationID, shaOf(content))
	wantCode(t, err, errcode.BlobGrantRequired)
}

func TestUploadSizeArithmeticCannotOverflow(t *testing.T) {
	f := newFixture(t, Config{MaxFileBytes: math.MaxInt64, MaxUploadBytes: math.MaxInt64, PartSize: 2}, nil)
	_, _, err := f.svc.normalizeFiles([]FileSpec{
		{SHA256: shaOf([]byte("a")), Size: math.MaxInt64},
		{SHA256: shaOf([]byte("b")), Size: 1},
	})
	wantReason(t, err, errcode.QuotaExceeded, "upload_too_large")
	part, count := f.svc.partLayout(math.MaxInt64)
	if part != 2 || int64(count) != math.MaxInt64/2+1 {
		t.Fatalf("overflowed part layout: %d %d", part, count)
	}
}

func TestExpirySweepDoesNotCloseRenewedUpload(t *testing.T) {
	f := newFixture(t, testConfig(), nil)
	content := synthetic("sweep-renewal", 2*64<<10)
	u := f.createUpload(f.who, f.project, content)
	f.putPart(f.who, u.UploadID, content, u.Files[0], 1)
	stale, err := f.svc.loadUpload(t.Context(), f.svc.db, u.UploadID)
	if err != nil {
		t.Fatal(err)
	}
	// 模拟 sweep 取出到期候选后，另一已接受分片将会话续到新的空闲期限。
	f.clk.Advance(24 * time.Hour)
	if err := f.svc.inTx(t.Context(), func(tx *sql.Tx) error { return f.svc.touch(t.Context(), tx, u.UploadID, f.clk.Now()) }); err != nil {
		t.Fatal(err)
	}
	closed, err := f.svc.close(t.Context(), f.who, stale, UploadExpired, "expired")
	if err != nil || closed {
		t.Fatalf("renewed upload expired from stale sweep candidate: %v %v", closed, err)
	}
	if _, err := os.Stat(f.svc.layout.uploadData(u.UploadID, shaOf(content))); err != nil {
		t.Fatalf("renewed upload staging was removed: %v", err)
	}
}

// 暂存目录删不掉时，清扫保留已完成的关闭并报告失败，不能当作已回收；
// 恢复权限后下一轮删除残留。
func TestExpirySweepReportsStagingRemovalFailure(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("directory permission bits do not block removal on this platform or for root")
	}
	f := newFixture(t, testConfig(), nil)
	content := synthetic("sweep-remove-failure", 2*64<<10)
	u := f.createUpload(f.who, f.project, content)
	f.putPart(f.who, u.UploadID, content, u.Files[0], 1)
	dir := f.svc.layout.uploadDir(u.UploadID)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	f.clk.Advance(25 * time.Hour)
	rep, err := f.svc.SweepExpiredUploads(t.Context())
	if err == nil || rep.Expired != 1 || rep.StagingRemoved != 0 {
		t.Fatalf("removal failure hidden: %+v %v", rep, err)
	}
	if got, _ := f.svc.loadUpload(t.Context(), f.svc.db, u.UploadID); got.State != UploadExpired {
		t.Fatalf("the close must stand even when staging removal fails: %s", got.State)
	}
	if _, err := os.Stat(f.svc.layout.uploadData(u.UploadID, shaOf(content))); err != nil {
		t.Fatalf("staging should still be present: %v", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	rep, err = f.svc.SweepExpiredUploads(t.Context())
	if err != nil || rep.Expired != 0 || rep.StagingRemoved != 1 {
		t.Fatalf("next sweep: %+v %v", rep, err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("leftover staging not removed: %v", err)
	}
}

func wantCode(t *testing.T, err error, code errcode.Code) *errcode.Error {
	t.Helper()
	if err == nil {
		t.Fatalf("want %s, got success", code)
	}
	e, ok := errcode.As(err)
	if !ok || e.Code != code {
		t.Fatalf("want %s, got %v", code, err)
	}
	return e
}

func wantReason(t *testing.T, err error, code errcode.Code, reason string) {
	t.Helper()
	e := wantCode(t, err, code)
	for _, d := range e.Details {
		if d.Reason == reason {
			return
		}
	}
	t.Fatalf("want reason %s in %v", reason, e.Details)
}

// 中断后续传：分片记录持久化，重启后只补缺的分片，整件哈希与大小一致才入库。
func TestResumeAfterRestart(t *testing.T) {
	f := newFixture(t, testConfig(), nil)
	content := synthetic("resume", 3*64<<10+1234) // 4 个分片
	u := f.createUpload(f.who, f.project, content)
	uf := u.Files[0]
	if uf.PartCount != 4 || uf.PartSize != 64<<10 {
		t.Fatalf("layout = %+v", uf)
	}
	f.putPart(f.who, u.UploadID, content, uf, 1)
	f.putPart(f.who, u.UploadID, content, uf, 3)
	f.restart()
	got, err := f.svc.GetUpload(t.Context(), f.who, u.UploadID)
	if err != nil {
		t.Fatal(err)
	}
	if r := got.Files[0].Received; len(r) != 2 || r[0] != 1 || r[1] != 3 {
		t.Fatalf("received after restart = %v", r)
	}
	_, err = f.svc.CompleteFile(t.Context(), f.who, u.UploadID, uf.SHA256)
	e := wantCode(t, err, errcode.InvalidStateTransition)
	if missing, _ := e.Details[0].Data["missing_parts"].([]int); len(missing) != 2 || missing[0] != 2 || missing[1] != 4 {
		t.Fatalf("missing parts = %v", e.Details[0].Data)
	}
	f.putPart(f.who, u.UploadID, content, uf, 2)
	f.putPart(f.who, u.UploadID, content, uf, 4)
	res := f.putPart(f.who, u.UploadID, content, uf, 2) // 重复同内容
	if !res.Duplicate || res.Received != 4 {
		t.Fatalf("duplicate part = %+v", res)
	}
	g, err := f.svc.CompleteFile(t.Context(), f.who, u.UploadID, uf.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if g.Basis != BasisUploaded || g.OperationID != u.OperationID || g.SHA256 != uf.SHA256 || g.Size != int64(len(content)) {
		t.Fatalf("grant = %+v", g)
	}
	again, err := f.svc.CompleteFile(t.Context(), f.who, u.UploadID, uf.SHA256)
	if err != nil || again.GrantID != g.GrantID {
		t.Fatalf("completing twice must return the same grant: %+v %v", again, err)
	}
	stored, err := os.ReadFile(f.svc.layout.BlobPath(uf.SHA256))
	if err != nil || !bytes.Equal(stored, content) {
		t.Fatal("stored content differs from the upload")
	}
	if _, err := os.Stat(f.svc.layout.uploadData(u.UploadID, uf.SHA256)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("staging data should be removed after the content is stored")
	}
}

func TestPartIntegrity(t *testing.T) {
	f := newFixture(t, testConfig(), nil)
	content := synthetic("parts", 2*64<<10)
	u := f.createUpload(f.who, f.project, content)
	uf := u.Files[0]
	good := partBytes(content, uf, 1)
	bad := bytes.Clone(good)
	bad[0] ^= 0xff
	put := func(declared, body []byte, size int64) error {
		_, err := f.svc.PutPart(t.Context(), PartRequest{Who: f.who, UploadID: u.UploadID, SHA256: uf.SHA256, PartNumber: 1,
			PartSHA256: shaOf(declared), Size: size, Body: bytes.NewReader(body)})
		return err
	}
	// 摘要不符：拒绝且不记录。
	wantReason(t, put(good, bad, int64(len(good))), errcode.HashMismatch, "part_digest")
	// 长度与布局不符、实际字节不足：拒绝。
	wantReason(t, put(good, good, int64(len(good))-1), errcode.SchemaInvalid, "part_size")
	wantReason(t, put(good, good[:100], int64(len(good))), errcode.HashMismatch, "part_length")
	if got, _ := f.svc.GetUpload(t.Context(), f.who, u.UploadID); len(got.Files[0].Received) != 0 {
		t.Fatal("rejected parts must not be recorded")
	}
	if err := put(good, good, int64(len(good))); err != nil {
		t.Fatal(err)
	}
	// 已记录的分片换内容重试：拒绝，原分片不变。
	wantReason(t, put(bad, bad, int64(len(bad))), errcode.HashMismatch, "part_conflict")
	f.putPart(f.who, u.UploadID, content, uf, 2)
	if _, err := f.svc.CompleteFile(t.Context(), f.who, u.UploadID, uf.SHA256); err != nil {
		t.Fatalf("original part was damaged by the rejected retry: %v", err)
	}
}

// 申报的哈希与分片一致但整件不符（申报错误）：整件核验失败，内容不入库。
func TestWholeFileMismatchIsNotStored(t *testing.T) {
	f := newFixture(t, testConfig(), nil)
	content := synthetic("whole", 1000)
	wrong := FileSpec{SHA256: shaOf([]byte("something else")), Size: int64(len(content))}
	u, err := f.svc.CreateUpload(t.Context(), CreateUploadRequest{Who: f.who, IdempotencyKey: f.key(), ProjectID: f.project, Files: []FileSpec{wrong}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.PutPart(t.Context(), PartRequest{Who: f.who, UploadID: u.UploadID, SHA256: wrong.SHA256, PartNumber: 1,
		PartSHA256: shaOf(content), Size: int64(len(content)), Body: bytes.NewReader(content)}); err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.CompleteFile(t.Context(), f.who, u.UploadID, wrong.SHA256)
	wantReason(t, err, errcode.HashMismatch, "file_digest")
	if _, err := os.Stat(f.svc.layout.BlobPath(wrong.SHA256)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("unverified bytes entered the content store")
	}
	if _, err := os.Stat(f.svc.layout.BlobPath(shaOf(content))); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("bytes were stored under a hash nobody declared")
	}
	_, err = f.svc.CompleteFile(t.Context(), f.who, u.UploadID, wrong.SHA256)
	wantReason(t, err, errcode.HashMismatch, "file_failed")
}

// 相同字节去重：内容库只存一份，但每个上传者都必须完整上传并各自取得授权；
// 没有本操作授权的哈希一律“需要上传”，不泄露内容是否已存在。
func TestDedupDoesNotBypassAuthorization(t *testing.T) {
	f := newFixture(t, testConfig(), nil)
	content := synthetic("shared", 5000)
	a := f.uploadAll(f.who, f.project, content)
	other := ids.New()
	b := f.agent(other)
	ub := f.createUpload(b, other, content, []byte("never uploaded"))
	st, err := f.svc.CheckBlobs(t.Context(), b, ub.UploadID, []FileSpec{specOf(content), specOf([]byte("unknown"))})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range st {
		if s.Status != "upload_required" {
			t.Fatalf("status for content %s = %s; existence must not leak", s.SHA256[:8], s.Status)
		}
	}
	_, err = f.svc.CompleteFile(t.Context(), b, ub.UploadID, shaOf(content))
	wantReason(t, err, errcode.InvalidStateTransition, "parts_missing")
	f.putAll(b, ub, content)
	gb, err := f.svc.CompleteFile(t.Context(), b, ub.UploadID, shaOf(content))
	if err != nil {
		t.Fatal(err)
	}
	ga, _ := f.svc.uploadedGrant(t.Context(), a.OperationID, shaOf(content))
	if gb.GrantID == ga.GrantID || gb.OperationID != ub.OperationID || gb.ProjectID != other {
		t.Fatalf("grants must stay per operation: %+v vs %+v", gb, ga)
	}
	st, _ = f.svc.CheckBlobs(t.Context(), b, ub.UploadID, []FileSpec{specOf(content)})
	if st[0].Status != "granted" {
		t.Fatalf("after a verified upload: %s", st[0].Status)
	}
	// 别人的会话看不到这个上传。
	_, err = f.svc.GetUpload(t.Context(), f.who, ub.UploadID)
	wantCode(t, err, errcode.NotFound)
	_, err = f.svc.CheckBlobs(t.Context(), f.who, ub.UploadID, []FileSpec{specOf(content)})
	wantCode(t, err, errcode.NotFound)
}

func TestLimitsAndIdempotency(t *testing.T) {
	cfg := testConfig()
	cfg.MaxFileBytes, cfg.MaxUploadBytes, cfg.MaxUploadFiles = 1000, 1500, 2
	f := newFixture(t, cfg, nil)
	create := func(key string, specs ...FileSpec) (Upload, error) {
		return f.svc.CreateUpload(t.Context(), CreateUploadRequest{Who: f.who, IdempotencyKey: key, ProjectID: f.project, Files: specs})
	}
	big := FileSpec{SHA256: shaOf([]byte("big")), Size: 1001}
	_, err := create("a", big)
	wantReason(t, err, errcode.QuotaExceeded, "file_too_large")
	_, err = create("b", specOf(synthetic("1", 800)), specOf(synthetic("2", 800)))
	wantReason(t, err, errcode.QuotaExceeded, "upload_too_large")
	_, err = create("c", specOf([]byte("x")), specOf([]byte("y")), specOf([]byte("z")))
	wantReason(t, err, errcode.QuotaExceeded, "too_many_files")
	// 边界内与恰好在边界上的请求被接受；同一内容重复申报按哈希去重。
	exact := FileSpec{SHA256: shaOf(synthetic("e", 1000)), Size: 1000}
	u1, err := create("d", exact, exact)
	if err != nil || len(u1.Files) != 1 {
		t.Fatalf("at the limit: %+v %v", u1, err)
	}
	u2, err := create("d", exact)
	if err != nil || u2.UploadID != u1.UploadID || u2.OperationID != u1.OperationID {
		t.Fatalf("same key must return the same upload: %+v %v", u2, err)
	}
	// 重放不分配额外存储，后来空间不足也不应把原来的成功改为失败。
	f.svc.cfg.MinFreeBytes = math.MaxUint64
	u2, err = create("d", exact)
	if err != nil || u2.UploadID != u1.UploadID {
		t.Fatalf("replay after space runs low: %+v %v", u2, err)
	}
	_, err = create("d", specOf([]byte("other")))
	wantCode(t, err, errcode.IdempotencyConflict)
	_, err = create("e", FileSpec{SHA256: "ABC", Size: 1})
	wantCode(t, err, errcode.SchemaInvalid)
}

func TestUploadRequiresCurrentPermission(t *testing.T) {
	f := newFixture(t, testConfig(), nil)
	stranger := f.agent(ids.New())
	_, err := f.svc.CreateUpload(t.Context(), CreateUploadRequest{Who: stranger, IdempotencyKey: "k", ProjectID: f.project, Files: []FileSpec{specOf([]byte("x"))}})
	wantCode(t, err, errcode.Forbidden)
	content := synthetic("revoked", 2*64<<10)
	u := f.createUpload(f.who, f.project, content)
	f.putPart(f.who, u.UploadID, content, u.Files[0], 1)
	f.az.Revoke(f.who.PrincipalID, f.project)
	b := partBytes(content, u.Files[0], 2)
	_, err = f.svc.PutPart(t.Context(), PartRequest{Who: f.who, UploadID: u.UploadID, SHA256: u.Files[0].SHA256, PartNumber: 2,
		PartSHA256: shaOf(b), Size: int64(len(b)), Body: bytes.NewReader(b)})
	if err == nil {
		t.Fatal("part accepted after revocation")
	}
}

// 上传到期与提交 pin 分开：到期只清掉暂存与未消费的授权，已被操作消费的授权
// 与内容库原件不受影响。
func TestExpiryIsSeparateFromCommitPins(t *testing.T) {
	f := newFixture(t, testConfig(), nil)
	used := synthetic("used", 1000)
	spare := synthetic("spare", 1000)
	partial := synthetic("partial", 2*64<<10)
	u := f.uploadAll(f.who, f.project, used, spare)
	manifestFiles := files(map[string][]byte{"a.bin": used})
	p, proof := f.prepareInstall(f.who, u, f.project, "", "exp/asset", "", manifestFiles)

	half := f.createUpload(f.who, f.project, partial)
	f.putPart(f.who, half.UploadID, partial, half.Files[0], 1)

	pins, err := f.svc.PinsFor(t.Context(), shaOf(used))
	if err != nil || len(pins) != 1 || !pins[0].ActiveAt(f.clk.Now()) || pins[0].Kind != pin.KindUpload {
		t.Fatalf("upload pin while open: %+v %v", pins, err)
	}
	f.clk.Advance(25 * time.Hour) // 超过空闲到期；原会话也已过期，换同一主体的新会话
	fresh := f.az.OpenSession(f.who.PrincipalID, 12*time.Hour)
	b := partBytes(partial, half.Files[0], 2)
	_, err = f.svc.PutPart(t.Context(), PartRequest{Who: fresh, UploadID: half.UploadID, SHA256: half.Files[0].SHA256, PartNumber: 2,
		PartSHA256: shaOf(b), Size: int64(len(b)), Body: bytes.NewReader(b)})
	wantReason(t, err, errcode.InvalidStateTransition, "upload_expired")

	rep, err := f.svc.SweepExpiredUploads(t.Context())
	if err != nil || rep.Expired != 2 {
		t.Fatalf("sweep = %+v %v", rep, err)
	}
	if _, err := os.Stat(f.svc.layout.uploadDir(half.UploadID)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("expired staging data was kept")
	}
	pins, _ = f.svc.PinsFor(t.Context(), shaOf(used))
	if held, _ := pin.Held(t.Context(), shaOf(used), f.clk.Now(), f.svc); held || len(pins) != 1 || pins[0].ReleasedAt.IsZero() {
		t.Fatalf("upload pin must be released at expiry: %+v", pins)
	}
	for _, c := range [][]byte{used, spare} {
		if _, err := os.Stat(f.svc.layout.BlobPath(shaOf(c))); err != nil {
			t.Fatal("expiry must not delete stored content; GC decides with all pin sources")
		}
	}
	// 已被 prepared 操作消费的授权仍然有效：提交照常完成。
	if _, err := f.ledger.Commit(t.Context(), p.OperationID, fresh, proof); err != nil {
		t.Fatalf("commit after upload expiry: %v", err)
	}
	// 未消费的授权随到期撤销：同一操作无法再用 spare。
	ok, _ := f.svc.hasGrant(t.Context(), f.svc.db, f.project, u.OperationID, shaOf(spare), int64(len(spare)), f.clk.Now())
	if ok {
		t.Fatal("an unconsumed grant survived upload expiry")
	}
}

func TestStorageFullIsReported(t *testing.T) {
	var full bool
	faults := &fileop.Faults{Write: func(string) error {
		if full {
			return syscall.ENOSPC
		}
		return nil
	}}
	f := newFixture(t, testConfig(), faults)
	content := synthetic("full", 1000)
	u := f.createUpload(f.who, f.project, content)
	full = true
	_, err := f.svc.PutPart(t.Context(), PartRequest{Who: f.who, UploadID: u.UploadID, SHA256: shaOf(content), PartNumber: 1,
		PartSHA256: shaOf(content), Size: int64(len(content)), Body: bytes.NewReader(content)})
	wantCode(t, err, errcode.StorageFull)
	full = false
	f.putAll(f.who, u, content)
	if _, err := f.svc.CompleteFile(t.Context(), f.who, u.UploadID, shaOf(content)); err != nil {
		t.Fatalf("after space is freed: %v", err)
	}
}

// 流式上传：内存分配不随文件大小线性增长。
func TestStreamingMemoryIsBounded(t *testing.T) {
	size := 64 << 20
	if testing.Short() {
		size = 16 << 20
	}
	cfg := testConfig()
	cfg.PartSize, cfg.SinglePartMax = 8<<20, 8<<20
	f := newFixture(t, cfg, nil)
	content := synthetic("stream", size)
	u := f.createUpload(f.who, f.project, content)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	f.putAll(f.who, u, content)
	if _, err := f.svc.CompleteFile(t.Context(), f.who, u.UploadID, shaOf(content)); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc
	t.Logf("uploaded %d MiB, allocated %.1f MiB", size>>20, float64(allocated)/(1<<20))
	if allocated > uint64(size)/4 {
		t.Fatalf("allocated %d bytes to stream %d bytes; memory must not grow with file size", allocated, size)
	}
}

// 同一内容的并发完成请求都返回同一授权，不会把已入库的内容误判为失败。
func TestConcurrentCompleteFile(t *testing.T) {
	f := newFixture(t, testConfig(), nil)
	content := synthetic("concurrent", 3*64<<10)
	u := f.createUpload(f.who, f.project, content)
	f.putAll(f.who, u, content)
	type result struct {
		g   BlobGrant
		err error
	}
	out := make(chan result, 4)
	for range 4 {
		go func() {
			g, err := f.svc.CompleteFile(context.Background(), f.who, u.UploadID, shaOf(content))
			out <- result{g, err}
		}()
	}
	var first ids.ID
	for range 4 {
		r := <-out
		if r.err != nil {
			t.Fatalf("concurrent completion: %v", r.err)
		}
		if first == "" {
			first = r.g.GrantID
		} else if r.g.GrantID != first {
			t.Fatal("concurrent completions returned different grants")
		}
	}
	got, _ := f.svc.GetUpload(t.Context(), f.who, u.UploadID)
	if got.Files[0].State != FileVerified || len(got.Files[0].Received) != 3 {
		t.Fatalf("file after concurrent completion = %+v", got.Files[0])
	}
}
