package httpapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/oujinhaoai/lantai/internal/contract/errcode"
)

func TestSnapshotPageLegacyBoundAndChangedContinuation(t *testing.T) {
	items := []map[string]string{{"id": "one", "payload": strings.Repeat("x", 9000)}, {"id": "two", "payload": strings.Repeat("y", 9000)}}
	call := func(query string) (map[string]any, error) {
		t.Helper()
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/snapshot?"+query, nil)
		err := respondSnapshot(w, r, items)
		if err != nil {
			return nil, err
		}
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out, nil
	}
	legacy, err := call("")
	if err != nil || len(legacy) != 1 || len(legacy["items"].([]any)) != 2 {
		t.Fatal("legacy shape changed", legacy, err)
	}
	first, err := call("pagination=explicit&limit=2")
	if err != nil || first["complete"] != false || first["next_offset"] != float64(1) {
		t.Fatal(first, err)
	}
	digest := first["sha256"].(string)
	second, err := call("pagination=explicit&offset=1&limit=2&expected_sha256=" + digest)
	if err != nil || second["complete"] != true || len(second["items"].([]any)) != 1 {
		t.Fatal(second, err)
	}
	items[0]["id"] = "changed"
	if _, err := call("pagination=explicit&offset=1&expected_sha256=" + digest); errcode.CodeOf(err) != errcode.PreconditionFailed {
		t.Fatal("mixed snapshots accepted", err)
	}
	for _, q := range []string{"pagination=invalid", "pagination=explicit&limit=21", "pagination=explicit&offset=1", "pagination=chunk&max_bytes=8193", "pagination=chunk&offset=-1"} {
		if _, err := call(q); errcode.CodeOf(err) != errcode.SchemaInvalid {
			t.Fatal("invalid snapshot query accepted", q, err)
		}
	}
}

func TestSnapshotChunksExceedClientWholeResponseBudget(t *testing.T) {
	items := []map[string]string{{"payload": strings.Repeat("x", 9<<20)}}
	raw, err := json.Marshal(map[string]any{"items": items})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])
	w := httptest.NewRecorder()
	err = respondSnapshot(w, httptest.NewRequest("GET", "/snapshot?pagination=explicit", nil), items)
	if errcode.CodeOf(err) != errcode.QuotaExceeded || w.Body.Len() != 0 {
		t.Fatal("oversized item silently truncated", err, w.Body.Len())
	}
	for _, offset := range []int{0, len(raw) - 31} {
		q := url.Values{"pagination": {"chunk"}, "offset": {strconv.Itoa(offset)}, "max_bytes": {"8192"}, "expected_sha256": {digest}}
		w := httptest.NewRecorder()
		if err := respondSnapshot(w, httptest.NewRequest("GET", "/snapshot?"+q.Encode(), nil), items); err != nil {
			t.Fatal(err)
		}
		if w.Body.Len() > 16<<10 {
			t.Fatal("unbounded HTTP chunk", w.Body.Len())
		}
		var p struct {
			Data     string `json:"data"`
			SHA256   string `json:"sha256"`
			Total    int    `json:"total_bytes"`
			Complete bool   `json:"complete"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		data, err := base64.StdEncoding.DecodeString(p.Data)
		if err != nil || !bytes.Equal(data, raw[offset:min(offset+8192, len(raw))]) || p.SHA256 != digest || p.Total != len(raw) || p.Complete != (offset > 0) {
			t.Fatal("snapshot chunks lost exact byte evidence", len(data), err)
		}
	}
}
