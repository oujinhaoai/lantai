package integration

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/catalog"
	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/provenance"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

// This is a rights/identity fixture, not a native media-format fixture. The
// small opaque payloads distinguish the original input versions; all mutations
// go through their ordinary storage/catalog owners, never SQL state edits.
func readinessAsset(t *testing.T, e *env, who authz.Context, slug string, kind manifest.AssetType, previous *catalog.VersionResult, uses ...catalog.DeclaredUse) catalog.VersionResult {
	t.Helper()
	data := []byte("synthetic rights graph payload: " + slug)
	u, err := e.storage.CreateUpload(t.Context(), storage.CreateUploadRequest{Who: who, IdempotencyKey: e.key(), ProjectID: e.project.ProjectID, Files: []storage.FileSpec{{SHA256: shaOf(data), Size: int64(len(data))}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.storage.PutPart(t.Context(), storage.PartRequest{Who: who, UploadID: u.UploadID, SHA256: shaOf(data), PartNumber: 1, PartSHA256: shaOf(data), Size: int64(len(data)), Body: bytes.NewReader(data)}); err != nil {
		t.Fatal(err)
	}
	if _, err = e.storage.CompleteFile(t.Context(), who, u.UploadID, shaOf(data)); err != nil {
		t.Fatal(err)
	}
	req := catalog.VersionRequest{Who: who, IdempotencyKey: e.key(), UploadID: u.UploadID, Slug: slug, Content: catalog.ContentInput{AssetType: kind, Rights: rightsOwned(), Uses: uses, Files: []manifest.InputFile{{Path: "payload.bin", Role: "primary", SHA256: shaOf(data), Size: int64(len(data))}}}}
	if previous != nil {
		req.Slug, req.AssetID, req.BaseVersionID = "", previous.AssetID, previous.VersionID
	}
	v, err := e.catalog.CommitVersion(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestReadinessMultiInputRightsTightening(t *testing.T) {
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	ctx := t.Context()
	e.setRole(e.admin.Context.PrincipalID, identity.RoleOwner, true)
	session := e.login()
	who := session.Context
	character := readinessAsset(t, e, who, "readiness/character-v1", manifest.TypeImage, nil)
	motion := readinessAsset(t, e, who, "readiness/motion-v1", manifest.TypeModel, nil)
	recipe := readinessAsset(t, e, who, "readiness/recipe-v1", manifest.TypeConfig, nil)
	inputs := []catalog.VersionResult{character, motion, recipe}
	uses := make([]catalog.DeclaredUse, 0, len(inputs))
	for _, v := range inputs {
		uses = append(uses, catalog.DeclaredUse{AssetID: v.AssetID, VersionID: v.VersionID, Relation: "derived_from"})
	}
	clip := readinessAsset(t, e, who, "readiness/finished-clip", manifest.TypeVideo, nil, uses...)
	// New source work must not float the already committed clip to a newer input.
	newCharacter := readinessAsset(t, e, who, "readiness/character-v2", manifest.TypeImage, &character)
	visible, err := e.rights.VisibleUses(ctx, who, clip.Ref)
	if err != nil || len(visible) != 3 {
		t.Fatal("three fixed input edges required", visible, err)
	}
	for _, input := range inputs {
		matched := false
		for _, edge := range visible {
			if edge.AssetID == input.AssetID && edge.VersionID == input.VersionID {
				matched = true
			}
		}
		if !matched {
			t.Fatal("fixed input lost", input.Ref)
		}
	}
	for _, edge := range visible {
		if edge.VersionID == newCharacter.VersionID {
			t.Fatal("input floated to latest")
		}
	}
	incoming, err := e.rights.IncomingUses(ctx, []ids.PermanentRef{motion.Ref})
	if err != nil || len(incoming) != 1 || incoming[0].Source != clip.Ref || incoming[0].Target != motion.Ref {
		t.Fatal("reverse use trace", incoming, err)
	}
	before, err := e.rights.EvaluateUse(ctx, who, clip.Ref, authz.PurposeProduction)
	if err != nil || !before.Allowed {
		t.Fatal(before, err)
	}
	grant, err := e.storage.IssueReadGrant(ctx, storage.ReadRequest{Who: who, AssetID: clip.AssetID, VersionID: clip.VersionID, Path: "payload.bin", Purpose: authz.PurposeProduction})
	if err != nil {
		t.Fatal(err)
	}
	proof := rightsEvidence(t, e, who, motion, false)
	revision, err := e.rights.CurrentRevision(ctx, motion.VersionID)
	if err != nil {
		t.Fatal(err)
	}
	reference := "reference"
	assertion := provenance.AssertionRequest{Subject: motion.Ref, ManifestDigest: motion.ManifestDigest, ExpectedRevision: revision, Kind: "restrict", Fields: provenance.AssertionFields{Usage: &reference}, EvidenceIDs: []ids.ID{proof.RecordID}, Reason: "synthetic source license narrowed to reference only"}
	if _, err = e.rights.ApplyAssertion(ctx, who, e.key(), assertion); err != nil {
		t.Fatal(err)
	}
	after, err := e.rights.EvaluateUse(ctx, who, clip.Ref, authz.PurposeProduction)
	if err != nil || after.Allowed || errcode.CodeOf(after.Err()) != errcode.UseRestricted || after.RightsEpoch <= before.RightsEpoch {
		t.Fatal("source restriction did not propagate", after, err)
	}
	checkDecision(t, e, who, clip.Ref, authz.PurposeArchiveReview, "")
	response := e.http(http.MethodGet, grant.URL, session.Token, map[string]string{"Range": "bytes=0-3"}, nil, 0)
	var envelope struct {
		Error struct {
			Code errcode.Code `json:"code"`
		} `json:"error"`
	}
	if err = json.Unmarshal(response.body, &envelope); err != nil || response.status != http.StatusForbidden || envelope.Error.Code != errcode.UseRestricted {
		t.Fatal("old production grant bypassed tightened input", response.status, envelope.Error.Code, err)
	}
	_, err = e.storage.IssueReadGrant(ctx, storage.ReadRequest{Who: who, AssetID: clip.AssetID, VersionID: clip.VersionID, Path: "payload.bin", Purpose: authz.PurposeProduction})
	if errcode.CodeOf(err) != errcode.UseRestricted {
		t.Fatal("new production grant bypassed tightened input", err)
	}
	committed, err := e.ledger.Version(ctx, clip.AssetID, clip.VersionID)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := e.storage.ReadManifest(ctx, committed)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := manifest.Parse(stored)
	if err != nil || doc.Content.Rights.Usage != "production" || len(doc.Content.Uses) != 3 {
		t.Fatal("historical manifest was rewritten", err)
	}
	t.Log("A11 PASS: three exact input versions, reverse trace, newer source does not float; current input restriction changes rights epoch and rejects existing/new production grants while archive review remains allowed")
}

func TestReadinessExpiredSessionResumesOriginalUpload(t *testing.T) {
	cfg := storage.Config{PartSize: 64 << 10, SinglePartMax: 64 << 10, MinFreeBytes: 1 << 20}
	e := newEnv(t, cfg, transfer.Limits{})
	ctx := t.Context()
	principal, _ := e.agent("natural-expiry@synthetic", identity.RoleContributor)
	p, err := e.id.GetPrincipal(ctx, e.login().Context, principal.ID)
	if err != nil {
		t.Fatal(err)
	}
	credential := e.sudo(&identity.IssueCredential{PrincipalID: p.ID, ExpectedRevision: p.Revision, Scopes: []identity.Scope{identity.ScopeRead, identity.ScopeIngest}})
	a := reopenApplication(t, e, cfg)
	servers := assetRemoteServers(t, a)
	upstream, _ := url.Parse("http://" + servers.Addresses.API)
	proxy := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) { r.SetURL(upstream) }}
	var expired atomic.Bool
	var firstPart atomic.Int64
	proxy.ModifyResponse = func(r *http.Response) error {
		if r.Request.Method == http.MethodPut && strings.HasSuffix(r.Request.URL.Path, "/parts/1") {
			firstPart.Add(1)
			if r.StatusCode == http.StatusOK && expired.CompareAndSwap(false, true) {
				e.clk.Advance(61 * time.Second)
			}
		}
		return nil
	}
	front := httptest.NewServer(proxy)
	t.Cleanup(front.Close)
	anon, err := client.New(client.Config{BaseURL: front.URL, AllowHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	defer anon.Close()
	exchange := func() (string, identity.SessionInfo) {
		t.Helper()
		response, er := anon.Do(ctx, http.MethodPost, "/api/v1/sessions/exchange", map[string]any{"token": credential.Secret, "channel": "cli", "ttl_seconds": 60, "scopes": []identity.Scope{identity.ScopeRead, identity.ScopeIngest}}, client.Options{})
		if er != nil {
			t.Fatal(er)
		}
		var value struct {
			Token   string               `json:"token"`
			Session identity.SessionInfo `json:"session"`
		}
		if er = response.Decode(&value); er != nil {
			t.Fatal(er)
		}
		if value.Session.PrincipalID != principal.ID || len(value.Session.Scopes) != 3 || !slices.Contains(value.Session.Scopes, identity.ScopeRead) || !slices.Contains(value.Session.Scopes, identity.ScopeIngest) || !slices.Contains(value.Session.Scopes, identity.ScopeSelf) || value.Session.ExpiresAt.After(e.clk.Now().Add(time.Minute)) {
			t.Fatal("exchange widened principal, scopes or TTL", value.Session)
		}
		return value.Token, value.Session
	}
	token, initial := exchange()
	c, err := client.New(client.Config{BaseURL: front.URL, AllowHTTP: true, SessionToken: token})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	root := assetRealTempDir(t)
	body := bytes.Repeat([]byte("synthetic session expiry payload\n"), 9000)
	if err = os.WriteFile(filepath.Join(root, "source.bin"), body, 0600); err != nil {
		t.Fatal(err)
	}
	in := client.PushInput{ProjectID: string(e.project.ProjectID), Slug: "readiness/natural-session", Content: client.ContentInput{AssetType: "doc", Files: []client.InputFile{{Path: "source.bin", Role: "primary"}}, Rights: mustJSON(rightsOwned())}}
	in, err = client.ApplyAssetProfile(in, client.AssetProfile{Source: "synthetic", BusinessState: "pending_review"})
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(assetRealTempDir(t), "push.json")
	if _, err = c.Push(ctx, in, root, state, false); assetAPIErrorCode(err) != errcode.TokenExpired || !expired.Load() {
		t.Fatal("natural session expiry was not observed", err)
	}
	var before client.PushState
	if err = client.LoadState(state, &before); err != nil || before.UploadID == "" || before.OperationID == "" || before.CreateKey == "" || before.CommitKey == "" {
		t.Fatal("original recovery state missing", err)
	}
	freshToken, fresh := exchange()
	if fresh.SessionID == initial.SessionID || fresh.CredentialID != initial.CredentialID || !initial.ExpiresAt.Before(e.clk.Now()) {
		t.Fatal("session was extended or machine identity changed")
	}
	if err = c.SetSessionToken(freshToken); err != nil {
		t.Fatal(err)
	}
	result, err := c.Push(ctx, in, root, state, false)
	if err != nil {
		t.Fatal(err)
	}
	var receipt struct {
		AssetID            string `json:"asset_id"`
		VersionID          string `json:"version_id"`
		OperationID        string `json:"operation_id"`
		DescriptionPending bool   `json:"description_pending"`
	}
	if err = result.Decode(&receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.OperationID != before.OperationID || !receipt.DescriptionPending || firstPart.Load() != 1 {
		t.Fatal("resume changed operation, resent accepted part, or gained organize permission", receipt, firstPart.Load())
	}
	var after client.PushState
	if err = client.LoadState(state, &after); err != nil || after.UploadID != before.UploadID || after.OperationID != before.OperationID || after.CreateKey != before.CreateKey || after.CommitKey != before.CommitKey || after.RequestHash != before.RequestHash {
		t.Fatal("resume replaced original state", err)
	}
	again, err := c.Push(ctx, in, root, state, false)
	if err != nil || !bytes.Equal(again.Body, result.Body) {
		t.Fatal("same state did not replay the same pending receipt", err)
	}
	if bytes.Contains(mustJSON(after), []byte(credential.Secret)) || bytes.Contains(mustJSON(after), []byte(freshToken)) {
		t.Fatal("recovery state leaked credentials")
	}
	t.Logf("A14 PASS: natural 60s session expired after accepted part 1; new bounded session from same credential resumed original upload/operation/keys, part1_requests=%d, description_pending retained without organize scope", firstPart.Load())
}
