package provenance

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

func TestVersionRightsViewUsesCurrentAuthorityAndAcceptedEvidence(t *testing.T) {
	f := setup(t)
	v := f.add(manifest.Rights{})
	before := string(f.files.manifests[v.VersionID])
	view, err := f.s.VersionRights(t.Context(), f.who, v.Ref(f.instance))
	if err != nil || view.Revision != 1 || len(view.Purposes) != 5 || len(view.Evidence) != 0 {
		t.Fatalf("initial view: %+v %v", view, err)
	}
	evidence, err := f.s.AppendEvidence(t.Context(), f.req(v))
	if err != nil {
		t.Fatal(err)
	}
	f.az.Grant(f.who.PrincipalID, f.project, "provenance.restrict")
	f.who = f.az.OpenSession(f.who.PrincipalID, time.Hour)
	yes := true
	receipt, err := f.s.ApplyAssertion(t.Context(), f.who, "synthetic-view-restriction", AssertionRequest{Subject: v.Ref(f.instance), ManifestDigest: v.ManifestDigest, ExpectedRevision: 1, Kind: "restrict", Fields: AssertionFields{NoAI: &yes}, EvidenceIDs: []ids.ID{evidence.RecordID}, Reason: "synthetic no-AI restriction"})
	if err != nil {
		t.Fatal(err)
	}
	view, err = f.s.VersionRights(t.Context(), f.who, v.Ref(f.instance))
	if err != nil || !view.Rights.NoAI || view.Revision != receipt.Revision || view.LastAssertionID != receipt.RecordID || len(view.Evidence) != 1 || view.Evidence[0].RecordID != evidence.RecordID {
		t.Fatalf("restricted view: %+v %v", view, err)
	}
	for _, p := range view.Purposes {
		if p.Purpose == authz.PurposeGenerativeInput && (p.Allowed || p.Code != errcode.UseRestricted) {
			t.Fatal(p)
		}
	}
	if string(f.files.manifests[v.VersionID]) != before {
		t.Fatal("view or assertion changed frozen manifest")
	}
	raw, _ := json.Marshal(view)
	for _, forbidden := range []string{"source_url", "grant_id", "human_grant", "session", "cookie"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("view exposes %s", forbidden)
		}
	}
	f.files.records[evidence.RecordID] = []byte("corrupt accepted evidence")
	if _, err = f.s.VersionRights(t.Context(), f.who, v.Ref(f.instance)); err == nil {
		t.Fatal("corrupt accepted evidence produced a rights view")
	}
}

func TestVersionRightsViewHidesPersonalAndRevokedTargets(t *testing.T) {
	f := setup(t)
	v := f.add(manifest.Rights{Usage: "reference", License: "LicenseRef-Synthetic", Sensitivity: "personal"})
	_, err := f.s.VersionRights(t.Context(), f.who, v.Ref(f.instance))
	code(t, err, errcode.NotFound)
	f.az.Grant(f.who.PrincipalID, f.project, ActionPersonalRead)
	f.who = f.az.OpenSession(f.who.PrincipalID, time.Hour)
	if _, err = f.s.VersionRights(t.Context(), f.who, v.Ref(f.instance)); err != nil {
		t.Fatal(err)
	}
	f.az.Revoke(f.who.PrincipalID, f.project)
	if _, err = f.s.VersionRights(t.Context(), f.who, v.Ref(f.instance)); err == nil {
		t.Fatal("revoked session retained view")
	}
}
