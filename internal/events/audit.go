package events

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/event"
	"github.com/oujinhaoai/lantai/internal/platform/fsutil"
)

type auditLine struct {
	GlobalSeq  int64           `json:"global_seq"`
	RecordedAt string          `json:"recorded_at"`
	Envelope   json.RawMessage `json:"envelope"`
}

// AuditManifest 是文件内容摘要与逻辑水位清单。PhysicalRecords 可大于唯一事件数。
type AuditManifest struct {
	Name            string        `json:"name"`
	Size            int64         `json:"size"`
	Digest          digest.Digest `json:"digest"`
	Through         int64         `json:"through_seq"`
	PhysicalRecords int64         `json:"physical_records"`
}

// ExportAudit 恢复并追加一批审计记录。顺序为文件 fsync→摘要清单原子安装→DB 水位。
// 崩溃后按 event_id 逻辑去重，截断未完成末行；中间行损坏或历史丢失明确报错。
// 进程内续写只核对整文件摘要，进程首次导出或文件被改动时逐行完整核验。
// 一个实例只配置一个审计目录；单实例 Gate 与 auditMu 协调本进程写入。
func (s *Store) ExportAudit(ctx context.Context, dir string, limit int) (AuditManifest, error) {
	if limit < 1 || limit > 1000 {
		return AuditManifest{}, errors.New("events: invalid audit batch size")
	}
	ctx, h, err := acquire(ctx, s.gate)
	if err != nil {
		return AuditManifest{}, err
	}
	defer h.Release()
	s.auditMu.Lock()
	defer s.auditMu.Unlock()
	if err = fsutil.EnsurePrivateDir(dir); err != nil {
		return AuditManifest{}, err
	}
	path := filepath.Join(dir, "events.jsonl")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return AuditManifest{}, err
	}
	defer f.Close()
	m, validBytes, err := s.verifiedPrefix(ctx, f, path)
	if err != nil {
		return AuditManifest{}, err
	}
	// 修改文件前作废缓存；只有本次导出完整成功才记录新的已核验状态。
	s.audit = verifiedAudit{}
	var pruned int64
	if err = s.db.QueryRowContext(ctx, `SELECT pruned_through FROM events_retention WHERE singleton=1`).Scan(&pruned); err != nil {
		return m, err
	}
	if m.Through < pruned {
		return m, errors.New("events: audit history is missing beyond retained recovery data")
	}
	if err = f.Truncate(validBytes); err != nil {
		return m, err
	}
	if _, err = f.Seek(validBytes, io.SeekStart); err != nil {
		return m, err
	}
	page, err := s.Read(ctx, m.Through, limit)
	if err != nil {
		return m, err
	}
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	for _, e := range page.Entries {
		if err = enc.Encode(auditLine{e.GlobalSeq, clock.Format(e.RecordedAt), e.Canonical}); err != nil {
			return m, err
		}
		m.Through = e.GlobalSeq
		m.PhysicalRecords++
	}
	if err = f.Sync(); err != nil {
		return m, err
	}
	if err = fsutil.SyncDir(dir); err != nil {
		return m, err
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return m, err
	}
	m.Digest, m.Size, err = hashReader(f)
	if err != nil {
		return m, err
	}
	m.Name = "events.jsonl"
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return m, err
	}
	if err = fsutil.WriteFileAtomic(filepath.Join(dir, "manifest.json"), append(b, '\n'), 0o600); err != nil {
		return m, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return m, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO events_audit_files(name,size,digest,through_seq,records) VALUES(?,?,?,?,?) ON CONFLICT(name) DO UPDATE SET size=excluded.size,digest=excluded.digest,through_seq=excluded.through_seq,records=excluded.records`, m.Name, m.Size, m.Digest, m.Through, m.PhysicalRecords); err != nil {
		return m, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE events_retention SET audit_through=? WHERE singleton=1`, m.Through); err != nil {
		return m, err
	}
	if err = tx.Commit(); err != nil {
		return m, err
	}
	s.audit = verifiedAudit{path: path, manifest: m}
	return m, nil
}

// verifiedPrefix 返回可续写的已核验前缀。本进程上次成功导出后文件摘要未变
// 时，只重算整文件摘要（顺序读取，不逐行解析、不逐行查库），续写代价不再随
// 历史增长；否则逐行完整核验，截断未完成末行、拒绝中间损坏与序号缺口。
func (s *Store) verifiedPrefix(ctx context.Context, f *os.File, path string) (AuditManifest, int64, error) {
	if c := s.audit; c.path == path {
		if st, err := f.Stat(); err == nil && st.Size() == c.manifest.Size {
			d, size, err := hashReader(f)
			if err != nil {
				return AuditManifest{}, 0, err
			}
			if d == c.manifest.Digest && size == c.manifest.Size {
				return c.manifest, size, nil
			}
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return AuditManifest{}, 0, err
		}
	}
	s.auditScans++
	return s.scanAudit(ctx, f)
}

func (s *Store) scanAudit(ctx context.Context, r io.Reader) (AuditManifest, int64, error) {
	m := AuditManifest{Name: "events.jsonl"}
	var valid int64
	reader := bufio.NewReader(r)
	for {
		line, err := reader.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			return m, valid, nil
		} // 未完成末行包括合法 JSON 但缺换行，统一重放。
		if err != nil {
			return m, valid, err
		}
		var rec auditLine
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.DisallowUnknownFields()
		if err = dec.Decode(&rec); err != nil {
			return m, valid, fmt.Errorf("events: corrupt audit record: %w", err)
		}
		var extra any
		if err = dec.Decode(&extra); !errors.Is(err, io.EOF) {
			return m, valid, errors.New("events: trailing data in audit record")
		}
		e, err := event.Parse(rec.Envelope)
		if err != nil {
			return m, valid, err
		}
		canonical, err := e.Canonical()
		if err != nil {
			return m, valid, err
		}
		var seq, at int64
		var d string
		if err = s.db.QueryRowContext(ctx, `SELECT global_seq,envelope_digest,recorded_at FROM events_identities WHERE event_id=?`, e.EventID).Scan(&seq, &d, &at); err != nil {
			return m, valid, err
		}
		if seq != rec.GlobalSeq || string(digest.Of(canonical)) != d || rec.RecordedAt != clock.Format(clock.FromMillis(at)) {
			return m, valid, errors.New("events: audit record does not match collected identity")
		}
		// 已核对永久身份表；此前只允许连续推进，因此 <=Through 的序号必为
		// 已见事件的物理重复，无需保存随历史增长的内存去重集合。
		if seq == m.Through+1 {
			m.Through = seq
		} else if seq > m.Through {
			return m, valid, errors.New("events: audit sequence contains a gap")
		}
		m.PhysicalRecords++
		valid += int64(len(line))
	}
}

func hashReader(r io.Reader) (digest.Digest, int64, error) {
	h := digest.NewHasher()
	_, err := io.Copy(h, r)
	return h.Digest(), h.Size(), err
}
