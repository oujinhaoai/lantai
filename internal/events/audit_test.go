package events

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
)

func TestAuditRecoversFileBeforeCheckpointCrash(t *testing.T) {
	f := newFixture(t)
	f.collect(t, f.record(t, 1))
	dir := t.TempDir()
	if _, err := f.db.Exec(`CREATE TRIGGER audit_checkpoint_fail BEFORE UPDATE OF audit_through ON events_retention BEGIN SELECT RAISE(ABORT,'simulated crash before checkpoint'); END;`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.ExportAudit(f.ctx, dir, 10); err == nil {
		t.Fatal("wanted checkpoint failure")
	}
	m, err := f.store.Metrics(f.ctx)
	if err != nil || m.AuditThrough != 0 {
		t.Fatalf("failed checkpoint committed: %+v %v", m, err)
	}
	if _, err = f.db.Exec(`DROP TRIGGER audit_checkpoint_fail`); err != nil {
		t.Fatal(err)
	}
	f.store = New(f.db, f.clock, f.gate)
	manifest, err := f.store.ExportAudit(f.ctx, dir, 10)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Through != 1 || manifest.PhysicalRecords != 1 {
		t.Fatalf("file-first recovery duplicated logical event: %+v", manifest)
	}
	file, err := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if digest.Of(file) != manifest.Digest || int64(len(file)) != manifest.Size {
		t.Fatal("manifest does not describe durable bytes")
	}
	stored, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var decoded AuditManifest
	if err = json.Unmarshal(stored, &decoded); err != nil || decoded != manifest {
		t.Fatalf("manifest mismatch: %+v %v", decoded, err)
	}
}

func TestAuditPhysicalDuplicateAndTruncatedTailRecovery(t *testing.T) {
	f := newFixture(t)
	f.collect(t, f.record(t, 1), f.record(t, 2))
	dir := t.TempDir()
	if _, err := f.store.ExportAudit(f.ctx, dir, 1); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "events.jsonl")
	line, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	partial := append(append(append([]byte{}, line...), line...), []byte(`{"global_seq":2,"envelope":`)...)
	if err = os.WriteFile(path, partial, 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := f.store.ExportAudit(f.ctx, dir, 10)
	if err != nil {
		t.Fatal(err)
	}
	if m.Through != 2 || m.PhysicalRecords != 3 {
		t.Fatalf("recovered logical/physical counts: %+v", m)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Count(data, []byte{'\n'}) != 3 || bytes.Contains(data, []byte(`"envelope":{"global_seq"`)) {
		t.Fatal("partial tail was not replaced")
	}
	if m.Digest != digest.Of(data) {
		t.Fatal("bad digest after tail recovery")
	}
	if again, err := f.store.ExportAudit(f.ctx, dir, 10); err != nil || again != m {
		t.Fatalf("idempotent export: %+v %v", again, err)
	}
}

func TestAuditRejectsCorruptInterior(t *testing.T) {
	f := newFixture(t)
	f.collect(t, f.record(t, 1))
	dir := t.TempDir()
	if _, err := f.store.ExportAudit(f.ctx, dir, 1); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "events.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	corrupt := bytes.Replace(data, []byte(`"revision":1`), []byte(`"revision":2`), 1)
	if bytes.Equal(data, corrupt) {
		t.Fatal("fixture replacement failed")
	}
	if err = os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.ExportAudit(f.ctx, dir, 1); err == nil {
		t.Fatal("tampered audit accepted")
	}
}

func TestPruneRequiresAuditConsumerBackupAndHotWindow(t *testing.T) {
	f := newFixture(t)
	r1, r2 := f.record(t, 1), f.record(t, 2)
	f.collect(t, r1, r2)
	if err := f.store.RegisterConsumer(f.ctx, "critical", true); err != nil {
		t.Fatal(err)
	}
	if err := f.store.AcknowledgeConsumer(f.ctx, "critical", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.ExportAudit(f.ctx, t.TempDir(), 10); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(HotWindow + time.Second)
	if got, err := f.store.Prune(f.ctx); err != nil || got != 0 {
		t.Fatalf("missing backup did not protect history: %d %v", got, err)
	}
	if err := f.store.ConfirmBackup(f.ctx, 2); err != nil {
		t.Fatal(err)
	}
	if got, err := f.store.Prune(f.ctx); err != nil || got != 1 {
		t.Fatalf("consumer watermark did not cap pruning: %d %v", got, err)
	}
	if err := f.store.AcknowledgeConsumer(f.ctx, "critical", 2); err != nil {
		t.Fatal(err)
	}
	if got, err := f.store.Prune(f.ctx); err != nil || got != 2 {
		t.Fatalf("safe records retained: %d %v", got, err)
	}
	var expired *errcode.Error
	if _, err := f.store.Read(f.ctx, 0, 10); !errors.As(err, &expired) || expired.Code != errcode.CursorExpired {
		t.Fatalf("zero cursor silently bypassed expiration: %v", err)
	}
	if n, err := f.store.Collect(f.ctx, []commands.OutboxRecord{r1, r2}); err != nil || n != 0 {
		t.Fatalf("pruned duplicate resurrected: %d %v", n, err)
	}
	if high, err := f.store.HighWater(f.ctx); err != nil || high != 2 {
		t.Fatalf("watermark changed after prune: %d %v", high, err)
	}
	r3 := f.record(t, 3)
	if _, err := f.store.Collect(f.ctx, []commands.OutboxRecord{r3}); err != nil {
		t.Fatal(err)
	}
	p, err := f.store.Read(f.ctx, 2, 10)
	if err != nil || len(p.Entries) != 1 || p.HighWater != 3 {
		t.Fatalf("page after pruning: %+v %v", p, err)
	}
	if err = f.store.ConfirmBackup(f.ctx, 3); err != nil {
		t.Fatal(err)
	}
	if err = f.store.AcknowledgeConsumer(f.ctx, "critical", 3); err != nil {
		t.Fatal(err)
	}
	if got, err := f.store.Prune(f.ctx); err != nil || got != 2 {
		t.Fatalf("unexported/hot event pruned: %d %v", got, err)
	}
}

func TestPruneAuditWatermarkCannotBeBypassed(t *testing.T) {
	f := newFixture(t)
	f.collect(t, f.record(t, 1))
	if err := f.store.ConfirmBackup(f.ctx, 1); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(HotWindow + time.Second)
	if got, err := f.store.Prune(f.ctx); err != nil || got != 0 {
		t.Fatalf("unexported event pruned: %d %v", got, err)
	}
	dir := t.TempDir()
	if _, err := f.store.ExportAudit(f.ctx, dir, 10); err != nil {
		t.Fatal(err)
	}
	if got, err := f.store.Prune(f.ctx); err != nil || got != 1 {
		t.Fatalf("exported event not pruned: %d %v", got, err)
	}
	// 已裁剪历史的审计丢失无法从热库伪造恢复，必须明确失败并保留原水位。
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte(`{"global_seq":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.ExportAudit(f.ctx, dir, 10); err == nil {
		t.Fatal("lost pruned audit history silently accepted")
	}
}

func TestPruneKeepsRecentlyCollectedOldOccurrence(t *testing.T) {
	f := newFixture(t)
	r := f.record(t, 1)
	f.clock.Advance(HotWindow + time.Second)
	f.collect(t, r)
	if _, err := f.store.ExportAudit(f.ctx, t.TempDir(), 10); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ConfirmBackup(f.ctx, 1); err != nil {
		t.Fatal(err)
	}
	if got, err := f.store.Prune(f.ctx); err != nil || got != 0 {
		t.Fatalf("old occurrence lost its hot replay window: %d %v", got, err)
	}
}
