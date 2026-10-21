package integration

import (
	"bytes"
	"context"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/application"
	"github.com/oujinhaoai/lantai/internal/cli"
	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/identity/masterkey"
	"github.com/oujinhaoai/lantai/internal/identity/totp"
	"github.com/oujinhaoai/lantai/internal/operations"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

const jointOracleSlug = "joint-recovery-verification"

type jointAsset struct {
	Key          string             `json:"asset_key"`
	Type         string             `json:"asset_type"`
	Coverage     string             `json:"coverage_id"`
	Version      string             `json:"version_label"`
	Root         string             `json:"root"`
	Files        []client.InputFile `json:"files"`
	Rights       json.RawMessage    `json:"rights"`
	Entrypoints  []string           `json:"entrypoints"`
	Dependencies []struct {
		From string `json:"from"`
		To   string `json:"to"`
	} `json:"dependencies"`
	Profile struct {
		Characters    []string `json:"characters"`
		Source        string   `json:"source"`
		BusinessState string   `json:"business_state"`
	} `json:"expected_metadata"`
	AssetID     string          `json:"asset_id"`
	VersionID   string          `json:"version_id"`
	Manifest    json.RawMessage `json:"manifest"`
	Description json.RawMessage `json:"description,omitempty"`
}

// The verifier is a synthetic config asset inside the backup, not a second
// recovery input. It contains public IDs, expected manifests and hashes only.
type jointOracle struct {
	Contract          string          `json:"contract"`
	InstanceID        string          `json:"instance_id"`
	ProjectID         string          `json:"project_id"`
	SourceOrigin      string          `json:"source_origin"`
	GeneratorManifest json.RawMessage `json:"generator_manifest"`
	Assets            []jointAsset    `json:"assets"`
}

func jointClient(t *testing.T, a *application.App, token string) *client.Client {
	t.Helper()
	s := assetRemoteServers(t, a)
	c, err := client.New(client.Config{BaseURL: "http://" + s.Addresses.API, AllowHTTP: true, SessionToken: token})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func jointCanonical(t *testing.T, raw []byte) []byte {
	t.Helper()
	b, err := canonjson.Canonicalize(raw)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func jointBatch(t *testing.T, c *client.Client, token, limited, source, work, name string, input client.ImportInput) client.ImportReport {
	t.Helper()
	plan, err := client.PrepareImport(t.Context(), input, source)
	if err != nil {
		t.Fatal(err)
	}
	planFile, state := filepath.Join(work, name+"-plan.json"), filepath.Join(work, name+"-state")
	if err = os.WriteFile(planFile, mustJSON(plan), 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"asset", "import", "apply", "--server", c.Origin(), "--allow-http", "--input", planFile, "--directory", source, "--state", state, "--output", filepath.Join(work, name+"-report.json")}
	t.Setenv("LANTAI_SESSION", "")
	t.Setenv("LANTAI_CA_FILE", "")
	invoke := func(credential string) (int, client.ImportReport) {
		t.Helper()
		t.Setenv("LANTAI_SESSION_TOKEN", credential)
		var out, stderr bytes.Buffer
		code := cli.Run(t.Context(), args, &out, &stderr)
		var report client.ImportReport
		if err := json.Unmarshal(out.Bytes(), &report); err != nil {
			t.Fatalf("batch %s: %d %s %s", name, code, out.String(), stderr.String())
		}
		if bytes.Contains(out.Bytes(), []byte(credential)) || bytes.Contains(stderr.Bytes(), []byte(credential)) {
			t.Fatal("credential leaked")
		}
		return code, report
	}
	var pending client.ImportReport
	if limited != "" {
		code, first := invoke(limited)
		pending = first
		if code == 0 || first.Complete || len(first.Results) != len(input.Assets) {
			t.Fatal("limited batch falsely completed", code)
		}
		for _, r := range first.Results {
			if r.Status != "description_pending" || r.VersionID == "" {
				t.Fatalf("missing committed pending receipt: %+v", r)
			}
		}
	}
	code, report := invoke(token)
	if code != 0 || !report.Complete || len(report.Results) != len(input.Assets) {
		t.Fatal("batch did not complete", name, code, report)
	}
	if limited != "" {
		for i, r := range report.Results {
			p := pending.Results[i]
			if r.AssetID != p.AssetID || r.VersionID != p.VersionID || r.OperationID != p.OperationID {
				t.Fatal("pending resume changed committed identity")
			}
		}
	}
	code, replay := invoke(token)
	if code != 0 || !reflect.DeepEqual(report, replay) {
		t.Fatal("batch retry changed receipts", name, code)
	}
	for range 2 {
		verified, err := c.VerifyImport(t.Context(), plan, report, source, state, filepath.Join(work, name+"-verify"), "archive_review")
		if err != nil || !verified.Complete {
			t.Fatal("batch independent verification", name, err)
		}
	}
	t.Logf("batch %s: %d stable asset/version/operation receipts; pending resume=%v; independent verify and replay passed", name, len(report.Results), limited != "")
	return report
}

func jointReadManifest(t *testing.T, c *client.Client, a jointAsset) json.RawMessage {
	t.Helper()
	r, err := c.Do(t.Context(), http.MethodGet, "/api/v1/assets/"+a.AssetID+"/versions/"+a.VersionID+"?view=full", nil, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Manifest json.RawMessage `json:"manifest"`
	}
	if err = r.Decode(&v); err != nil || len(v.Manifest) == 0 {
		t.Fatal("missing fixed manifest", err)
	}
	return jointCanonical(t, v.Manifest)
}

func jointDescription(t *testing.T, c *client.Client, id string) json.RawMessage {
	t.Helper()
	r, err := c.Do(t.Context(), http.MethodGet, "/api/v1/assets/"+id+"?view=full", nil, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Description json.RawMessage `json:"description"`
	}
	if err = r.Decode(&v); err != nil {
		t.Fatal(err)
	}
	return jointCanonical(t, v.Description)
}

func jointValidate(t *testing.T, c *client.Client, oracle jointOracle, destination string) {
	t.Helper()
	filter := client.AssetFilter{ProjectID: oracle.ProjectID, Characters: []string{"hero-a"}, Source: "synthetic", BusinessState: "pending_review"}
	found, err := c.FindAssets(t.Context(), filter, client.AssetFindOptions{PageSize: 2})
	if err != nil || !found.Complete || len(found.Items) != 9 {
		t.Fatal("restored precise AND query", len(found.Items), err)
	}
	latest := map[string]jointAsset{}
	versionCounts := map[string]int64{}
	ws := client.WorksetInput{Schema: client.WorksetSchema}
	count := 0
	for _, a := range oracle.Assets {
		latest[a.AssetID] = a
		versionCounts[a.AssetID]++
		if !bytes.Equal(jointReadManifest(t, c, a), jointCanonical(t, a.Manifest)) {
			t.Fatal("fixed manifest/history/rights changed", a.Key, a.Version)
		}
		ws.Entries = append(ws.Entries, client.WorksetEntry{AssetID: a.AssetID, VersionID: a.VersionID, Destination: a.Root})
		for _, path := range a.Entrypoints {
			ws.Entrypoints = append(ws.Entrypoints, a.Root+"/"+path)
		}
		for _, d := range a.Dependencies {
			dep := client.WorksetDependency{From: a.Root + "/" + d.From, Requires: []string{a.Root + "/" + d.To}}
			ws.Dependencies = append(ws.Dependencies, dep)
		}
		count += len(a.Files)
		if a.Coverage == "T08" {
			_, deny := c.PullWorkset(t.Context(), client.WorksetInput{Schema: client.WorksetSchema, Entries: []client.WorksetEntry{ws.Entries[len(ws.Entries)-1]}}, filepath.Join(assetRealTempDir(t), "denied"), "production")
			if assetAPIErrorCode(deny) != errcode.UseRestricted {
				t.Fatal("restricted rights changed", deny)
			}
		}
	}
	for _, item := range found.Items {
		a, ok := latest[item.AssetID]
		if !ok || item.VersionID != a.VersionID || item.VersionNumber != versionCounts[item.AssetID] || !bytes.Equal(jointDescription(t, c, item.AssetID), jointCanonical(t, a.Description)) {
			t.Fatal("current metadata/latest version changed", item.AssetID)
		}
	}
	filter.Characters = []string{"hero-b"}
	miss, err := c.FindAssets(t.Context(), filter, client.AssetFindOptions{})
	if err != nil || !miss.Complete || len(miss.Items) != 0 {
		t.Fatal("nonmatching character leaked into query", err)
	}
	plan, err := c.PlanWorkset(t.Context(), ws, destination)
	if err != nil || plan.FileCount != count || plan.Origin != c.Origin() {
		t.Fatal("fixed workset plan", err)
	}
	for _, entry := range plan.Entries {
		if entry.InstanceID != oracle.InstanceID {
			t.Fatal("restored instance binding changed")
		}
	}
	for range 2 {
		if _, err = c.PullWorkset(t.Context(), ws, destination, "archive_review"); err != nil {
			t.Fatal("fixed workset/replay", err)
		}
	}
	for _, a := range oracle.Assets {
		for _, f := range a.Files {
			b, err := os.ReadFile(filepath.Join(destination, filepath.FromSlash(a.Root), filepath.FromSlash(f.Path)))
			if err != nil || int64(len(b)) != f.Size || shaOf(b) != f.SHA256 {
				t.Fatal("restored original bytes differ", a.Key, a.Version, f.Path, err)
			}
		}
	}
	if err = os.WriteFile(filepath.Join(destination, "manifest.json"), oracle.GeneratorManifest, 0600); err != nil {
		t.Fatal(err)
	}
	t.Logf("joint validation: 9 exact query matches; %d fixed versions; %d file hashes; current descriptions, frozen profiles, restrictions and workset replay preserved", len(oracle.Assets), count)
}

// Explicitly enabled synthetic corpus; the isolated restore mode accepts only
// the complete package. No source fixture, source state, key file or oracle is
// supplied to the restore implementation or the post-restore verifier.
func TestAssetRecoveryJointPackage(t *testing.T) {
	if os.Getenv("LANTAI_ASSET_RECOVERY_MODE") == "restore" {
		if os.Getenv("LANTAI_ASSET_FIXTURE_ROOT") != "" {
			t.Fatal("isolated target must not receive source fixtures")
		}
		jointRestore(t, os.Getenv("LANTAI_ASSET_RECOVERY_PACKAGE"))
		return
	}
	root := os.Getenv("LANTAI_ASSET_FIXTURE_ROOT")
	if root == "" {
		t.Skip("explicit synthetic nine-class corpus required")
	}
	raw, err := os.ReadFile(filepath.Join(root, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var corpus struct {
		Contract string       `json:"contract"`
		Assets   []jointAsset `json:"assets"`
	}
	if err = json.Unmarshal(raw, &corpus); err != nil || corpus.Contract != "lantai.synthetic-fixtures/v1" || len(corpus.Assets) != 18 {
		t.Fatal("nine two-version synthetic corpus required", err)
	}
	e := newEnv(t, storage.Config{}, transfer.Limits{})
	e.setRole(e.admin.Context.PrincipalID, identity.RoleContributor, true)
	p, maker := e.agent("joint-maker@synthetic", identity.RoleContributor)
	p, err = e.id.GetPrincipal(t.Context(), e.login().Context, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	credential := e.sudo(&identity.IssueCredential{PrincipalID: p.ID, ExpectedRevision: p.Revision, Scopes: []identity.Scope{identity.ScopeRead, identity.ScopeIngest}})
	limited, err := e.id.ExchangeToken(t.Context(), credential.Secret, identity.SessionRequest{Channel: identity.ChannelCLI})
	if err != nil {
		t.Fatal(err)
	}
	a := reopenApplication(t, e)
	c := jointClient(t, a, maker.Token)
	work := assetRealTempDir(t)
	oracle := jointOracle{Contract: "lantai.joint-recovery-test/v1", InstanceID: string(a.Instance.InstanceID()), ProjectID: string(e.project.ProjectID), SourceOrigin: c.Origin(), GeneratorManifest: raw, Assets: corpus.Assets}
	previous := map[string]client.ImportResult{}
	for _, version := range []string{"v1", "v2"} {
		input := client.ImportInput{Schema: client.ImportInputSchema}
		for _, item := range corpus.Assets {
			if item.Version != version {
				continue
			}
			push := client.PushInput{ProjectID: oracle.ProjectID, Content: client.ContentInput{AssetType: item.Type, Files: item.Files, Rights: item.Rights}}
			if version == "v1" {
				push.Slug = "joint/" + item.Key
			} else {
				prior := previous[item.Key]
				push.AssetID = prior.AssetID
				push.BaseVersionID = prior.VersionID
			}
			push, err = client.ApplyAssetProfile(push, client.AssetProfile{Characters: item.Profile.Characters, Source: item.Profile.Source, BusinessState: item.Profile.BusinessState, LegacyVersion: version})
			if err != nil {
				t.Fatal(err)
			}
			input.Assets = append(input.Assets, client.ImportAsset{Key: item.Key, Directory: item.Root, Classification: "final", Push: push})
		}
		limitedToken := ""
		if version == "v1" {
			limitedToken = limited.Token
		}
		report := jointBatch(t, c, maker.Token, limitedToken, root, work, version, input)
		for _, r := range report.Results {
			previous[r.Key] = r
		}
		for i := range oracle.Assets {
			item := &oracle.Assets[i]
			if item.Version != version {
				continue
			}
			r := previous[item.Key]
			item.AssetID = r.AssetID
			item.VersionID = r.VersionID
			if version == "v2" {
				profile, er := client.ApplyAssetProfile(client.PushInput{}, client.AssetProfile{Characters: item.Profile.Characters, Source: item.Profile.Source, BusinessState: item.Profile.BusinessState, LegacyVersion: version})
				if er != nil {
					t.Fatal(er)
				}
				current, er := c.Do(t.Context(), http.MethodGet, "/api/v1/assets/"+r.AssetID+"?view=full", nil, client.Options{})
				if er != nil {
					t.Fatal(er)
				}
				if _, er = c.Do(t.Context(), http.MethodPatch, "/api/v1/assets/"+r.AssetID+"/metadata", profile.Describe, client.Options{IfMatch: current.ETag, IdempotencyKey: "joint-description-" + r.AssetID}); er != nil {
					t.Fatal(er)
				}
				item.Description = jointDescription(t, c, r.AssetID)
			}
			item.Manifest = jointReadManifest(t, c, *item)
		}
	}
	if err = a.Sync(t.Context()); err != nil {
		t.Fatal(err)
	}
	jointValidate(t, c, oracle, filepath.Join(work, "before-backup"))
	proofDir := filepath.Join(work, "proof-source")
	if err = os.Mkdir(proofDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(proofDir, "oracle.json"), mustJSON(oracle), 0600); err != nil {
		t.Fatal(err)
	}
	push, err := client.PreparePush(t.Context(), client.PushInput{ProjectID: oracle.ProjectID, Slug: jointOracleSlug, Content: client.ContentInput{AssetType: "config", Rights: mustJSON(rightsOwned()), Files: []client.InputFile{{Path: "oracle.json", Role: "primary"}}}}, proofDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Push(t.Context(), push, proofDir, filepath.Join(work, "proof-state.json"), false); err != nil {
		t.Fatal(err)
	}
	if err = a.Sync(t.Context()); err != nil {
		t.Fatal(err)
	}
	backupDir := filepath.Join(work, "backup")
	backup, err := a.Backup(t.Context(), operations.BackupOptions{Destination: backupDir})
	if err != nil || backup.RecoveryPackage == nil {
		t.Fatal("complete joint backup", err)
	}
	file := operations.RecoveryPackagePath(backupDir)
	if mode := os.Getenv("LANTAI_ASSET_RECOVERY_MODE"); mode == "capture" {
		out := os.Getenv("LANTAI_ASSET_RECOVERY_EXPORT")
		if out == "" {
			t.Fatal("explicit empty export directory required")
		}
		entries, er := os.ReadDir(out)
		if er != nil || len(entries) != 0 {
			t.Fatal("export must be empty", er)
		}
		b, er := os.ReadFile(file)
		if er != nil {
			t.Fatal(er)
		}
		if er = os.WriteFile(filepath.Join(out, "recovery.lantai"), b, 0600); er != nil {
			t.Fatal(er)
		}
		t.Logf("CAPTURE PASS: package=%s; nine classes and verification asset are inside package; no external oracle or key exported", backup.RecoveryPackage.SHA256)
		return
	}
	if err = a.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	sourceHome := a.Instance.Layout().Home
	if err = os.Rename(sourceHome, sourceHome+".disconnected"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sourceHome + ".disconnected") })
	t.Setenv("LANTAI_ASSET_FIXTURE_ROOT", "")
	jointRestore(t, file)
}

func jointRestore(t *testing.T, file string) {
	t.Helper()
	if file == "" {
		t.Fatal("complete package path required")
	}
	ctx := t.Context()
	home := filepath.Join(assetRealTempDir(t), "restored")
	m, err := application.RestorePackage(ctx, file, operations.RestoreOptions{Home: home}, identity.Config{Password: fastPassword})
	if err != nil {
		t.Fatal(err)
	}
	if m.Restore == nil {
		t.Fatal("restore gate missing")
	}
	opts := application.Options{Instance: operations.Options{Home: home}, Identity: identity.Config{Password: fastPassword}}
	a, err := application.OpenOffline(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	var principal ids.ID
	var name string
	var secret []byte
	const password = "joint synthetic restored administrator password"
	err = a.Maintenance(ctx, func(c context.Context) error {
		admins, er := a.Identity.RestoreAdministrators(c)
		if er != nil {
			return er
		}
		if len(admins) == 0 {
			return fmt.Errorf("restored administrator missing")
		}
		name = admins[0].Name
		reset, er := a.Identity.BeginOfflineReset(c, name, password)
		if er != nil {
			return er
		}
		principal = reset.Principal().ID
		if _, er = reset.PrepareRecoveryCodes(); er != nil {
			return er
		}
		secret, er = base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(reset.Enrollment.Secret)
		if er != nil {
			return er
		}
		_, er = reset.Confirm(c, totp.Code(secret, totp.Counter(time.Now())), "joint synthetic recovery")
		return er
	})
	if err != nil {
		t.Fatal(err)
	}
	key, err := masterkey.Load(a.Instance.Config().SecretsDir)
	if err != nil || key.ID() == m.Restore.SourceKeyID {
		t.Fatal("runtime key not rotated", err)
	}
	err = a.Maintenance(ctx, func(c context.Context) error {
		report, e := a.Recover(c)
		if e != nil {
			return e
		}
		if len(report.NeedsReconciliation) != 0 {
			return fmt.Errorf("joint restore has unresolved operations: %d", len(report.NeedsReconciliation))
		}
		integrity, e := a.FSCK(c, true)
		if e != nil {
			return e
		}
		return integrity.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	proof := mustJSON(map[string]any{"core_review": map[string]any{"run_id": m.Restore.RunID, "automatic_checks_passed": true, "unresolved_operations": 0}, "operator_accepted_recovery_point": true, "operator_confirmed_known_changes_reviewed": true, "operator_confirmed_source_offline": true, "unknown_after_capture_verified": false})
	review := operations.RecoveryReview{Contract: "lantai.recovery-review/v2", RunID: m.Restore.RunID, BackupID: m.Restore.BackupID, ManifestDigest: m.Restore.ManifestDigest, RecoveryEpoch: m.RecoveryEpoch, Administrator: string(principal), AcceptRecoveryPoint: true, UnknownAfterCapture: true, EvidenceDigest: digest.Of(proof), Note: "Synthetic source stopped; explicitly accept recorded recovery point."}
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
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	login, err := a.Identity.Login(ctx, identity.LoginRequest{Name: name, Password: password, Code: totp.Code(secret, totp.Counter(time.Now())+1), Channel: identity.ChannelCLI})
	if err != nil {
		t.Fatal(err)
	}
	c := jointClient(t, a, login.Token)
	projects, err := c.Do(ctx, http.MethodGet, "/api/v1/projects", nil, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		Items []struct {
			ProjectID string `json:"project_id"`
		} `json:"items"`
	}
	if err = projects.Decode(&list); err != nil || len(list.Items) != 1 {
		t.Fatal("restored project discovery", err)
	}
	found, err := c.FindAssets(ctx, client.AssetFilter{ProjectID: list.Items[0].ProjectID}, client.AssetFindOptions{})
	if err != nil || !found.Complete {
		t.Fatal(err)
	}
	var oracleEntry client.WorksetEntry
	for _, item := range found.Items {
		if item.Slug == jointOracleSlug {
			oracleEntry = client.WorksetEntry{AssetID: item.AssetID, VersionID: item.VersionID, Destination: "proof"}
		}
	}
	if oracleEntry.AssetID == "" {
		t.Fatal("verification asset missing from package")
	}
	work := assetRealTempDir(t)
	proofDir := filepath.Join(work, "proof")
	if _, err = c.PullWorkset(ctx, client.WorksetInput{Schema: client.WorksetSchema, Entries: []client.WorksetEntry{oracleEntry}}, proofDir, "archive_review"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(proofDir, "proof", "oracle.json"))
	if err != nil {
		t.Fatal(err)
	}
	var oracle jointOracle
	if err = json.Unmarshal(b, &oracle); err != nil || oracle.Contract != "lantai.joint-recovery-test/v1" || len(oracle.Assets) != 18 || oracle.ProjectID != list.Items[0].ProjectID || oracle.InstanceID != string(a.Instance.InstanceID()) {
		t.Fatal("restored verifier binding", err)
	}
	destination := filepath.Join(work, "roundtrip")
	if out := os.Getenv("LANTAI_ASSET_RECOVERY_OUTPUT"); out != "" {
		entries, er := os.ReadDir(out)
		if er != nil || len(entries) != 0 {
			t.Fatal("output must be empty", er)
		}
		destination = filepath.Join(out, "roundtrip")
	}
	jointValidate(t, c, oracle, destination)
	input := client.ImportInput{Schema: client.ImportInputSchema}
	continuationSource := filepath.Join(work, "continuation-source")
	var third []jointAsset
	for _, item := range oracle.Assets {
		if item.Version != "v2" {
			continue
		}
		next := item
		next.Version, next.Root = "v3", item.Key+"/v3"
		next.Files = append([]client.InputFile(nil), item.Files...)
		for _, f := range item.Files {
			body, er := os.ReadFile(filepath.Join(destination, filepath.FromSlash(item.Root), filepath.FromSlash(f.Path)))
			if er != nil {
				t.Fatal(er)
			}
			path := filepath.Join(continuationSource, filepath.FromSlash(next.Root), filepath.FromSlash(f.Path))
			if er = os.MkdirAll(filepath.Dir(path), 0700); er != nil {
				t.Fatal(er)
			}
			if er = os.WriteFile(path, body, 0600); er != nil {
				t.Fatal(er)
			}
		}
		// Continue each recovered native asset by adding a valid text sidecar;
		// retain the native files and the exact original asset/base identities.
		note := "continued after complete-package recovery: " + item.Key + "\n"
		if er := os.WriteFile(filepath.Join(continuationSource, filepath.FromSlash(next.Root), "continuation-note.txt"), []byte(note), 0600); er != nil {
			t.Fatal(er)
		}
		next.Files = append(next.Files, client.InputFile{Path: "continuation-note.txt", Role: "doc"})
		push, er := client.ApplyAssetProfile(client.PushInput{ProjectID: oracle.ProjectID, AssetID: item.AssetID, BaseVersionID: item.VersionID, Content: client.ContentInput{AssetType: item.Type, Files: next.Files, Rights: item.Rights}}, client.AssetProfile{Characters: item.Profile.Characters, Source: item.Profile.Source, BusinessState: item.Profile.BusinessState, LegacyVersion: "v3"})
		if er != nil {
			t.Fatal(er)
		}
		push, er = client.PreparePush(ctx, push, filepath.Join(continuationSource, filepath.FromSlash(next.Root)))
		if er != nil {
			t.Fatal(er)
		}
		next.Files = push.Content.Files
		third = append(third, next)
		input.Assets = append(input.Assets, client.ImportAsset{Key: item.Key, Directory: next.Root, Classification: "final", Push: push})
	}
	continued := jointBatch(t, c, login.Token, "", continuationSource, work, "restored-v3", input)
	for i, result := range continued.Results {
		if result.AssetID != third[i].AssetID || result.VersionID == third[i].VersionID {
			t.Fatal("continuation changed original asset or reused v2")
		}
		third[i].VersionID = result.VersionID
		third[i].Manifest = jointReadManifest(t, c, third[i])
		oracle.Assets = append(oracle.Assets, third[i])
	}
	if err = a.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	jointValidate(t, c, oracle, filepath.Join(work, "three-versions"))
	t.Log("RESTORE PASS: only complete package supplied; verifier discovered through restored public APIs; nine original assets continued from v2 to v3, all 27 exact versions verified, replay created no v4")
}
