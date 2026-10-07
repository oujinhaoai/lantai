//go:build windows

package events

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"golang.org/x/sys/windows"
	"os"
	"path/filepath"
	"testing"
)

func TestAuditWindowsOccupiedManifestRecovery(t *testing.T) {
	f := newFixture(t)
	f.collect(t, f.record(t, 1))
	dir := t.TempDir()
	first, err := f.store.ExportAudit(f.ctx, dir, 10)
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(dir, "manifest.json")
	old, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	wide, err := windows.UTF16PtrFromString(name)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(wide, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	held := true
	defer func() {
		if held {
			windows.CloseHandle(handle)
		}
	}()
	f.collect(t, f.record(t, 2))
	_, err = f.store.ExportAudit(f.ctx, dir, 10)
	if !errors.Is(err, windows.ERROR_SHARING_VIOLATION) && !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Fatal("native replacement did not fail transparently", err)
	}
	before, err := f.store.Metrics(f.ctx)
	if err != nil || before.AuditThrough != 1 {
		t.Fatal("failed replacement advanced watermark", before, err)
	}
	actual, err := os.ReadFile(name)
	if err != nil || !bytes.Equal(actual, old) {
		t.Fatal("failed replacement changed prior manifest", err)
	}
	temporary, err := filepath.Glob(filepath.Join(dir, ".manifest.json.tmp-*"))
	if err != nil || len(temporary) != 0 {
		t.Fatal("failed replacement left temp file", temporary, err)
	}
	windows.CloseHandle(handle)
	held = false
	f.store = New(f.db, f.clock, f.gate)
	recovered, err := f.store.ExportAudit(f.ctx, dir, 10)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Through != 2 || recovered.PhysicalRecords != 2 || first.Through != 1 {
		t.Fatal("recovery lost or duplicated event", recovered)
	}
	body, err := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	if err != nil || digest.Of(body) != recovered.Digest || int64(len(body)) != recovered.Size {
		t.Fatal("recovered byte digest", err)
	}
	raw, err := os.ReadFile(name)
	var decoded AuditManifest
	if err != nil || json.Unmarshal(raw, &decoded) != nil || decoded != recovered {
		t.Fatal("recovered manifest", err)
	}
	after, err := f.store.Metrics(f.ctx)
	if err != nil || after.AuditThrough != 2 {
		t.Fatal("recovered watermark", after, err)
	}
	again, err := f.store.ExportAudit(f.ctx, dir, 10)
	if err != nil || again != recovered {
		t.Fatal("replay changed recovered facts", again, err)
	}
	t.Log("native occupied replacement returned error, old manifest/watermark retained, temp removed; released cold-owner retry has 2 unique/2 physical records and verified bytes")
}
