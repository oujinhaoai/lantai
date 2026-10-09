package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/schema"
	"github.com/oujinhaoai/lantai/internal/extensions"
	"github.com/oujinhaoai/lantai/internal/extensions/exttest"
	"github.com/oujinhaoai/lantai/internal/identity"
)

func TestM2GovernanceProbeCompleteRawProtocol(t *testing.T) {
	f := newAppFlow(t)
	ctx := t.Context()
	m := f.app.ExtensionManager
	builtin, err := f.app.Extensions.Producer(ctx, "org.lantai.corecheck.manifest")
	if err != nil {
		t.Fatal(err)
	}
	base := f.approvedProfile("profiles/bootstrap", profileDocument("bootstrap", builtin), ids.PermanentRef{})
	for i, mode := range []string{"probe_checks", "probe_files", "probe_unknown_field", "probe_unknown_producer_field"} {
		t.Run(mode, func(t *testing.T) {
			pkg, _ := f.approvedSpec(base, "plugins/"+mode, exttest.Spec{ID: "org.example." + strings.ReplaceAll(mode, "_", ""), Server: true})
			saved := filepath.Join(t.TempDir(), "actual-probe-result.json")
			cfg, _ := json.Marshal(map[string]any{"mode": mode, "heartbeat": saved})
			req := extensions.EnableRequest{ExtensionID: pkg.Manifest.ID, ExtensionVersion: pkg.Manifest.Version, PackageDigest: pkg.Digest, Target: "server", ScopeKind: "instance", Config: cfg, ConfigRevision: 1, Trust: extensions.TrustUnenforced, Probe: true, Reason: "raw protocol regression"}
			a, err := m.EnableHumanAction(ctx, req)
			if err != nil {
				t.Fatal(err)
			}
			who, g, op := f.grant(a)
			got, enableErr := m.Enable(ctx, who, req, g, op, f.id)
			if strings.HasPrefix(mode, "probe_unknown_") {
				raw, err := os.ReadFile(saved)
				if err != nil {
					t.Fatal(err)
				}
				reg, err := schema.Default()
				if err != nil {
					t.Fatal(err)
				}
				if err = reg.ValidateJSON("lantai.processor-result/v1", raw); err == nil {
					t.Fatal("fixture did not violate authoritative raw schema")
				}
				t.Log("authoritative raw schema rejects actual plugin result", err)
				captureGovernanceEvidence(t, "actual-invalid-probe-result.json", raw)
			}
			captureGovernanceEvidence(t, "observed-enable-result.json", []byte(rawJSON(got)))
			if enableErr == nil || got.Activation == nil || got.Activation.State == "ready" {
				t.Errorf("raw invalid/impure probe acquired ready: mode=%s error=%v activation=%+v", mode, enableErr, got.Activation)
			}
			f.sudo(&identity.SetPolicy{ProjectID: f.project.ProjectID, Key: "plugins.allowed", Value: json.RawMessage(`["` + pkg.Manifest.ID + `"]`), ExpectedRevision: int64(i)})
			if _, _, err = m.Snapshot(ctx, f.project.ProjectID, pkg.Manifest.ID+".check", 1); err == nil {
				t.Error("raw invalid probe acquired dispatch qualification")
			}
		})
	}
}
