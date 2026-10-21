package integration

import (
	"bytes"
	"context"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/application"
	"github.com/oujinhaoai/lantai/internal/catalog"
	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/identity/masterkey"
	"github.com/oujinhaoai/lantai/internal/identity/totp"
	"github.com/oujinhaoai/lantai/internal/operations"
	"github.com/oujinhaoai/lantai/internal/query"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

func TestSelfContainedPackageColdRestoreThreeAssets(t *testing.T) {
	if os.Getenv("LANTAI_RECOVERY_ISOLATED") == "restore" {
		isolatedPackageTarget(t)
		return
	}
	ctx := t.Context()
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	e.setRole(e.admin.Context.PrincipalID, identity.RoleContributor, true)
	p, oldSession := e.agent("complete-backup@synthetic", identity.RoleContributor)
	p, err := e.id.GetPrincipal(ctx, e.login().Context, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	credential := e.sudo(&identity.IssueCredential{PrincipalID: p.ID, ExpectedRevision: p.Revision, Scopes: []identity.Scope{identity.ScopeRead}})
	versions := make([]catalog.VersionResult, 3)
	bodies := make([][]byte, 3)
	for i := range versions {
		bodies[i] = bytes.Repeat([]byte(fmt.Sprintf("independent recovery source %d\n", i)), 2048+i)
		versions[i] = e.ingest(oldSession.Context, fmt.Sprintf("full-restore/source-%d", i), bodies[i], *rightsOwned())
	}
	oldGrant, err := e.storage.IssueReadGrant(ctx, storage.ReadRequest{Who: oldSession.Context, AssetID: versions[0].AssetID, VersionID: versions[0].VersionID, Path: "content.txt", Purpose: "archive_review"})
	if err != nil {
		t.Fatal(err)
	}

	configBodies := [][]byte{[]byte(`{"quality":"draft","batch":3}`), []byte(`{"quality":"final","batch":7}`)}
	configVersions := []catalog.VersionResult{}
	for n, body := range configBodies {
		req := catalog.VersionRequest{Who: oldSession.Context, IdempotencyKey: e.key(), UploadID: e.upload(oldSession.Context, body), Slug: "project-settings", Content: catalog.ContentInput{AssetType: manifest.TypeConfig, Rights: rightsOwned(), Files: []manifest.InputFile{{Path: "config.json", Role: "primary", SHA256: shaOf(body), Size: int64(len(body))}}}}
		if n > 0 {
			req.Slug = ""
			req.AssetID = configVersions[0].AssetID
			req.BaseVersionID = configVersions[0].VersionID
		}
		v, err := e.catalog.CommitVersion(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		configVersions = append(configVersions, v)
	}
	portable := map[string]any{"contract": "lantai.config/v1", "instance": map[string]any{"name": "test"}, "secrets": map[string]any{"dir": e.inst.Config().SecretsDir}, "storage": map[string]any{"min_free_bytes": 16777216, "allow_fuse": false, "max_file_bytes": 65536, "max_upload_bytes": 262144, "max_upload_files": 32, "upload_idle_expiry_seconds": 180, "upload_absolute_expiry_seconds": 1800}, "listen": map[string]any{"api": "127.0.0.1:41880", "transfer": "127.0.0.1:41881", "operations": "127.0.0.1:41990"}, "http": map[string]any{"allowed_origins": []string{"https://source.example"}, "max_json_bytes": 2097152, "api_timeout_seconds": 23}, "transfer": map[string]any{"interactive_slots": 2, "batch_slots": 3, "batch_per_principal": 1, "batch_bytes_per_second": 131072, "batch_bytes_per_second_while_interactive": 65536}, "lifecycle": map[string]any{"scheduler": false, "interval_seconds": 420, "batch": 17}}
	configRaw, _ := json.Marshal(portable)
	if err = os.WriteFile(filepath.Join(e.inst.Layout().Home, "config.yaml"), configRaw, 0600); err != nil {
		t.Fatal(err)
	}
	a := reopenApplication(t, e)
	wantConfig := a.Instance.Config()
	archive := filepath.Join(t.TempDir(), "snapshot")
	m, err := a.Backup(ctx, operations.BackupOptions{Destination: archive})
	if err != nil {
		t.Fatal(err)
	}
	file := operations.RecoveryPackagePath(archive)
	info, err := operations.VerifyRecoveryPackage(ctx, file)
	if err != nil || info.Manifest.BackupID != m.BackupID {
		t.Fatal("package invalid", err)
	}

	if os.Getenv("LANTAI_RECOVERY_ISOLATED") == "capture" {
		destination := os.Getenv("LANTAI_RECOVERY_EXPORT")
		if destination == "" {
			t.Fatal("isolated export directory required")
		}
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(destination, "recovery.lantai"), data, 0600); err != nil {
			t.Fatal(err)
		}
		oracle := recoveryOracle{Config: wantConfig, ProjectID: e.project.ProjectID, PackageSHA: info.SHA256}
		for i, v := range versions {
			oracle.Files = append(oracle.Files, recoveryExpectedFile{v.AssetID, v.VersionID, "content.txt", shaOf(bodies[i]), int64(len(bodies[i]))})
		}
		for i, v := range configVersions {
			oracle.Files = append(oracle.Files, recoveryExpectedFile{v.AssetID, v.VersionID, "config.json", shaOf(configBodies[i]), int64(len(configBodies[i]))})
		}
		data, _ = json.Marshal(oracle)
		if err = os.WriteFile(filepath.Join(destination, "oracle.json"), data, 0600); err != nil {
			t.Fatal(err)
		}
		t.Log("captured synthetic package; target recovery receives only this package, no original volume, session, or key")
		return
	}
	sourceHome := a.Instance.Layout().Home
	if err = a.Close(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sourceHome + ".disconnected") })
	if err = os.Rename(sourceHome, sourceHome+".disconnected"); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "restored")
	marker, err := application.RestorePackage(ctx, file, operations.RestoreOptions{Home: target}, identity.Config{Password: fastPassword})
	if err != nil {
		t.Fatal(err)
	}
	if marker.Restore == nil || marker.RecoveryEpoch <= m.RecoveryEpoch {
		t.Fatal("recovery latch or generation missing")
	}
	opts := application.Options{Instance: operations.Options{Home: target, Clock: e.clk}, Identity: identity.Config{Password: fastPassword}, Storage: storage.Config{MinFreeBytes: 1 << 20}}
	offline, err := application.OpenOffline(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = offline.Close(context.Background()) })
	key, err := masterkey.Load(offline.Instance.Config().SecretsDir)
	if err != nil || key.ID() == m.KeyID {
		t.Fatal("new runtime key required", err)
	}
	var secret []byte
	const password = "new standalone recovery administrator password"
	err = offline.Maintenance(ctx, func(c context.Context) error {
		r, err := offline.Identity.BeginOfflineReset(c, "ada", password)
		if err != nil {
			return err
		}
		codes, err := r.PrepareRecoveryCodes()
		if err != nil || len(codes) == 0 {
			return fmt.Errorf("recovery codes not prepared: %v", err)
		}
		secret, err = base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(r.Enrollment.Secret)
		if err != nil {
			return err
		}
		_, err = r.Confirm(c, totp.Code(secret, totp.Counter(e.clk.Now())), "synthetic standalone package restore")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	run := marker.Restore.RunID
	evidence, _ := json.Marshal(map[string]any{"core_review": map[string]any{"run_id": run, "automatic_checks_passed": true, "unresolved_operations": 0}, "operator_accepted_recovery_point": true, "operator_confirmed_known_changes_reviewed": true, "operator_confirmed_source_offline": true, "unknown_after_capture_verified": false})
	review := operations.RecoveryReview{Contract: "lantai.recovery-review/v2", RunID: run, BackupID: m.BackupID, ManifestDigest: marker.Restore.ManifestDigest, RecoveryEpoch: marker.RecoveryEpoch, Administrator: string(e.admin.Context.PrincipalID), EvidenceDigest: digest.Of(evidence), AcceptRecoveryPoint: true, UnknownAfterCapture: true, Note: "Independent synthetic recovery accepts the capture point; unknown source changes remain unknown."}
	if err = offline.CompleteRestore(ctx, review, evidence); err != nil {
		t.Fatal(err)
	}
	if err = offline.Close(ctx); err != nil {
		t.Fatal(err)
	}
	restored, err := application.Open(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restored.Close(context.Background()) })
	if _, err = restored.Identity.VerifySession(ctx, oldSession.Context.SessionID); err == nil {
		t.Fatal("old session survived")
	}
	if _, err = restored.Identity.ExchangeToken(ctx, credential.Secret, identity.SessionRequest{Channel: identity.ChannelCLI}); err == nil {
		t.Fatal("old machine credential survived")
	}
	e.clk.Advance(30 * time.Second)
	login, err := restored.Identity.Login(ctx, identity.LoginRequest{Name: "ada", Password: password, Code: totp.Code(secret, totp.Counter(e.clk.Now())), Channel: identity.ChannelCLI})
	if err != nil {
		t.Fatal(err)
	}
	page, err := restored.Query.Search(ctx, query.SearchRequest{Who: login.Context, Filter: query.Filter{ProjectID: e.project.ProjectID}})
	if err != nil || len(page.Items) != 4 {
		t.Fatalf("restored assets %d: %v", len(page.Items), err)
	}

	gotConfig := restored.Instance.Config()
	if gotConfig.InstanceName != wantConfig.InstanceName || gotConfig.MinFreeBytes != wantConfig.MinFreeBytes || gotConfig.Uploads != wantConfig.Uploads || gotConfig.Transfer != wantConfig.Transfer || gotConfig.HTTP.MaxJSONBytes != wantConfig.HTTP.MaxJSONBytes || gotConfig.HTTP.APITimeoutSeconds != wantConfig.HTTP.APITimeoutSeconds || gotConfig.Lifecycle.IntervalSeconds != 420 || gotConfig.Lifecycle.Batch != 17 || gotConfig.Lifecycle.Scheduler {
		t.Fatal("portable configuration not preserved", gotConfig)
	}
	if gotConfig.SecretsDir == wantConfig.SecretsDir || gotConfig.Listen.API == wantConfig.Listen.API || len(gotConfig.HTTP.AllowedOrigins) != 0 {
		t.Fatal("old deployment binding revived")
	}
	oversized := bytes.Repeat([]byte("x"), 65537)
	if _, err = restored.Storage.CreateUpload(ctx, storage.CreateUploadRequest{Who: login.Context, IdempotencyKey: string(ids.New()), ProjectID: e.project.ProjectID, Files: []storage.FileSpec{{SHA256: shaOf(oversized), Size: int64(len(oversized))}}}); errcode.CodeOf(err) != errcode.QuotaExceeded {
		t.Fatal("restored file limit not enforced", err)
	}
	small := []byte("new write after standalone recovery")
	u, err := restored.Storage.CreateUpload(ctx, storage.CreateUploadRequest{Who: login.Context, IdempotencyKey: string(ids.New()), ProjectID: e.project.ProjectID, Files: []storage.FileSpec{{SHA256: shaOf(small), Size: int64(len(small))}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = restored.Storage.PutPart(ctx, storage.PartRequest{Who: login.Context, UploadID: u.UploadID, SHA256: shaOf(small), PartNumber: 1, PartSHA256: shaOf(small), Size: int64(len(small)), Body: bytes.NewReader(small)}); err != nil {
		t.Fatal(err)
	}
	if _, err = restored.Storage.CompleteFile(ctx, login.Context, u.UploadID, shaOf(small)); err != nil {
		t.Fatal(err)
	}
	smallVersion, err := restored.Catalog.CommitVersion(ctx, catalog.VersionRequest{Who: login.Context, IdempotencyKey: string(ids.New()), UploadID: u.UploadID, Slug: "after-recovery", Content: catalog.ContentInput{AssetType: manifest.TypeDoc, Rights: rightsOwned(), Files: []manifest.InputFile{{Path: "content.txt", Role: "primary", SHA256: shaOf(small), Size: int64(len(small))}}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Log("portable non-default limits preserved; 65,537-byte upload rejected; new small version committed")
	servers := remoteServers(t, restored, false)
	for i, v := range versions {
		grant, err := restored.Storage.IssueReadGrant(ctx, storage.ReadRequest{Who: login.Context, AssetID: v.AssetID, VersionID: v.VersionID, Path: "content.txt", Purpose: "archive_review"})
		if err != nil {
			t.Fatal(err)
		}
		r, err := http.NewRequestWithContext(ctx, "GET", "http://"+servers.Addresses.Transfer+grant.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer "+login.Token)
		response, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if err != nil || response.StatusCode != 200 || shaOf(body) != shaOf(bodies[i]) {
			t.Fatalf("asset %d payload changed: %v", i, err)
		}
	}

	checks := []struct {
		v    catalog.VersionResult
		path string
		body []byte
	}{{configVersions[0], "config.json", configBodies[0]}, {configVersions[1], "config.json", configBodies[1]}, {smallVersion, "content.txt", small}}
	for _, check := range checks {
		_, err := restored.Catalog.GetVersion(ctx, login.Context, check.v.AssetID, check.v.VersionID)
		if err != nil {
			t.Fatal(err)
		}
		grant, err := restored.Storage.IssueReadGrant(ctx, storage.ReadRequest{Who: login.Context, AssetID: check.v.AssetID, VersionID: check.v.VersionID, Path: check.path, Purpose: "archive_review"})
		if err != nil {
			t.Fatal(err)
		}
		req, _ := http.NewRequestWithContext(ctx, "GET", "http://"+servers.Addresses.Transfer+grant.URL, nil)
		req.Header.Set("Authorization", "Bearer "+login.Token)
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil || response.StatusCode != 200 || !bytes.Equal(got, check.body) {
			t.Fatal("configuration history or new write differs", err)
		}
	}
	r, err := http.NewRequestWithContext(ctx, "GET", "http://"+servers.Addresses.Transfer+oldGrant.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer "+login.Token)
	response, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode == 200 {
		t.Fatal("old content capability survived")
	}
	if _, err = operations.VerifyRecoveryPackage(ctx, file); err != nil {
		t.Fatal("restore modified source package", err)
	}
	if !ids.ID(review.Administrator).Valid() {
		t.Fatal("administrator binding lost")
	}
	t.Logf("complete package sha256=%s bytes=%d backup_id=%s; source disconnected; epoch %d -> %d; runtime key rotated; old identities denied", info.SHA256, info.Size, m.BackupID, m.RecoveryEpoch, marker.RecoveryEpoch)
	for i, body := range bodies {
		t.Logf("restored asset %s bytes=%d sha256=%s", versions[i].AssetID, len(body), shaOf(body))
	}
	t.Log("one sensitive package restored three original assets and exact payloads without any access to the disconnected source or a separately supplied key")
}

// These two stages are used by the isolated container driver. The oracle holds
// only public IDs/configuration and expected hashes. It is read AFTER restore,
// never supplied to the recovery implementation as data or key material.
type recoveryExpectedFile struct {
	AssetID   ids.ID
	VersionID ids.ID
	Path      string
	SHA256    string
	Size      int64
}
type recoveryOracle struct {
	Config     operations.Config
	ProjectID  ids.ID
	PackageSHA string
	Files      []recoveryExpectedFile
}

func isolatedPackageTarget(t *testing.T) {
	ctx := t.Context()
	file := os.Getenv("LANTAI_RECOVERY_INPUT")
	if file == "" {
		t.Fatal("isolated input package required")
	}
	target := filepath.Join(t.TempDir(), "restored")
	m, err := application.RestorePackage(ctx, file, operations.RestoreOptions{Home: target}, identity.Config{Password: fastPassword})
	if err != nil {
		t.Fatal(err)
	}
	opts := application.Options{Instance: operations.Options{Home: target}, Identity: identity.Config{Password: fastPassword}}
	a, err := application.OpenOffline(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close(context.Background()) }()
	var principal ids.ID
	var name string
	var secret []byte
	const password = "new isolated administrator password"
	err = a.Maintenance(ctx, func(c context.Context) error {
		admins, err := a.Identity.RestoreAdministrators(c)
		if err != nil {
			return err
		}
		if len(admins) == 0 {
			return fmt.Errorf("no restored administrator")
		}
		name = admins[0].Name
		reset, err := a.Identity.BeginOfflineReset(c, name, password)
		if err != nil {
			return err
		}
		principal = reset.Principal().ID
		if _, err = reset.PrepareRecoveryCodes(); err != nil {
			return err
		}
		secret, err = base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(reset.Enrollment.Secret)
		if err != nil {
			return err
		}
		_, err = reset.Confirm(c, totp.Code(secret, totp.Counter(time.Now())), "isolated synthetic restore")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	proof, _ := json.Marshal(map[string]any{"core_review": map[string]any{"run_id": m.Restore.RunID, "automatic_checks_passed": true, "unresolved_operations": 0}, "operator_accepted_recovery_point": true, "operator_confirmed_known_changes_reviewed": true, "operator_confirmed_source_offline": true, "unknown_after_capture_verified": false})
	review := operations.RecoveryReview{Contract: "lantai.recovery-review/v2", RunID: m.Restore.RunID, BackupID: m.Restore.BackupID, ManifestDigest: m.Restore.ManifestDigest, RecoveryEpoch: m.RecoveryEpoch, Administrator: string(principal), AcceptRecoveryPoint: true, UnknownAfterCapture: true, EvidenceDigest: digest.Of(proof), Note: "Isolated source container destroyed; target explicitly accepts this synthetic recovery point."}
	if err = a.CompleteRestore(ctx, review, proof); err != nil {
		t.Fatal(err)
	}
	if err = a.Close(ctx); err != nil {
		t.Fatal(err)
	}
	a, err = application.Open(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	// The enrollment consumed the current step; use a new allowed future step.
	login, err := a.Identity.Login(ctx, identity.LoginRequest{Name: name, Password: password, Code: totp.Code(secret, totp.Counter(time.Now())+1), Channel: identity.ChannelCLI})
	if err != nil {
		t.Fatal(err)
	}
	var oracle recoveryOracle
	raw, err := os.ReadFile(os.Getenv("LANTAI_RECOVERY_ORACLE"))
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &oracle); err != nil {
		t.Fatal(err)
	}
	cfg := a.Instance.Config()
	want := oracle.Config
	if cfg.InstanceName != want.InstanceName || cfg.MinFreeBytes != want.MinFreeBytes || cfg.Uploads != want.Uploads || cfg.Transfer != want.Transfer || cfg.HTTP.MaxJSONBytes != want.HTTP.MaxJSONBytes || cfg.HTTP.APITimeoutSeconds != want.HTTP.APITimeoutSeconds || cfg.Lifecycle.Batch != want.Lifecycle.Batch || cfg.Lifecycle.IntervalSeconds != want.Lifecycle.IntervalSeconds || cfg.Lifecycle.Scheduler {
		t.Fatal("isolated portable config mismatch", cfg)
	}
	srv := remoteServers(t, a, false)
	for _, item := range oracle.Files {
		grant, err := a.Storage.IssueReadGrant(ctx, storage.ReadRequest{Who: login.Context, AssetID: item.AssetID, VersionID: item.VersionID, Path: item.Path, Purpose: "archive_review"})
		if err != nil {
			t.Fatal(err)
		}
		r, _ := http.NewRequestWithContext(ctx, "GET", "http://"+srv.Addresses.Transfer+grant.URL, nil)
		r.Header.Set("Authorization", "Bearer "+login.Token)
		response, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil || response.StatusCode != 200 || int64(len(body)) != item.Size || shaOf(body) != item.SHA256 {
			t.Fatal("isolated restored bytes differ", item.AssetID, err)
		}
	}
	excessive := bytes.Repeat([]byte("x"), 65537)
	if _, err = a.Storage.CreateUpload(ctx, storage.CreateUploadRequest{Who: login.Context, IdempotencyKey: string(ids.New()), ProjectID: oracle.ProjectID, Files: []storage.FileSpec{{SHA256: shaOf(excessive), Size: int64(len(excessive))}}}); errcode.CodeOf(err) != errcode.QuotaExceeded {
		t.Fatal("isolated configured limit not active", err)
	}
	t.Logf("ISOLATED PASS: only package input; new runtime identity; %d historical payloads verified; portable config preserved and quota enforced; ready=%v", len(oracle.Files), a.Ready())
}
