package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"testing"

	"github.com/oujinhaoai/lantai/internal/application"
	"github.com/oujinhaoai/lantai/internal/catalog"
	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/event"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/operations"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
	"github.com/oujinhaoai/lantai/internal/transport/httpapi"
)

func TestPostCommitCheckEnrollmentAndReplay(t *testing.T) {
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	ctx := t.Context()
	e.setRole(e.admin.Context.PrincipalID, identity.RoleOwner, true)
	owner := e.login().Context
	current := e.project
	// This commit is left unrelayed while disabled. Opening the enabled app
	// relays it late, but lack of ledger enrollment must prevent a task.
	old, err := e.commitDoc(owner, "check/old", "", "", []byte("old while off"), nil)
	if err != nil {
		t.Fatal(err)
	}
	home := e.inst.Layout().Home
	rule := commit.PostCommitCheckRule{Generation: ids.New(), ProjectID: current.ProjectID, ActorID: owner.PrincipalID}
	writeRule := func() {
		t.Helper()
		b := fmt.Sprintf("contract: lantai.config/v1\nstorage:\n  min_free_bytes: 1048576\npost_commit_check:\n  generation: %s\n  project_id: %s\n  actor_id: %s\n", rule.Generation, rule.ProjectID, rule.ActorID)
		if err := os.WriteFile(e.inst.Layout().ConfigPath(), []byte(b), 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeRule()
	a := reopenApplication(t, e, storage.Config{MinFreeBytes: 1 << 20})
	list := func(want ...ids.ID) {
		t.Helper()
		records, err := a.Tasks.List(ctx, owner, current.ProjectID, "", 100, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(records) != len(want) {
			t.Fatalf("tasks got %d, want %d: %+v", len(records), len(want), records)
		}
		expected := map[ids.ID]bool{}
		for _, id := range want {
			expected[id] = true
		}
		for _, r := range records {
			if r.Type != "question" || len(r.Input.Refs) != 1 || r.Input.Refs[0].InstanceID != a.Instance.InstanceID() || !expected[r.Input.Refs[0].VersionID] || len(r.OutputRefs) != 0 || r.FlowID != "" || r.StepRunID != "" {
				t.Fatalf("expected a standalone exact human checklist: %+v", r)
			}
			delete(expected, r.Input.Refs[0].VersionID)
		}
	}
	push := func(slug string) catalog.VersionResult {
		t.Helper()
		r, err := e.commitDoc(owner, slug, "", "", []byte(slug), nil)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	other, err := e.catalog.CreateProject(ctx, catalog.ProjectRequest{Who: owner, IdempotencyKey: e.key(), Key: "excluded", Name: "Excluded"})
	if err != nil {
		t.Fatal(err)
	}
	e.project = other
	e.setRole(owner.PrincipalID, identity.RoleOwner, true)
	// Refresh owner after the membership change, retaining the authorized actor.
	owner = e.login().Context
	excluded := push("check/excluded")
	e.project = current
	one := push("check/once")
	afterCrash := push("check/pending")
	stale := push("check/prior-generation")
	for _, r := range []catalog.VersionResult{old, excluded, {ProjectID: current.ProjectID, VersionID: "bad"}} {
		if err := a.Flows.PostCommitChecklist(ctx, owner, r.ProjectID, r.VersionID); err != nil {
			t.Fatal(err)
		}
	}
	list()
	if err := a.Flows.PostCommitChecklist(ctx, owner, current.ProjectID, one.VersionID); err != nil {
		t.Fatal(err)
	}
	list(one.VersionID)
	// Record a second representation of the actual enrollment, plus a forged
	// enrollment for a version committed while OFF, in a separate source.
	v, _ := a.Ledger.VersionByID(ctx, one.VersionID)
	enrolled, found, err := a.Ledger.PostCommitCheckEnrollment(ctx, v)
	if err != nil || !found {
		t.Fatalf("missing enrollment: %v", err)
	}
	duplicate := enrolled
	duplicate.EventID = ids.New()
	invalid := enrolled
	invalid.EventID = ids.New()
	invalid.AggregateID = old.VersionID
	invalid.OperationID = old.OperationID
	bogus := commit.PostCommitCheckEnrollment{Rule: rule, AssetID: old.AssetID, VersionID: old.VersionID, ManifestDigest: old.ManifestDigest}
	invalid.Payload, _ = json.Marshal(bogus)
	if err = a.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	var records []commands.OutboxRecord
	for _, variant := range []event.Envelope{duplicate, invalid} {
		raw, err := variant.Canonical()
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, commands.OutboxRecord{EventID: variant.EventID, OperationID: variant.OperationID, Envelope: raw, OccurredAt: variant.OccurredAt})
	}
	if _, err = a.Events.Collect(ctx, records); err != nil {
		t.Fatal(err)
	}
	if _, err = a.Flows.CatchUp(ctx, 1000); err != nil {
		t.Fatal(err)
	}
	list(one.VersionID)
	// Simulate interruption with durable pending intents, after one target was
	// accepted but before either representation acquired a dispatch receipt.
	if err = a.Close(ctx); err != nil {
		t.Fatal(err)
	}
	a, err = application.Open(ctx, application.Options{Instance: operations.Options{Home: home, Clock: e.clk}, Identity: identity.Config{Password: fastPassword}, Storage: storage.Config{MinFreeBytes: 1 << 20}})
	if err != nil {
		t.Fatal(err)
	}
	newApp := a
	t.Cleanup(func() { _ = newApp.Close(context.Background()) })
	e.inst, e.id, e.ledger, e.storage, e.catalog = a.Instance, a.Identity, a.Ledger, a.Storage, a.Catalog
	owner = e.login().Context
	// Corrupted/foreign context must not consume the current actor's queue.
	wrong := owner
	wrong.InstanceID = ids.New()
	if _, err = a.Flows.Dispatch(ctx, wrong, 100); err != nil {
		t.Fatal(err)
	}
	list(one.VersionID)
	if _, err = a.Flows.Dispatch(ctx, owner, 100); err != nil {
		t.Fatal(err)
	}
	list(one.VersionID, afterCrash.VersionID, stale.VersionID)
	if _, err = a.Flows.Dispatch(ctx, owner, 100); err != nil {
		t.Fatal(err)
	}
	list(one.VersionID, afterCrash.VersionID, stale.VersionID)
	for _, id := range []ids.ID{one.VersionID, afterCrash.VersionID, stale.VersionID} {
		c, err := a.Ledger.VersionControl(ctx, id)
		if err != nil || c.ReviewState != "draft" {
			t.Fatalf("task modified review or publication state: %+v, %v", c, err)
		}
	}
}

func TestPostCommitCheckHTTPAutoCreate(t *testing.T) {
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	ctx := t.Context()
	e.setRole(e.admin.Context.PrincipalID, identity.RoleOwner, true)
	owner := e.login()
	rule := fmt.Sprintf("contract: lantai.config/v1\nstorage:\n  min_free_bytes: 1048576\npost_commit_check:\n  generation: %s\n  project_id: %s\n  actor_id: %s\n", ids.New(), e.project.ProjectID, owner.Context.PrincipalID)
	if err := os.WriteFile(e.inst.Layout().ConfigPath(), []byte(rule), 0600); err != nil {
		t.Fatal(err)
	}
	a := reopenApplication(t, e, storage.Config{MinFreeBytes: 1 << 20})
	owner = e.login()
	server := remoteServers(t, a, true)
	c, err := client.New(client.Config{BaseURL: "http://" + server.Addresses.API, AllowHTTP: true, SessionToken: owner.Token})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	data := []byte("synthetic HTTP enrollment")
	upload := e.upload(owner.Context, data)
	request := httpapi.CommitRequest{Slug: "check/http-commit", Content: catalog.ContentInput{AssetType: manifest.TypeDoc, Rights: rightsOwned(), Files: []manifest.InputFile{{Path: "content.txt", Role: "primary", SHA256: shaOf(data), Size: int64(len(data))}}}}
	key := e.key()
	var version ids.ID
	for range 2 {
		r, err := c.Do(ctx, http.MethodPost, "/api/v1/uploads/"+string(upload)+"/commit", request, client.Options{IdempotencyKey: key})
		if err != nil {
			t.Fatal(err)
		}
		var committed catalog.VersionResult
		if err = r.Decode(&committed); err != nil {
			t.Fatal(err)
		}
		if version != "" && committed.VersionID != version {
			t.Fatal("commit replay changed version")
		}
		version = committed.VersionID
		rows, err := a.Tasks.List(ctx, owner.Context, e.project.ProjectID, "", 100, false)
		if err != nil || len(rows) != 1 || len(rows[0].Input.Refs) != 1 || rows[0].Input.Refs[0].VersionID != version {
			t.Fatalf("expected one task before any explicit dispatch, version %s: %+v, %v", version, rows, err)
		}
	}
}
