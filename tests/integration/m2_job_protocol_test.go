package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/contract/execution"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	"github.com/oujinhaoai/lantai/internal/contract/schema"
	"github.com/oujinhaoai/lantai/internal/identity"
)

func TestM2GovernanceJobRejectsRawForbiddenFields(t *testing.T) {
	f := newAppFlow(t)
	ctx := t.Context()
	builtin, err := f.app.Extensions.Producer(ctx, "org.lantai.corecheck.manifest")
	if err != nil {
		t.Fatal(err)
	}
	base := f.approvedProfile("profiles/bootstrap", profileDocument("bootstrap", builtin), ids.PermanentRef{})
	pkg, _ := f.approvedPackage(base, "plugins/raw-result", "0.1.0")
	f.sudo(&identity.SetPolicy{ProjectID: f.project.ProjectID, Key: "plugins.allowed", Value: json.RawMessage(`["org.example.business"]`)})
	reg, err := schema.Default()
	if err != nil {
		t.Fatal(err)
	}
	for i, mode := range []string{"unknown_field", "unknown_producer_field", "unknown_check_field"} {
		t.Run(mode, func(t *testing.T) {
			saved := filepath.Join(t.TempDir(), "actual-job-result.json")
			cfg, _ := json.Marshal(map[string]any{"mode": mode, "heartbeat": saved})
			f.enablePackage(pkg, string(cfg), int64(i+1))
			v, flow := f.candidate("docs/raw-"+mode, manifest.TypeDoc, map[string][]byte{"data.json": []byte(`{"valid":true}`)}, nil)
			j := f.checkCandidate(v, flow, pkg.Manifest.ID+".check")
			raw, err := os.ReadFile(saved)
			if err != nil {
				t.Fatal(err)
			}
			if err = reg.ValidateJSON("lantai.processor-result/v1", raw); err == nil {
				t.Fatal("fixture did not violate raw schema")
			}
			if j.State != "failed" || j.Failure != "runtime_fault" || j.Attempt.Outcome != execution.InvocationRuntimeFault || len(j.Evidence) != 0 {
				t.Fatal("forbidden wire field produced accepted evidence", j)
			}
			var faults int
			if err = f.inst.DB(ownership.Runtime).QueryRowContext(ctx, `SELECT count(*) FROM extensions_breaker_faults WHERE invocation_id=?`, j.Attempt.ID).Scan(&faults); err != nil || faults != 1 {
				t.Fatal("malformed result fault accounting", faults, err)
			}
			captureGovernanceEvidence(t, "actual-invalid-job-result.json", raw)
			captureGovernanceEvidence(t, "rejected-job.json", []byte(rawJSON(j)))
		})
	}
}
