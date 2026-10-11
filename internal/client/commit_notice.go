package client

import (
	"encoding/json"

	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

// CommitNotice explains a committed version with unfinished initial metadata.
// It is presentation only: the original receipt and recovery state stay intact.
func CommitNotice(response Response) string {
	var receipt struct {
		AssetID            ids.ID       `json:"asset_id"`
		VersionID          ids.ID       `json:"version_id"`
		CommittedAt        string       `json:"committed_at"`
		DescriptionPending bool         `json:"description_pending"`
		DescriptionError   errcode.Code `json:"description_error"`
	}
	if json.Unmarshal(response.Body, &receipt) != nil || !receipt.DescriptionPending ||
		!receipt.AssetID.Valid() || !receipt.VersionID.Valid() || receipt.CommittedAt == "" {
		return ""
	}
	notice := "Files are committed; initial metadata is pending."
	if _, known := errcode.Lookup(receipt.DescriptionError); known {
		notice += " Metadata error: " + string(receipt.DescriptionError) + "."
	}
	notice += " Read or download the returned exact asset_id/version_id. Do not start another upload to recover this committed version; keep the original input, state and idempotency keys."
	if receipt.DescriptionError == errcode.Forbidden {
		notice += " Initial metadata requires an authorized role and organize scope; do not expand permissions automatically."
	}
	return notice
}
