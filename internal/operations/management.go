package operations

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

// ManagementInfo is an explicitly public-to-administrators projection. Never
// add host paths, keys, arbitrary diagnostic messages or operator notes here.
type ManagementInfo struct {
	InstanceID       ids.ID                    `json:"instance_id"`
	RecoveryEpoch    int64                     `json:"recovery_epoch"`
	State            State                     `json:"state"`
	ObservedAt       time.Time                 `json:"observed_at"`
	FreeBytes        *uint64                   `json:"free_bytes"`
	MinFreeBytes     uint64                    `json:"min_free_bytes"`
	CapacityError    string                    `json:"capacity_error,omitempty"`
	RecoveryReceipts []CompletedRestoreReceipt `json:"recovery_receipts"`
	RecoveryError    string                    `json:"recovery_error,omitempty"`
	RecoveryPartial  bool                      `json:"recovery_partial"`
}

// CompletedRestoreReceipt proves a previous local acceptance, not perpetual
// availability, source isolation, or knowledge of events after capture.
type CompletedRestoreReceipt struct {
	RunID               ids.ID        `json:"run_id"`
	BackupID            ids.ID        `json:"backup_id"`
	ManifestDigest      digest.Digest `json:"manifest_digest"`
	RecoveryEpoch       int64         `json:"recovery_epoch"`
	EvidenceDigest      digest.Digest `json:"evidence_digest"`
	AcceptanceBasis     string        `json:"acceptance_basis"`
	UnknownAfterCapture bool          `json:"unknown_after_capture"`
}

// ManagementInfo reads only the current instance's fixed roots. It cannot be
// supplied a path by an HTTP caller and never changes the instance or receipts.
func (i *Instance) ManagementInfo(ctx context.Context) (ManagementInfo, error) {
	if err := ctx.Err(); err != nil {
		return ManagementInfo{}, err
	}
	m := i.Marker()
	out := ManagementInfo{InstanceID: m.InstanceID, RecoveryEpoch: m.RecoveryEpoch, State: i.State(), ObservedAt: i.clock.Now(), MinFreeBytes: i.cfg.MinFreeBytes, RecoveryReceipts: []CompletedRestoreReceipt{}}
	if free, err := freeBytes(i.layout); err == nil {
		out.FreeBytes = &free
	} else {
		out.CapacityError = "capacity_unavailable"
	}
	root, err := os.OpenRoot(i.layout.Home)
	if err != nil {
		out.RecoveryError = "recovery_receipts_unavailable"
		return out, nil
	}
	defer root.Close()
	dirInfo, err := root.Lstat("logs")
	if errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	if err != nil || !dirInfo.IsDir() {
		out.RecoveryError = "recovery_receipts_unavailable"
		return out, nil
	}
	directory, err := root.Open("logs")
	if err != nil {
		out.RecoveryError = "recovery_receipts_unavailable"
		return out, nil
	}
	entries, err := directory.ReadDir(10001)
	_ = directory.Close()
	if err != nil && !errors.Is(err, io.EOF) {
		out.RecoveryError = "recovery_receipts_unavailable"
		return out, nil
	}
	// Bound enumeration and parse work; never silently call a partial list full.
	if len(entries) > 10000 {
		out.RecoveryError = "recovery_receipt_budget_exceeded"
		out.RecoveryPartial = true
		return out, nil
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return ManagementInfo{}, err
		}
		name := entry.Name()
		if !strings.HasPrefix(name, "restore-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		id := ids.ID(strings.TrimSuffix(strings.TrimPrefix(name, "restore-"), ".json"))
		if !id.Valid() {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil || !info.Mode().IsRegular() || info.Size() > 64<<10 {
			out.RecoveryError = "recovery_receipt_invalid"
			continue
		}
		f, openErr := root.Open("logs/" + name)
		if openErr != nil {
			out.RecoveryError = "recovery_receipt_invalid"
			continue
		}
		pinnedInfo, pinnedErr := f.Stat()
		if pinnedErr != nil || !pinnedInfo.Mode().IsRegular() || !os.SameFile(info, pinnedInfo) || pinnedInfo.Size() > 64<<10 {
			_ = f.Close()
			out.RecoveryError = "recovery_receipt_invalid"
			continue
		}
		var review RecoveryReview
		d := json.NewDecoder(io.LimitReader(f, (64<<10)+1))
		d.DisallowUnknownFields()
		decodeErr := d.Decode(&review)
		if decodeErr == nil {
			var extra any
			if e := d.Decode(&extra); e != io.EOF {
				decodeErr = errors.New("trailing receipt")
			}
		}
		_ = f.Close()
		point := review.Contract == "lantai.recovery-review/v2" && review.AcceptRecoveryPoint && review.UnknownAfterCapture && !review.RevocationsReconciled && !review.DeletionsReconciled && !review.OpenOperationsReconciled
		legacy := review.Contract == "lantai.recovery-review/v1" && review.RevocationsReconciled && review.DeletionsReconciled && review.OpenOperationsReconciled && !review.AcceptRecoveryPoint && !review.UnknownAfterCapture
		if decodeErr != nil || review.RunID != id || !review.BackupID.Valid() || !review.ManifestDigest.Valid() || !review.EvidenceDigest.Valid() || review.Administrator == "" || len(review.Note) < 8 || len(review.Note) > 2000 || review.RecoveryEpoch < 1 || review.RecoveryEpoch > m.RecoveryEpoch || (!point && !legacy) || (m.Restore != nil && m.Restore.RunID == id) {
			out.RecoveryError = "recovery_receipt_invalid"
			continue
		}
		basis := "legacy_reconciliation"
		if point {
			basis = "accepted_capture_point"
		}
		out.RecoveryReceipts = append(out.RecoveryReceipts, CompletedRestoreReceipt{RunID: review.RunID, BackupID: review.BackupID, ManifestDigest: review.ManifestDigest, RecoveryEpoch: review.RecoveryEpoch, EvidenceDigest: review.EvidenceDigest, AcceptanceBasis: basis, UnknownAfterCapture: review.UnknownAfterCapture})
	}
	sort.Slice(out.RecoveryReceipts, func(a, b int) bool { return out.RecoveryReceipts[a].RunID > out.RecoveryReceipts[b].RunID })
	if len(out.RecoveryReceipts) > 100 {
		out.RecoveryReceipts = out.RecoveryReceipts[:100]
		out.RecoveryPartial = true
	}
	return out, nil
}
