package client

import (
	"strings"
	"testing"
)

func TestCommitNotice(t *testing.T) {
	base := `"asset_id":"01ARZ3NDEKTSV4RRFFQ69G5FAV","version_id":"01ARZ3NDEKTSV4RRFFQ69G5FAW","committed_at":"2026-10-11T12:17:15.335Z"`
	for _, tc := range []struct {
		name, body string
		want       bool
	}{
		{"pending_forbidden", `{` + base + `,"description_pending":true,"description_error":"FORBIDDEN"}`, true},
		{"pending_other", `{` + base + `,"description_pending":true,"description_error":"MAINTENANCE"}`, true},
		{"complete", `{` + base + `}`, false},
		{"not_committed", `{"description_pending":true,"description_error":"FORBIDDEN"}`, false},
		{"invalid", `{`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			notice := CommitNotice(Response{Body: []byte(tc.body)})
			if (notice != "") != tc.want {
				t.Fatalf("notice=%q", notice)
			}
			if tc.want && (!strings.Contains(notice, "Files are committed") || !strings.Contains(notice, "exact asset_id/version_id") || !strings.Contains(notice, "Do not start another upload")) {
				t.Fatalf("missing recovery guidance: %s", notice)
			}
			if tc.name == "pending_forbidden" && !strings.Contains(notice, "organize scope") {
				t.Fatal("missing permission boundary")
			}
		})
	}
	// Never echo arbitrary server-provided text from an unregistered error code.
	if notice := CommitNotice(Response{Body: []byte(`{` + base + `,"description_pending":true,"description_error":"secret-canary"}`)}); strings.Contains(notice, "secret-canary") {
		t.Fatal("untrusted error text was echoed")
	}
}
