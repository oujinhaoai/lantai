package storage

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/storage/fileop"
)

func TestMaintenanceRejectsPrivateStorageWrites(t *testing.T) {
	t.Run("upload", func(t *testing.T) {
		f := newFixture(t, testConfig(), nil)
		data := []byte("private bytes")
		u := f.createUpload(f.who, f.project, data)
		f.inst.Gate().Close(commands.ReasonMaintenance)
		read := false
		_, err := f.svc.PutPart(t.Context(), PartRequest{Who: f.who, UploadID: u.UploadID, SHA256: shaOf(data), PartNumber: 1, PartSHA256: shaOf(data), Size: int64(len(data)), Body: &readOnceFunc{r: bytes.NewReader(data), once: func() { read = true }}})
		wantCode(t, err, errcode.MaintenanceMode)
		if read {
			t.Fatal("read network bytes while writes were closed")
		}
		if _, err := os.Stat(f.svc.layout.uploadData(u.UploadID, shaOf(data))); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("staged data during maintenance: %v", err)
		}
	})
	t.Run("installation", func(t *testing.T) {
		f, req := installFixture(t, nil)
		f.inst.Gate().Close(commands.ReasonMaintenance)
		_, err := f.svc.Install(t.Context(), req)
		wantCode(t, err, errcode.MaintenanceMode)
		if _, err := os.Stat(f.svc.layout.installStaging(req.OperationID)); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("assembled during maintenance: %v", err)
		}
	})
}

func TestMaintenanceWaitsForPrivateStorageWrites(t *testing.T) {
	// 在真正写字节的回调中尝试维护，必须等到这次写入退出屏障；安全撤权不受阻。
	assertBarrier := func(t *testing.T, f *fixture) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
		_, held, err := f.inst.Gate().Maintain(ctx, commands.ReasonMaintenance)
		cancel()
		if err == nil {
			held.Release()
			t.Fatal("maintenance passed an active private writer")
		}
	}
	t.Run("upload", func(t *testing.T) {
		f := newFixture(t, testConfig(), nil)
		data := []byte("in flight")
		u := f.createUpload(f.who, f.project, data)
		_, err := f.svc.PutPart(t.Context(), PartRequest{Who: f.who, UploadID: u.UploadID, SHA256: shaOf(data), PartNumber: 1, PartSHA256: shaOf(data), Size: int64(len(data)), Body: &readOnceFunc{r: bytes.NewReader(data), once: func() { assertBarrier(t, f) }}})
		wantCode(t, err, errcode.MaintenanceMode)
	})
	t.Run("installation", func(t *testing.T) {
		faults := &fileop.Faults{}
		f, req := installFixture(t, faults)
		checked := false
		faults.Write = func(string) error {
			if !checked {
				checked = true
				assertBarrier(t, f)
			}
			return nil
		}
		if _, err := f.svc.Install(t.Context(), req); err != nil {
			t.Fatal(err)
		}
		if !checked {
			t.Fatal("install never wrote its private area")
		}
	})
}
