package integration

import (
	"bytes"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/application"
	"github.com/oujinhaoai/lantai/internal/catalog"
	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/ledger"
)

// Representative A09/A10/A12/A15 matrix on the real assembled owners. Check
// evidence is produced by the real corecheck process; decisions use TOTP-backed
// HumanGrant. No approval, lifecycle or task record is installed with test SQL.
func TestAssetRecoveryGovernanceAndPermissions(t *testing.T) {
	f := newAppFlow(t)
	e := f.env
	ctx := t.Context()
	producer, err := f.app.Extensions.Producer(ctx, "org.lantai.corecheck.manifest")
	if err != nil {
		t.Fatal(err)
	}
	profile := f.approvedProfile("recovery/acceptance", profileDocument("recovery", producer), ids.PermanentRef{})
	checked := func(previous *catalog.VersionResult, body string) (catalog.VersionResult, ledger.ReviewTarget) {
		t.Helper()
		v, flow := f.candidateVersion("recovery/production", manifest.TypeDoc, map[string][]byte{"content.txt": []byte(body)}, nil, previous)
		job := f.checkCandidate(v, flow, "org.lantai.corecheck.manifest")
		return v, f.submitChecked(v, flow, profile.Ref, job, false)
	}
	v1, target1 := checked(nil, "rejected synthetic production v1")
	f.decide(target1.ID, "reject")
	v2, target2 := checked(&v1, "approved synthetic production v2")
	f.decide(target2.ID, "approve")
	s1, err := e.ledger.VersionControl(ctx, v1.VersionID)
	if err != nil || s1.ReviewState != "rejected" {
		t.Fatal(s1, err)
	}
	s2, err := e.ledger.VersionControl(ctx, v2.VersionID)
	if err != nil || s2.ReviewState != "approved" {
		t.Fatal(s2, err)
	}
	pub, err := f.reviews.Publish(ctx, e.login().Context, e.key(), ledger.PublishRequest{ProjectID: e.project.ProjectID, VersionID: v2.VersionID, ReviewID: s2.EffectiveReviewID, Action: "publish", Reason: "synthetic exact v2 publication"})
	if err != nil {
		t.Fatal(err)
	}

	who := e.login().Context
	heldBytes, uniqueBytes, sharedBytes := []byte("held recovery bytes"), []byte("unique GC recovery bytes"), []byte("shared GC recovery bytes")
	held := e.ingest(who, "recovery/held", heldBytes, *rightsOwned())
	unique := e.ingest(who, "recovery/gc-unique", uniqueBytes, *rightsOwned())
	shared := e.ingest(who, "recovery/gc-shared", sharedBytes, *rightsOwned())
	survivor := e.ingest(who, "recovery/survivor", sharedBytes, *rightsOwned())
	heldEntry := trashForTest(t, e, f.app.Lifecycle, who, held.AssetID)
	heldEntry, err = mutateTrashForTest(t, e, f.app.Lifecycle, who, heldEntry, identity.ActHold)
	if err != nil {
		t.Fatal(err)
	}
	uniqueEntry := trashForTest(t, e, f.app.Lifecycle, who, unique.AssetID)
	sharedEntry := trashForTest(t, e, f.app.Lifecycle, who, shared.AssetID)
	restrictedRights := *rightsOwned()
	restrictedRights.Usage = "reference"
	restrictedRights.NoAI = true
	restricted := e.ingest(who, "recovery/restricted", []byte("restricted synthetic bytes"), restrictedRights)
	outsider, _ := e.agent("recovery-outsider@synthetic", "")
	scopes := []identity.Scope{identity.ScopeRead, identity.ScopeIngest, identity.ScopeOrganize, identity.ScopeTask}
	member := availabilitySession(t, e, f.maker.PrincipalID, scopes)
	outside := availabilitySession(t, e, outsider.ID, scopes)

	var permissionServer *application.HTTPServers
	permissions := func(label string) (*client.Client, client.DownloadGrant) {
		t.Helper()
		servers := assetRemoteServers(t, f.app)
		permissionServer = servers
		good, er := client.New(client.Config{BaseURL: "http://" + servers.Addresses.API, AllowHTTP: true, SessionToken: member.Token})
		if er != nil {
			t.Fatal(er)
		}
		t.Cleanup(good.Close)
		bad, er := client.New(client.Config{BaseURL: good.Origin(), AllowHTTP: true, SessionToken: outside.Token})
		if er != nil {
			t.Fatal(er)
		}
		t.Cleanup(bad.Close)
		list, er := bad.FindAssets(ctx, client.AssetFilter{ProjectID: string(e.project.ProjectID)}, client.AssetFindOptions{})
		if er != nil || !list.Complete || len(list.Items) != 0 {
			t.Fatal("outsider search leaked assets")
		}
		if _, er = bad.Do(ctx, http.MethodGet, "/api/v1/assets/"+string(v2.AssetID)+"/versions/"+string(v2.VersionID), nil, client.Options{}); assetAPIErrorCode(er) != errcode.NotFound {
			t.Fatal("outsider exact version leaked", er)
		}
		grantResponse, er := good.Do(ctx, http.MethodPost, "/api/v1/assets/"+string(v2.AssetID)+"/versions/"+string(v2.VersionID)+"/read-grants", map[string]string{"path": "content.txt", "purpose": "archive_review"}, client.Options{})
		if er != nil {
			t.Fatal(er)
		}
		var grant client.DownloadGrant
		if er = grantResponse.Decode(&grant); er != nil {
			t.Fatal(er)
		}
		if status, _ := availabilityRange(t, good.Origin(), outside.Token, grant); status != http.StatusNotFound {
			t.Fatal("outsider Range used another holder's grant", status)
		}
		if status, body := availabilityRange(t, good.Origin(), member.Token, grant); status != http.StatusPartialContent || !bytes.Equal(body, []byte("appr")) {
			t.Fatal("authorized Range failed", status, string(body))
		}
		if _, er = good.Do(ctx, http.MethodPost, "/api/v1/assets/"+string(restricted.AssetID)+"/versions/"+string(restricted.VersionID)+"/read-grants", map[string]string{"path": "content.txt", "purpose": "production"}, client.Options{}); assetAPIErrorCode(er) != errcode.UseRestricted {
			t.Fatal("restricted production was allowed", er)
		}
		if _, er = bad.Do(ctx, http.MethodPost, "/api/v1/uploads", map[string]any{"project_id": e.project.ProjectID, "files": []map[string]any{{"sha256": shaOf([]byte("approved synthetic production v2")), "size": len("approved synthetic production v2")}}}, client.Options{IdempotencyKey: string(ids.New())}); assetAPIErrorCode(er) != errcode.NotFound {
			t.Fatal("outsider could probe or reuse project content", er)
		}
		purge := ledger.TrashMutation{Action: identity.ActPurge, ProjectID: e.project.ProjectID, TrashID: heldEntry.ID, ExpectedRevision: heldEntry.Revision, FilesDigest: heldEntry.FilesDigest, Reason: "ordinary ingest identity cannot clear data"}
		if _, er = f.app.Lifecycle.MutateTrashHuman(ctx, member.Context, purge, "", ids.New(), e.id); errcode.CodeOf(er) != errcode.Forbidden {
			t.Fatal("ordinary identity bypassed human purge", er)
		}
		request, er := f.app.Lifecycle.PreviewTrash(ctx, member.Context, ledger.TrashSelector{ProjectID: e.project.ProjectID, AssetID: v2.AssetID, VersionID: v2.VersionID, Reason: "ordinary user must not purge"})
		if er == nil {
			if _, er = f.app.Lifecycle.TrashOwn(ctx, member.Context, e.key(), request); er == nil {
				t.Fatal("ordinary member trashed protected approved production")
			}
		}
		t.Logf("%s: outsider list/detail/Range denied; member Range exact; restricted production and protected mutation denied", label)
		return good, grant
	}
	permissions("before recovery")
	due, err := clock.Parse(max(heldEntry.DueAt, uniqueEntry.DueAt, sharedEntry.DueAt))
	if err != nil {
		t.Fatal(err)
	}
	e.clk.Set(due.Add(time.Hour))
	run, err := f.app.RunLifecycle(ctx)
	if err != nil || run.Purges != 2 {
		t.Fatal("due purge with Hold", run, err)
	}
	if _, err = os.Stat(f.app.Storage.Layout().BlobPath(shaOf(uniqueBytes))); err != nil {
		t.Fatal("GC ran before 24 hours", err)
	}
	heldEntry, err = f.app.Lifecycle.Entry(ctx, e.login().Context, e.project.ProjectID, heldEntry.ID)
	if err != nil || !heldEntry.Hold || heldEntry.State != "trashed" {
		t.Fatal("Hold lost before backup", heldEntry, err)
	}
	// Renew the synthetic actors after advancing retention time. The pending
	// target is created immediately before capture, not already lease-expired.
	f.owner = e.login().Context
	member = availabilitySession(t, e, f.maker.PrincipalID, scopes)
	f.maker = member.Context
	f.worker = availabilitySession(t, e, f.worker.PrincipalID, []identity.Scope{identity.ScopeWorker}).Context
	v3, target3 := checked(&v2, "pending synthetic production v3")
	decision := ledger.ReviewDecision{Action: identity.ActRecordReview, TargetID: target3.ID, ExpectedRevision: target3.Revision, Verdict: "approve", Reason: "synthetic agent must not approve", Waivers: []ledger.ReviewWaiver{}}
	if _, err = f.reviews.Record(ctx, f.maker, decision, "", ids.New(), e.id); err == nil {
		t.Fatal("agent forged a human approval")
	}
	pending, err := e.ledger.VersionControl(ctx, v3.VersionID)
	if err != nil || pending.ReviewState == "approved" {
		t.Fatal("agent changed pending review", pending, err)
	}
	if id, er := e.ledger.Published(ctx, v2.AssetID); er != nil || id != v2.VersionID {
		t.Fatal("new pending version moved publication", id, er)
	}

	oldMemberToken := member.Token
	f.app = restoreAvailability(t, e, f.app, permissionServer)
	f.tasks, f.flows, f.reviews, f.source, f.log = f.app.Tasks, f.app.Flows, f.app.Reviews, f.app.Evidence, f.app.Events
	f.owner = e.login().Context
	f.maker = availabilitySession(t, e, f.maker.PrincipalID, scopes).Context
	f.checker = availabilitySession(t, e, f.checker.PrincipalID, scopes).Context
	f.worker = availabilitySession(t, e, f.worker.PrincipalID, []identity.Scope{identity.ScopeWorker}).Context
	member = availabilitySession(t, e, f.maker.PrincipalID, scopes)
	outside = availabilitySession(t, e, outsider.ID, scopes)
	good, grant := permissions("after recovery")
	staleClient, err := client.New(client.Config{BaseURL: good.Origin(), AllowHTTP: true, SessionToken: oldMemberToken})
	if err != nil {
		t.Fatal(err)
	}
	defer staleClient.Close()
	if _, err = staleClient.Do(ctx, http.MethodGet, "/api/v1/projects", nil, client.Options{}); assetAPIErrorCode(err) != errcode.TokenRevoked {
		t.Fatal("old credential survived", err)
	}
	for _, pair := range []struct {
		v     catalog.VersionResult
		state ledger.VersionControl
	}{{v1, s1}, {v2, s2}, {v3, pending}} {
		current, er := e.ledger.VersionControl(ctx, pair.v.VersionID)
		if er != nil || current.ReviewState != pair.state.ReviewState || current.EffectiveReviewID != pair.state.EffectiveReviewID {
			t.Fatal("historical review state changed", current, er)
		}
	}
	if id, er := e.ledger.Published(ctx, v2.AssetID); er != nil || id != v2.VersionID {
		t.Fatal("restored publication pointer moved", id, er)
	}
	decision.ExpectedRevision = target3.Revision
	decision.Reason = "fresh human must not revive captured review target"
	action, err := f.reviews.HumanAction(ctx, decision)
	if err != nil {
		t.Fatal(err)
	}
	freshHuman := e.login().Context
	g, items := grantDomainForTest(t, e, freshHuman, []identity.HumanAction{action}, f.app)
	if _, err = f.reviews.Record(ctx, freshHuman, decision, g, items[0].OperationID, e.id); errcode.CodeOf(err) != errcode.ReviewTargetStale && errcode.CodeOf(err) != errcode.LeaseStale {
		t.Fatal("captured review target revived", err)
	}
	t.Logf("captured target rejected at %s guard", errcode.CodeOf(err))
	stillPending, er := e.ledger.VersionControl(ctx, v3.VersionID)
	if er != nil || stillPending.ReviewState != pending.ReviewState {
		t.Fatal("stale target changed review", stillPending, er)
	}
	v4, target4 := checked(&v3, "new checked production after recovery")
	postDecision := ledger.ReviewDecision{Action: identity.ActRecordReview, TargetID: target4.ID, ExpectedRevision: target4.Revision, Verdict: "approve", Reason: "agent still cannot approve after recovery", Waivers: []ledger.ReviewWaiver{}}
	if _, err = f.reviews.Record(ctx, f.maker, postDecision, "", ids.New(), e.id); errcode.CodeOf(err) != errcode.Forbidden {
		t.Fatal("agent approval accepted after recovery", err)
	}
	f.decide(target4.ID, "approve")
	if id, er := e.ledger.Published(ctx, v2.AssetID); er != nil || id != v2.VersionID {
		t.Fatal("approval auto-moved manual publication", id, er)
	}
	s4, err := e.ledger.VersionControl(ctx, v4.VersionID)
	if err != nil || s4.ReviewState != "approved" {
		t.Fatal(s4, err)
	}
	if _, err = f.reviews.Publish(ctx, e.login().Context, e.key(), ledger.PublishRequest{ProjectID: e.project.ProjectID, VersionID: v4.VersionID, ReviewID: s4.EffectiveReviewID, ExpectedRevision: pub.Revision, Action: "publish", Reason: "explicit post-recovery publication"}); err != nil {
		t.Fatal(err)
	}

	// The GC waiting period and Hold survive capture. Only the unique purged
	// content is collectible; a live version still owns the shared bytes.
	e.clk.Advance(24 * time.Hour)
	run, err = f.app.RunLifecycle(ctx)
	if err != nil || run.Collected != 1 || run.Retained != 1 {
		t.Fatal("restored GC owner check", run, err)
	}
	if _, err = os.Stat(f.app.Storage.Layout().BlobPath(shaOf(uniqueBytes))); !os.IsNotExist(err) {
		t.Fatal("unreferenced blob retained", err)
	}
	for _, data := range [][]byte{heldBytes, sharedBytes} {
		if _, err = os.Stat(f.app.Storage.Layout().BlobPath(shaOf(data))); err != nil {
			t.Fatal("GC removed held/live data", err)
		}
	}
	if er := e.ledger.CheckVersionRead(ctx, unique.AssetID, unique.VersionID); errcode.CodeOf(er) != errcode.AssetPurged {
		t.Fatal("purged reference revived", er)
	}
	heldEntry, err = f.app.Lifecycle.Entry(ctx, e.login().Context, e.project.ProjectID, heldEntry.ID)
	if err != nil || !heldEntry.Hold || heldEntry.State != "trashed" {
		t.Fatal("restored Hold lost", heldEntry, err)
	}
	if _, err = mutateTrashForTest(t, e, f.app.Lifecycle, e.login().Context, heldEntry, identity.ActPurge); errcode.CodeOf(err) != errcode.TrashHeld {
		t.Fatal("held purge accepted", err)
	}
	heldEntry, err = mutateTrashForTest(t, e, f.app.Lifecycle, e.login().Context, heldEntry, identity.ActUnhold)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.app.Lifecycle.Restore(ctx, e.login().Context, e.key(), ledger.RestoreRequest{ProjectID: e.project.ProjectID, TrashID: heldEntry.ID, ExpectedRevision: heldEntry.Revision, Reason: "recover synthetic mistaken trash"}); err != nil {
		t.Fatal(err)
	}
	member = availabilitySession(t, e, f.maker.PrincipalID, scopes)
	if err = good.SetSessionToken(member.Token); err != nil {
		t.Fatal(err)
	}
	restoredCopy := filepath.Join(assetRealTempDir(t), "held")
	if _, err = good.PullWorkset(ctx, client.WorksetInput{Schema: client.WorksetSchema, Entries: []client.WorksetEntry{{AssetID: string(held.AssetID), VersionID: string(held.VersionID), Destination: "held"}, {AssetID: string(survivor.AssetID), VersionID: string(survivor.VersionID), Destination: "survivor"}}}, restoredCopy, "archive_review"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(restoredCopy, "held/content.txt"))
	if err != nil || !bytes.Equal(got, heldBytes) {
		t.Fatal("restored held bytes differ", err)
	}
	response, err := good.Do(ctx, http.MethodPost, "/api/v1/assets/"+string(v4.AssetID)+"/versions/"+string(v4.VersionID)+"/read-grants", map[string]string{"path": "content.txt", "purpose": "archive_review"}, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err = response.Decode(&grant); err != nil {
		t.Fatal(err)
	}
	e.setRole(f.maker.PrincipalID, identity.RoleContributor, false)
	if status, _ := availabilityRange(t, good.Origin(), member.Token, grant); status != http.StatusNotFound {
		t.Fatal("Range survived live role revocation", status)
	}
	if _, err = good.Do(ctx, http.MethodGet, "/api/v1/assets/"+string(v4.AssetID)+"/versions/"+string(v4.VersionID), nil, client.Options{}); assetAPIErrorCode(err) != errcode.NotFound {
		t.Fatal("detail survived live role revocation", err)
	}
	t.Log("PASS: real human reject/approve/manual publish before and after recovery; captured target stale; Hold, due purge, delayed GC, shared CAS and mistaken-trash restore; outsider and revoked Range denied")
}

func availabilityRange(t *testing.T, origin, token string, g client.DownloadGrant) (int, []byte) {
	t.Helper()
	base, err := url.Parse(origin)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := url.Parse(g.URL)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base.ResolveReference(ref).String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Range", "bytes=0-3")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, body
}
