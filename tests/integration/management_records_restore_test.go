package integration

import (
	"bytes"
	"context"
	"encoding/base32"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/application"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/identity/totp"
	"github.com/oujinhaoai/lantai/internal/identity/totp/totptest"
	"github.com/oujinhaoai/lantai/internal/operations"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

// This joint test deliberately uses the actual management record adapter and
// actual UI payload builders, not arbitrary config assets as a substitute.
// The first restore gives recovery evidence a real core acceptance receipt;
// the second restores all three saved record kinds into another empty home.
func TestManagementRecordsCompletePackageRestoreHTTPS(t *testing.T) {
	if os.Getenv("LANTAI_WEB_NODE_INTEGRATION") != "1" {
		t.Skip("set LANTAI_WEB_NODE_INTEGRATION=1 for real TS/HTTPS/complete-package joint test")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal(err)
	}
	version, err := exec.CommandContext(t.Context(), node, "--version").Output()
	if err != nil || !strings.HasPrefix(string(version), "v24.") {
		t.Fatalf("existing Node 24 required, got %q: %v", version, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Second)
	defer cancel()
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	e.setRole(e.admin.Context.PrincipalID, identity.RoleContributor, true)
	seed := reopenApplication(t, e)
	seedHome := seed.Instance.Layout().Home
	seedArchive := filepath.Join(t.TempDir(), "seed-backup")
	if _, err = seed.Backup(ctx, operations.BackupOptions{Destination: seedArchive}); err != nil {
		t.Fatal(err)
	}
	if err = seed.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(seedHome, seedHome+".disconnected"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(seedHome + ".disconnected") })
	const sourcePassword = "synthetic new record source administrator password"
	source, sourceSecret := restoreRecordsApp(t, e, operations.RecoveryPackagePath(seedArchive), filepath.Join(t.TempDir(), "record-source"), sourcePassword)
	// Create genuinely active source credentials before capture, then prove the
	// restored target rejects them. No credential is passed to RestorePackage.
	e.inst, e.id, e.ledger, e.rights, e.storage, e.catalog, e.secret = source.Instance, source.Identity, source.Ledger, source.Rights, source.Storage, source.Catalog, sourceSecret
	sourceLogin := func() identity.IssuedSession {
		login, err := source.Identity.Login(ctx, identity.LoginRequest{Name: "ada", Password: sourcePassword, Code: e.fresh(), Channel: identity.ChannelCLI})
		if err != nil {
			t.Fatal(err)
		}
		return login
	}
	e.admin = sourceLogin()
	command := func(cmd identity.Command) identity.Result {
		who := sourceLogin().Context
		challenge, err := source.Identity.CreateChallenge(ctx, who, cmd)
		if err != nil {
			t.Fatal(err)
		}
		grant, err := source.Identity.VerifyChallenge(ctx, who, challenge.ChallengeID, e.fresh(), "192.0.2.10")
		if err != nil {
			t.Fatal(err)
		}
		result, err := source.Identity.Execute(ctx, who, grant.GrantID, string(ids.New()), cmd)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	registered := command(&identity.RegisterPrincipal{Kind: authz.Agent, Name: "records-source@synthetic"})
	var machine struct {
		ID ids.ID `json:"principal_id"`
	}
	if err = json.Unmarshal(registered.Summary, &machine); err != nil || !machine.ID.Valid() {
		t.Fatal("register source agent", err)
	}
	principal, err := source.Identity.GetPrincipal(ctx, sourceLogin().Context, machine.ID)
	if err != nil {
		t.Fatal(err)
	}
	credential := command(&identity.IssueCredential{PrincipalID: principal.ID, ExpectedRevision: principal.Revision, Scopes: []identity.Scope{identity.ScopeRead}})
	_, membersRevision, err := source.Identity.Members(ctx, sourceLogin().Context, e.project.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	command(&identity.SetProjectRole{ProjectID: e.project.ProjectID, ExpectedRevision: membersRevision, PrincipalID: machine.ID, Role: identity.RoleContributor, Grant: true})
	oldSession, err := source.Identity.ExchangeToken(ctx, credential.Secret, identity.SessionRequest{Channel: identity.ChannelCLI})
	if err != nil {
		t.Fatal(err)
	}
	front, ca, closeSourceHTTP := recordsHTTPS(t, source)
	input := map[string]any{"mode": "save", "origin": front.URL, "project_id": e.project.ProjectID, "instance_id": source.Instance.InstanceID(), "name": "ada", "password": sourcePassword, "code": e.fresh(), "clock_ms": e.clk.Now().UnixMilli()}
	saved := runRecordsNode(t, ctx, node, ca, input)
	var sourceResult struct {
		Schema       string `json:"schema"`
		StrictTLS    bool   `json:"strict_tls"`
		AdapterLoads int    `json:"adapter_load_count"`
		Oracle       struct {
			SourceSessionID ids.ID `json:"source_session_id"`
			Records         []struct {
				Kind string `json:"kind"`
				Ref  struct {
					AssetID   ids.ID `json:"asset_id"`
					VersionID ids.ID `json:"version_id"`
				} `json:"ref"`
				Payload json.RawMessage `json:"payload"`
			} `json:"records"`
			First struct {
				AssetID   ids.ID `json:"asset_id"`
				VersionID ids.ID `json:"version_id"`
			} `json:"first"`
		} `json:"oracle"`
	}
	if err = json.Unmarshal(saved, &sourceResult); err != nil {
		t.Fatal(err)
	}
	if sourceResult.Schema != "lantai.management-records-restore-live/v1" || !sourceResult.StrictTLS || sourceResult.AdapterLoads != 6 || len(sourceResult.Oracle.Records) != 6 {
		t.Fatal("source omitted required record/TLS cases")
	}
	expected := []string{"migration", "workset", "recovery", "workset", "migration", "migration"}
	schemas := []string{"lantai.migration-record/v1", "lantai.managed-workset/v1", "lantai.recovery-evidence/v1", "lantai.managed-workset/v1", "lantai.migration-verification-record/v1", "lantai.migration-batch/v1"}
	for i, record := range sourceResult.Oracle.Records {
		if record.Kind != expected[i] || !record.Ref.AssetID.Valid() || !record.Ref.VersionID.Valid() {
			t.Fatal("record identity or kind invalid")
		}
		var payload struct {
			Schema string `json:"schema"`
		}
		if json.Unmarshal(record.Payload, &payload) != nil || payload.Schema != schemas[i] {
			t.Fatal("generic config cannot substitute for actual management record payload")
		}
	}
	if sourceResult.Oracle.Records[1].Ref.AssetID != sourceResult.Oracle.Records[3].Ref.AssetID || sourceResult.Oracle.Records[1].Ref.VersionID == sourceResult.Oracle.Records[3].Ref.VersionID {
		t.Fatal("workset revision was not a fixed version of the original record")
	}
	oldGrant, err := source.Storage.IssueReadGrant(ctx, storage.ReadRequest{Who: oldSession.Context, AssetID: sourceResult.Oracle.First.AssetID, VersionID: sourceResult.Oracle.First.VersionID, Path: "model.obj", Purpose: "archive_review"})
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "record-complete-backup")
	manifest, err := source.Backup(ctx, operations.BackupOptions{Destination: archive})
	if err != nil {
		t.Fatal(err)
	}
	packagePath := operations.RecoveryPackagePath(archive)
	info, err := operations.VerifyRecoveryPackage(ctx, packagePath)
	if err != nil {
		t.Fatal(err)
	}
	closeSourceHTTP()
	front.Close()
	sourceHome := source.Instance.Layout().Home
	if err = source.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(sourceHome, sourceHome+".disconnected"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sourceHome + ".disconnected") })
	const targetPassword = "synthetic independent restored target administrator password"
	target, targetSecret := restoreRecordsApp(t, e, packagePath, filepath.Join(t.TempDir(), "independent-empty-target"), targetPassword)
	if target.Instance.Layout().Home == sourceHome || target.Instance.Marker().RecoveryEpoch <= manifest.RecoveryEpoch {
		t.Fatal("target not independently restored with advanced epoch")
	}
	if _, err = target.Identity.VerifySession(ctx, oldSession.Context.SessionID); err == nil {
		t.Fatal("source machine session survived")
	}
	if _, err = target.Identity.VerifySession(ctx, sourceResult.Oracle.SourceSessionID); err == nil {
		t.Fatal("source browser session survived")
	}
	if _, err = target.Identity.ExchangeToken(ctx, credential.Secret, identity.SessionRequest{Channel: identity.ChannelCLI}); err == nil {
		t.Fatal("source machine credential survived")
	}
	restoredFront, targetCA, _ := recordsHTTPS(t, target)
	var rawResult map[string]json.RawMessage
	if err = json.Unmarshal(saved, &rawResult); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(30 * time.Second)
	targetInput := map[string]any{"mode": "restore", "origin": restoredFront.URL, "project_id": e.project.ProjectID, "instance_id": target.Instance.InstanceID(), "name": "ada", "password": targetPassword, "code": totp.Code(targetSecret, totp.Counter(e.clk.Now())), "clock_ms": e.clk.Now().UnixMilli(), "recovery_epoch": target.Instance.Marker().RecoveryEpoch, "oracle": rawResult["oracle"], "old_password": sourcePassword, "fresh_code_for_old_password": totp.Code(targetSecret, totp.Counter(e.clk.Now())+1)}
	loaded := runRecordsNode(t, ctx, node, targetCA, targetInput)
	var targetResult struct {
		Mode         string `json:"mode"`
		StrictTLS    bool   `json:"strict_tls"`
		AdapterLoads int    `json:"adapter_load_count"`
		Browser      bool   `json:"browser_executed"`
	}
	if err = json.Unmarshal(loaded, &targetResult); err != nil || targetResult.Mode != "restore" || !targetResult.StrictTLS || targetResult.AdapterLoads != 6 || targetResult.Browser {
		t.Fatal("target missing complete adapter restore checks", err)
	}
	// Even the newly enrolled identity cannot redeem a capability from the old
	// runtime generation. Use the target's authenticated CLI to isolate grant age.
	e.clk.Advance(30 * time.Second)
	login, err := target.Identity.Login(ctx, identity.LoginRequest{Name: "ada", Password: targetPassword, Code: totp.Code(targetSecret, totp.Counter(e.clk.Now())), Channel: identity.ChannelCLI})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, restoredFront.URL+oldGrant.URL, nil)
	req.Header.Set("Authorization", "Bearer "+login.Token)
	response, err := restoredFront.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode == http.StatusOK {
		t.Fatal("old read grant survived recovery")
	}
	// An authenticated new principal without this project's membership cannot
	// turn a known fixed record ID into either metadata or a byte capability.
	targetCommand := func(cmd identity.Command) identity.Result {
		challenge, err := target.Identity.CreateChallenge(ctx, login.Context, cmd)
		if err != nil {
			t.Fatal(err)
		}
		approved, err := target.Identity.VerifyChallenge(ctx, login.Context, challenge.ChallengeID, totptest.Fresh(t, e.clk, targetSecret), "192.0.2.10")
		if err != nil {
			t.Fatal(err)
		}
		result, err := target.Identity.Execute(ctx, login.Context, approved.GrantID, string(ids.New()), cmd)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	outsiderResult := targetCommand(&identity.RegisterPrincipal{Kind: authz.Agent, Name: "records-outsider@synthetic"})
	var outsider struct {
		ID ids.ID `json:"principal_id"`
	}
	if json.Unmarshal(outsiderResult.Summary, &outsider) != nil || !outsider.ID.Valid() {
		t.Fatal("register target outsider")
	}
	outsiderPrincipal, err := target.Identity.GetPrincipal(ctx, login.Context, outsider.ID)
	if err != nil {
		t.Fatal(err)
	}
	outsiderCredential := targetCommand(&identity.IssueCredential{PrincipalID: outsider.ID, ExpectedRevision: outsiderPrincipal.Revision, Scopes: []identity.Scope{identity.ScopeRead}})
	outsiderSession, err := target.Identity.ExchangeToken(ctx, outsiderCredential.Secret, identity.SessionRequest{Channel: identity.ChannelCLI})
	if err != nil {
		t.Fatal(err)
	}
	recordRef := sourceResult.Oracle.Records[0].Ref
	for _, check := range []struct {
		method, path string
		body         io.Reader
	}{{http.MethodGet, fmt.Sprintf("/api/v1/assets/%s/versions/%s?view=full", recordRef.AssetID, recordRef.VersionID), nil}, {http.MethodPost, fmt.Sprintf("/api/v1/assets/%s/versions/%s/read-grants", recordRef.AssetID, recordRef.VersionID), strings.NewReader(`{"path":"record.json","purpose":"archive_review"}`)}} {
		request, _ := http.NewRequestWithContext(ctx, check.method, restoredFront.URL+check.path, check.body)
		request.Header.Set("Authorization", "Bearer "+outsiderSession.Token)
		request.Header.Set("Content-Type", "application/json")
		response, err := restoredFront.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusForbidden && response.StatusCode != http.StatusNotFound {
			t.Fatalf("authenticated outsider read record: %d", response.StatusCode)
		}
	}
	after, err := operations.VerifyRecoveryPackage(ctx, packagePath)
	if err != nil || after.SHA256 != info.SHA256 {
		t.Fatal("restore changed frozen source package", err)
	}
	t.Logf("management records: actual migration/workset/recovery + migration batch/verification TS save -> complete package sha256=%s bytes=%d -> independent empty target restore -> six exact-version adapter loads; original workset fixed version retained", info.SHA256, info.Size)
	t.Log("restored batch current record/assets/purpose/worksets/verifications rechecked by backend; actual current core unknown remains recovery unknown and acceptance blocked; authenticated non-member record metadata/read-grant denied")
	t.Log("real OBJ/MTL/ComfyUI selected dependency closure and full restored workset bytes read; explicit returned version and stale-base rejection; strict trusted TLS + untrusted/hostname negatives; old browser/session/machine/grant/password denied; browser not executed")
}

func restoreRecordsApp(t *testing.T, e *env, file, home, password string) (*application.App, []byte) {
	t.Helper()
	ctx := t.Context()
	marker, err := application.RestorePackage(ctx, file, operations.RestoreOptions{Home: home}, identity.Config{Password: fastPassword})
	if err != nil {
		t.Fatal(err)
	}
	opts := application.Options{Instance: operations.Options{Home: home, Clock: e.clk}, Identity: identity.Config{Password: fastPassword}, Storage: storage.Config{MinFreeBytes: 1 << 20}}
	offline, err := application.OpenOffline(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	var secret []byte
	err = offline.Maintenance(ctx, func(c context.Context) error {
		reset, err := offline.Identity.BeginOfflineReset(c, "ada", password)
		if err != nil {
			return err
		}
		if _, err = reset.PrepareRecoveryCodes(); err != nil {
			return err
		}
		secret, err = base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(reset.Enrollment.Secret)
		if err != nil {
			return err
		}
		_, err = reset.Confirm(c, totp.Code(secret, totp.Counter(e.clk.Now())), "synthetic management record recovery")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	proof, _ := json.Marshal(map[string]any{"core_review": map[string]any{"run_id": marker.Restore.RunID, "automatic_checks_passed": true, "unresolved_operations": 0}, "operator_accepted_recovery_point": true, "operator_confirmed_known_changes_reviewed": true, "operator_confirmed_source_offline": true, "unknown_after_capture_verified": false})
	review := operations.RecoveryReview{Contract: "lantai.recovery-review/v2", RunID: marker.Restore.RunID, BackupID: marker.Restore.BackupID, ManifestDigest: marker.Restore.ManifestDigest, RecoveryEpoch: marker.RecoveryEpoch, Administrator: string(e.admin.Context.PrincipalID), EvidenceDigest: digest.Of(proof), AcceptRecoveryPoint: true, UnknownAfterCapture: true, Note: "Synthetic source is closed and disconnected; accept this capture point with unknown later changes remaining unknown."}
	if err = offline.CompleteRestore(ctx, review, proof); err != nil {
		t.Fatal(err)
	}
	if err = offline.Close(ctx); err != nil {
		t.Fatal(err)
	}
	app, err := application.Open(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close(context.Background()) })
	return app, secret
}

func recordsHTTPS(t *testing.T, a *application.App) (*httptest.Server, string, func()) {
	t.Helper()
	var apiTarget, transferTarget atomic.Pointer[httputil.ReverseProxy]
	front := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var proxy *httputil.ReverseProxy
		if strings.HasPrefix(r.URL.Path, "/api/v1/") {
			proxy = apiTarget.Load()
		} else if strings.HasPrefix(r.URL.Path, "/xfer/v1/") {
			proxy = transferTarget.Load()
		}
		if proxy == nil {
			http.NotFound(w, r)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	front.Config.ErrorLog = log.New(io.Discard, "", 0)
	front.StartTLS()
	t.Cleanup(front.Close)
	config := a.Instance.Config()
	config.Listen = operations.ListenConfig{API: "127.0.0.1:0", Transfer: "127.0.0.1:0", Operations: "127.0.0.1:0"}
	config.HTTP.AllowedOrigins = []string{front.URL}
	server, err := a.StartHTTP(t.Context(), config, application.WithAccessLog(io.Discard))
	if err != nil {
		t.Fatal(err)
	}
	closeHTTP := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}
	t.Cleanup(closeHTTP)
	for _, target := range []struct {
		address string
		store   *atomic.Pointer[httputil.ReverseProxy]
	}{{server.Addresses.API, &apiTarget}, {server.Addresses.Transfer, &transferTarget}} {
		u, err := url.Parse("http://" + target.address)
		if err != nil {
			t.Fatal(err)
		}
		proxy := httputil.NewSingleHostReverseProxy(u)
		proxy.ErrorLog = log.New(io.Discard, "", 0)
		target.store.Store(proxy)
	}
	ca := filepath.Join(t.TempDir(), "synthetic-records-ca.pem")
	if err = os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: front.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	return front, ca, closeHTTP
}

func runRecordsNode(t *testing.T, ctx context.Context, node, ca string, input any) []byte {
	t.Helper()
	web, err := filepath.Abs(filepath.Join("..", "..", "web"))
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"unit/register.mjs", "tests/management-records.restore.mjs", "node_modules/typescript/package.json"} {
		if _, err = os.Stat(filepath.Join(web, required)); err != nil {
			t.Fatal("existing dependency required; no downloads", err)
		}
	}
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, node, "--import", "./unit/register.mjs", "./tests/management-records.restore.mjs")
	cmd.Dir = web
	cmd.Stdin = bytes.NewReader(raw)
	cmd.WaitDelay = 5 * time.Second
	for _, variable := range os.Environ() {
		name, _, _ := strings.Cut(variable, "=")
		switch name {
		case "NODE_EXTRA_CA_CERTS", "NODE_OPTIONS", "NODE_TLS_REJECT_UNAUTHORIZED", "NODE_USE_ENV_PROXY", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy":
			continue
		}
		cmd.Env = append(cmd.Env, variable)
	}
	cmd.Env = append(cmd.Env, "NODE_EXTRA_CA_CERTS="+ca, "NODE_TLS_REJECT_UNAUTHORIZED=1")
	var out, diagnostic bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &diagnostic
	if err = cmd.Run(); err != nil {
		t.Fatalf("actual management-record adapter phase failed: %v\n%s", err, diagnostic.String())
	}
	return out.Bytes()
}
