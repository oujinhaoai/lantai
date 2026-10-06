package integration

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/provenance"
	"github.com/oujinhaoai/lantai/internal/query"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

// Removing a member from personal.readers while it keeps its project role hides
// personal objects it may already have cached. The incremental event page has no
// event for those objects, so it must tell the client to replace its visible
// scope (BUG-20261001-04). Role changes do the same; unrelated logins do not.
func TestM2PartialRevocationRequiresScopeReplacement(t *testing.T) {
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	ctx := t.Context()
	e.setRole(e.admin.Context.PrincipalID, identity.RoleOwner, true)
	who := e.login().Context
	agent, session := e.agent("personal-reader@node", identity.RoleContributor)
	readers, _ := json.Marshal([]ids.ID{who.PrincipalID, agent.ID})
	e.sudo(&identity.SetPolicy{ProjectID: e.project.ProjectID, Key: "personal.readers", ExpectedRevision: 0, Value: readers})
	v := e.ingest(who, "personal-notes", []byte("synthetic personal notes"), *rightsOwned())
	evidence := rightsEvidence(t, e, who, v, false)
	personal := "personal"
	if _, err := e.rights.ApplyAssertion(ctx, who, e.key(), provenance.AssertionRequest{Subject: v.Ref, ManifestDigest: v.ManifestDigest, ExpectedRevision: 1, Kind: "restrict", Fields: provenance.AssertionFields{Sensitivity: &personal}, EvidenceIDs: []ids.ID{evidence.RecordID}, Reason: "synthetic privacy restriction"}); err != nil {
		t.Fatal(err)
	}
	log, c := collaborationForTest(t, e, nil, nil, nil)
	e.relay(log)
	visible := func() []ids.ID {
		t.Helper()
		out, cursor := []ids.ID{}, ""
		for {
			page, err := c.Resync(ctx, session.Context, e.project.ProjectID, cursor, 100)
			if err != nil {
				t.Fatal(err)
			}
			for _, o := range page.Objects {
				out = append(out, o.Ref.ID)
			}
			if page.NextCursor == "" {
				return out
			}
			cursor = page.NextCursor
		}
	}
	if !slices.Contains(visible(), v.VersionID) {
		t.Fatal("a personal reader must see the personal version before revocation")
	}
	page, err := c.Events(ctx, query.EventRequest{Who: session.Context, ProjectID: e.project.ProjectID, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	after := page.LastSeq

	// Remove the agent from personal.readers only; it stays a contributor.
	readers, _ = json.Marshal([]ids.ID{who.PrincipalID})
	e.sudo(&identity.SetPolicy{ProjectID: e.project.ProjectID, Key: "personal.readers", ExpectedRevision: 1, Value: readers})
	e.relay(log)
	page, err = c.Events(ctx, query.EventRequest{Who: session.Context, ProjectID: e.project.ProjectID, After: after, Limit: 1000})
	if err != nil || page.LastSeq <= after {
		t.Fatal(page, err)
	}
	if !page.ReplaceRequired {
		t.Fatal("a partial revocation must require replacing the cached scope", page)
	}
	for _, ev := range page.Events {
		if ev.Object.Ref.ID == v.VersionID || ev.Object.Ref.ID == v.AssetID {
			t.Fatal("revoked personal object returned", ev)
		}
	}
	if slices.Contains(visible(), v.VersionID) {
		t.Fatal("the replacement scope still holds the personal version")
	}
	after = page.LastSeq

	// An unrelated login is not a scope change.
	e.login()
	e.relay(log)
	page, err = c.Events(ctx, query.EventRequest{Who: session.Context, ProjectID: e.project.ProjectID, After: after, Limit: 1000})
	if err != nil || page.ReplaceRequired {
		t.Fatal("an unrelated login forced a scope replacement", page, err)
	}
	after = page.LastSeq

	// Granting a role may reveal older objects that no incremental event carries.
	e.setRole(agent.ID, identity.RoleReviewer, true)
	e.relay(log)
	page, err = c.Events(ctx, query.EventRequest{Who: session.Context, ProjectID: e.project.ProjectID, After: after, Limit: 1000})
	if err != nil || !page.ReplaceRequired {
		t.Fatal("a role change must require replacing the visible scope", page, err)
	}
}
