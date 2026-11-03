package client

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

func TestRecoveryEvidenceRechecksPurposeAndEpochAfterDownload(t *testing.T) {
	for _, mode := range []string{"current", "purpose_revoked_after_download", "grant_changed_after_download", "epoch_changed"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newWorksetFixture(t)
			entry := fixture.entry("selected", map[string]string{"source.bin": "selected synthetic recovery bytes"})
			run := string(ids.New())
			backup := string(ids.New())
			var reads atomic.Int32
			if mode == "purpose_revoked_after_download" {
				fixture.onTransfer = func() { fixture.deny = true }
			}
			if mode == "grant_changed_after_download" {
				fixture.onTransfer = func() {
					fixture.changeGrant = func(grant *DownloadGrant) { grant.SHA256 = sha("different synthetic bytes") }
				}
			}
			fixture.c = newTestClient(t, func(w http.ResponseWriter, request *http.Request) {
				if request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/read-grants") {
					response := httptest.NewRecorder()
					fixture.serve(response, request)
					if response.Code != http.StatusOK {
						w.WriteHeader(response.Code)
						_, _ = w.Write(response.Body.Bytes())
						return
					}
					var grant map[string]any
					if err := json.Unmarshal(response.Body.Bytes(), &grant); err != nil {
						t.Error(err)
						return
					}
					grant["asset_id"], grant["version_id"], grant["method"], grant["expires_at"] = entry.AssetID, entry.VersionID, "GET", time.Now().Add(time.Minute).UTC()
					writeJSON(w, grant)
					return
				}
				if request.URL.Path != "/api/v1/admin/instance" {
					fixture.serve(w, request)
					return
				}
				if request.Method != http.MethodGet || request.Header.Get("Authorization") != "Bearer private-session" {
					t.Error("instance read changed admission")
				}
				epoch := int64(1)
				if reads.Add(1) > 1 && mode == "epoch_changed" {
					epoch = 2
				}
				writeJSON(w, map[string]any{"instance_id": fixture.instance, "state": "ready", "recovery_epoch": epoch, "observed_at": time.Now().UTC(), "recovery_receipts": []any{map[string]any{"run_id": run, "backup_id": backup, "manifest_digest": digest.Of([]byte("manifest")), "evidence_digest": digest.Of([]byte("evidence")), "recovery_epoch": 1, "acceptance_basis": "legacy_reconciliation", "unknown_after_capture": false}}})
			})
			prepared, err := fixture.c.PrepareRecoveryEvidence(t.Context(), RecoveryEvidenceSpec{ProjectID: string(ids.New()), Title: "Synthetic recovery observation", RunID: run, Purpose: "production", Workset: WorksetInput{Schema: WorksetSchema, Entries: []WorksetEntry{entry}}}, worksetTemp(t))
			if mode != "current" {
				if err == nil {
					t.Fatal("changed current source authority or epoch accepted", mode)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var evidence RecoveryEvidence
			if err := json.Unmarshal(prepared.Spec.Payload, &evidence); err != nil || len(evidence.Verifications) != 1 || evidence.NativeSoftwareContinuationVerified || evidence.SourceRetirementAuthorized {
				t.Fatal("selected observation differs", evidence, err)
			}
		})
	}
}
