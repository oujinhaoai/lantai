package httpapi

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/oujinhaoai/lantai/internal/contract/errcode"
)

// Owners still perform every authorized read. Pagination is an explicit wire
// projection of that snapshot; it creates no stored cursor or second ledger.
func respondSnapshot[O any](w http.ResponseWriter, r *http.Request, items []O) error {
	if items == nil {
		items = []O{}
	}
	mode := r.URL.Query().Get("pagination")
	if mode == "" {
		return respond(w, http.StatusOK, map[string]any{"items": items})
	}
	if mode != "explicit" && mode != "chunk" {
		return invalid("pagination must be explicit or chunk")
	}
	offset := 0
	if raw := r.URL.Query().Get("offset"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return invalid("nonnegative snapshot offset required")
		}
		offset = n
	}
	expected := r.URL.Query().Get("expected_sha256")
	if offset > 0 && expected == "" {
		return invalid("snapshot continuation requires original expected_sha256")
	}
	if expected != "" {
		if raw, err := hex.DecodeString(expected); err != nil || len(raw) != sha256.Size {
			return invalid("snapshot digest must be SHA256 hex")
		}
	}
	raw, err := json.Marshal(map[string]any{"items": items})
	if err != nil {
		return err
	}
	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])
	if expected != "" && expected != digest {
		return errcode.New(errcode.PreconditionFailed, "snapshot changed; replace prior pages or chunks")
	}
	if mode == "chunk" {
		budget := 4096
		if value := r.URL.Query().Get("max_bytes"); value != "" {
			n, err := strconv.Atoi(value)
			if err != nil {
				return invalid("invalid chunk byte budget")
			}
			budget = n
		}
		if budget < 1 || budget > 8192 || offset > len(raw) {
			return invalid("snapshot chunk budget or offset invalid")
		}
		end := min(offset+budget, len(raw))
		page := map[string]any{"encoding": "base64", "data": base64.StdEncoding.EncodeToString(raw[offset:end]), "sha256": digest, "offset": offset, "total_bytes": len(raw), "complete": end == len(raw)}
		if end < len(raw) {
			page["next_offset"] = end
		}
		return respond(w, http.StatusOK, page)
	}
	n := 10
	if value := r.URL.Query().Get("limit"); value != "" {
		count, err := strconv.Atoi(value)
		if err != nil {
			return invalid("invalid snapshot page limit")
		}
		n = count
	}
	if n < 1 || n > 20 || offset > len(items) {
		return invalid("snapshot limit must be 1-20 and offset within items")
	}
	end := min(offset+n, len(items))
	for {
		page := map[string]any{"items": items[offset:end], "sha256": digest, "offset": offset, "total_items": len(items), "complete": end == len(items)}
		if end < len(items) {
			page["next_offset"] = end
		}
		encoded, err := json.Marshal(page)
		if err != nil {
			return err
		}
		// Leaves room for the complete MCP text+structured double serialization.
		if len(encoded) <= 16<<10 {
			return respond(w, http.StatusOK, page)
		}
		if end <= offset+1 {
			return errcode.New(errcode.QuotaExceeded, "snapshot item exceeds page budget; use pagination=chunk")
		}
		end--
	}
}
