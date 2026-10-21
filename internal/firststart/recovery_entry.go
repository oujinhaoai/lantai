package firststart

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/oujinhaoai/lantai/internal/application"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/operations"
	"github.com/oujinhaoai/lantai/internal/platform/fsutil"
)

const recoveryEntryFile = "recovery-entry.json"

//go:embed entry.html entry.js setup.css
var entryAssets embed.FS

type recoverySummary struct {
	PackageID  ids.ID `json:"package_id"`
	BackupID   ids.ID `json:"backup_id"`
	InstanceID ids.ID `json:"instance_id"`
	SHA256     string `json:"sha256"`
	Size       int64  `json:"size"`
	Files      int    `json:"files"`
	CapturedAt string `json:"captured_at"`
}
type recoveryEntryState struct {
	Contract       string                          `json:"contract"`
	Target         ids.ID                          `json:"target_id"`
	Home           string                          `json:"home"`
	Phase          string                          `json:"phase"`
	Config         []byte                          `json:"target_config"`
	Package        *recoverySummary                `json:"package,omitempty"`
	RunID          ids.ID                          `json:"run_id,omitempty"`
	Administrator  ids.ID                          `json:"administrator,omitempty"`
	Error          string                          `json:"error,omitempty"`
	Administrators []identity.RestoreAdministrator `json:"administrators,omitempty"`
	API            string                          `json:"api"`
	Origins        []string                        `json:"origins"`
	Reserve        uint64                          `json:"reserve"`
	Secrets        string                          `json:"secrets_dir"`
}
type recoveryReview struct {
	ID                    ids.ID   `json:"id"`
	RunID                 ids.ID   `json:"run_id"`
	PackageSHA256         string   `json:"package_sha256"`
	AutomaticChecksPassed bool     `json:"automatic_checks_passed"`
	Unresolved            int      `json:"unresolved_operations"`
	Unknown               []string `json:"unknown_after_capture"`
}

// Entry is the owner-claimed first-start control plane. Before the person
// chooses initialization or recovery it creates no business databases. The
// existing private installation claim is the only admission capability; a
// complete recovery package is never treated as authority to claim a device.
type Entry struct {
	mu             sync.Mutex
	ctx            context.Context
	opts           operations.Options
	dir            string
	cfg            operations.Config
	state          recoveryEntryState
	code           string
	lock           *fsutil.Lock
	busy           bool
	worker         bool
	workerCancel   context.CancelFunc
	wg             sync.WaitGroup
	requests       sync.WaitGroup
	app            *application.App
	install        *Session
	installHandler *Handler
	reset          *identity.OfflineResetSession
	resetExpires   time.Time
	resetAttempts  int
	review         *recoveryReview
	done           chan error
	restoreTimeout time.Duration               // private test override; default two hours.
	restoreStep    func(context.Context) error // deterministic failure injection in package tests.
	identityConfig identity.Config             // private test configuration; never from HTTP.
}

func OpenEntry(ctx context.Context, opts operations.Options, dir string) (*Entry, error) {
	l, err := operations.NewLayout(opts.Home)
	if err != nil {
		return nil, err
	}
	opts.Home = l.Home
	marker, markerErr := operations.ReadMarker(l)
	statePath := filepath.Join(dir, recoveryEntryFile)
	var state recoveryEntryState
	if st, e := os.Lstat(statePath); e == nil {
		if !st.Mode().IsRegular() || st.Size() > 1<<20 || st.Mode().Perm() != 0600 || !privateMetadata(st) {
			return nil, errors.New("unsafe recovery controller record")
		}
		raw, e := os.ReadFile(statePath)
		if e != nil {
			return nil, e
		}
		if e = decodeEntryJSON(raw, &state); e != nil {
			return nil, e
		}
		if state.Contract != "lantai.recovery-entry/v1" || state.Home != l.Home || !state.Target.Valid() || len(state.Config) == 0 || len(state.Config) > 64<<10 || !filepath.IsAbs(state.Secrets) {
			return nil, errors.New("recovery controller belongs to another target")
		}

		switch state.Phase {
		case "select", "uploaded", "restoring", "cancelling", "interrupted", "failed", "administrator", "review", "activating", "complete", "installing":
		default:
			return nil, errors.New("invalid recovery controller phase")
		}
		if state.Phase != "select" && state.Phase != "installing" && state.Phase != "complete" && state.Package == nil {
			return nil, errors.New("recovery controller package missing")
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return nil, e
	}
	if markerErr == nil && marker.Restore == nil {
		// Normal initialization can create its marker before this controller
		// persists "installing". Hand the unfinished setup back to Open, which
		// reacquires the instance lock and verifies its own bound claim. This
		// does not admit a recovery over an already active instance.
		initializing := marker.State == operations.MarkerInitializing && (state.Phase == "select" || state.Phase == "uploaded")
		if initializing || state.Phase == "activating" || state.Phase == "complete" || state.Phase == "installing" || state.Target == "" {
			return nil, nil
		}
		return nil, errors.New("existing instance cannot be overwritten by recovery")
	}
	if markerErr != nil && !errors.Is(markerErr, os.ErrNotExist) {
		return nil, markerErr
	}
	if markerErr == nil && state.Target == "" {
		return nil, nil
	} // legacy offline restore keeps its old entry.
	cfg, err := operations.LoadConfig(l)
	if state.Target != "" {
		cfg.Listen.API = state.API
		cfg.HTTP.AllowedOrigins = state.Origins
		cfg.MinFreeBytes = state.Reserve
		cfg.SecretsDir = state.Secrets
		err = nil
	}
	if err != nil {
		return nil, err
	}
	if err = validateOrigins(cfg.HTTP.AllowedOrigins); err != nil {
		return nil, err
	}
	st, err := os.Lstat(dir)
	if err != nil || !st.IsDir() || st.Mode().Perm() != 0700 || !privateMetadata(st) {
		return nil, errors.New("installation control directory must be private")
	}
	e := &Entry{ctx: ctx, opts: opts, dir: dir, cfg: cfg, state: state, done: make(chan error, 1)}
	if markerErr != nil {
		entries, err := os.ReadDir(l.Home)
		if err != nil {
			return nil, err
		}
		for _, v := range entries {
			if v.Name() != "config.yaml" && v.Name() != "lantai.lock" {
				return nil, errors.New("recovery requires a new empty target")
			}
			s, er := v.Info()
			if er != nil || !s.Mode().IsRegular() {
				return nil, errors.New("unsafe new target")
			}
		}
		e.lock, err = fsutil.TryLock(l.LockPath())
		if err != nil {
			return nil, err
		}
		if state.Target == "" {
			raw, er := os.ReadFile(l.ConfigPath())
			if er != nil {
				_ = e.lock.Unlock()
				return nil, er
			}
			if len(raw) > 64<<10 {
				_ = e.lock.Unlock()
				return nil, errors.New("target configuration too large")
			}
			e.state = recoveryEntryState{Contract: "lantai.recovery-entry/v1", Target: ids.New(), Home: l.Home, Phase: "select", Config: raw, API: cfg.Listen.API, Origins: cfg.HTTP.AllowedOrigins, Reserve: cfg.MinFreeBytes, Secrets: cfg.SecretsDir}
			if err = e.save(); err != nil {
				_ = e.lock.Unlock()
				return nil, err
			}
		}
	}
	c, err := loadClaim(dir, string(e.state.Target))
	if err != nil {
		_ = e.Close()
		return nil, err
	}
	e.code = c.Code
	// A crash may precede the first target marker (for example while verifying
	// or unpacking). The durable controller intent must still become resumable.
	if e.state.Phase == "restoring" || e.state.Phase == "cancelling" {
		e.state.Phase = "interrupted"
	}
	if markerErr == nil {
		e.state.RunID = marker.Restore.RunID
		if e.state.Phase == "failed" {
			e.state.Phase = "interrupted"
		}
		if e.state.Phase == "activating" {
			e.state.Phase = "review"
		}
		if e.state.Administrator.Valid() && (e.state.Phase == "administrator" || e.state.Phase == "review") {
			if err = e.ensureApp(); err != nil {
				_ = e.Close()
				return nil, err
			}
			if err = e.app.Maintenance(ctx, func(c context.Context) error {
				return e.app.Identity.VerifyRestoreAdministrator(c, e.state.RunID, e.state.Administrator)
			}); err == nil {
				e.state.Phase = "review"
			}
		}
	}
	return e, nil
}

func decodeEntryJSON(raw []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		return e
	}
	if d.Decode(&struct{}{}) != io.EOF {
		return errors.New("extra recovery state")
	}
	return nil
}

func (e *Entry) save() error {
	raw, err := json.Marshal(e.state)
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(filepath.Join(e.dir, recoveryEntryFile), raw, 0600)
}
func (e *Entry) packagePath() string { return filepath.Join(e.dir, "recovery-package.lantai") }
func (e *Entry) allowed(r *http.Request) bool {
	e.mu.Lock()
	closed := e.state.Phase == "complete" || e.state.Phase == "activating"
	e.mu.Unlock()
	if closed {
		return false
	}
	h := Handler{origins: e.cfg.HTTP.AllowedOrigins}
	if !h.validOrigin(r) || r.URL.RawQuery != "" || r.URL.Path != r.URL.EscapedPath() {
		return false
	}
	a := r.Header.Values("Authorization")
	return len(a) == 1 && subtle.ConstantTimeCompare([]byte(a[0]), []byte("Bearer "+e.code)) == 1
}
func (e *Entry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	e.requests.Add(1)
	defer e.requests.Done()
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
	e.mu.Lock()
	child := e.installHandler
	e.mu.Unlock()
	if child != nil {
		child.ServeHTTP(w, r)
		return
	}
	if r.Method == "GET" && (r.URL.Path == "/setup" || r.URL.Path == "/setup/" || r.URL.Path == "/setup/entry.js" || r.URL.Path == "/setup/setup.css") {
		h := Handler{origins: e.cfg.HTTP.AllowedOrigins}
		if !h.validOrigin(r) || r.URL.RawQuery != "" {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		name, typ := "entry.html", "text/html; charset=utf-8"
		if r.URL.Path == "/setup/entry.js" {
			name, typ = "entry.js", "text/javascript; charset=utf-8"
		}
		if r.URL.Path == "/setup/setup.css" {
			name, typ = "setup.css", "text/css; charset=utf-8"
		}
		raw, _ := entryAssets.ReadFile(name)
		w.Header().Set("Content-Type", typ)
		_, _ = w.Write(raw)
		return
	}
	if !e.allowed(r) {
		http.Error(w, "Installation claim required", http.StatusUnauthorized)
		return
	}
	if r.Method == "GET" && r.URL.Path == "/setup/recovery/status" {
		e.status(w, r)
		return
	}
	if r.Method == "POST" && r.URL.Path == "/setup/recovery/cancel" {
		e.mu.Lock()
		cancel := e.workerCancel
		if cancel != nil {
			e.state.Phase = "cancelling"
			_ = e.save()
		}
		e.mu.Unlock()
		if cancel == nil {
			http.Error(w, "No active restore", http.StatusConflict)
			return
		}
		cancel()
		reply(w, map[string]bool{"cancelling": true})
		return
	}
	if r.Method != "POST" {
		http.NotFound(w, r)
		return
	}
	e.mu.Lock()
	if e.busy {
		e.mu.Unlock()
		http.Error(w, "Recovery operation in progress", http.StatusConflict)
		return
	}
	e.busy = true
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		if !e.worker {
			e.busy = false
		}
		e.mu.Unlock()
	}()
	if r.URL.Path == "/setup/recovery/upload" {
		e.upload(w, r)
		return
	}
	typ, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || typ != "application/json" {
		http.Error(w, "JSON required", http.StatusUnsupportedMediaType)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16384)
	switch r.URL.Path {
	case "/setup/begin":
		e.initialize(w, r)
	case "/setup/recovery/restore":
		e.restore(w, r)
	case "/setup/recovery/admin":
		e.adminBegin(w, r)
	case "/setup/recovery/admin-confirm":
		e.adminConfirm(w, r)
	case "/setup/recovery/review":
		e.prepareReview(w, r)
	case "/setup/recovery/complete":
		e.complete(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (e *Entry) status(w http.ResponseWriter, _ *http.Request) {
	e.mu.Lock()
	defer e.mu.Unlock()
	reply(w, map[string]any{"phase": e.state.Phase, "busy": e.busy, "package": e.state.Package, "run_id": e.state.RunID, "administrator": e.state.Administrator, "error": e.state.Error, "review": e.review, "administrators": e.state.Administrators})
}

func (e *Entry) initialize(w http.ResponseWriter, r *http.Request) {
	if e.state.Phase != "select" && e.state.Phase != "uploaded" {
		http.Error(w, "Not a new installation", http.StatusConflict)
		return
	}
	if e.lock != nil {
		_ = e.lock.Unlock()
		e.lock = nil
	}
	removeClaim(e.dir, string(e.state.Target), e.code)
	s, err := Open(e.ctx, e.opts, e.dir)
	if err != nil || s == nil {
		http.Error(w, "Initialization unavailable", http.StatusServiceUnavailable)
		return
	}
	s.code = e.code
	raw, _ := json.Marshal(claim{Schema: "lantai.first-start/v1", InstanceID: string(s.Instance.InstanceID()), Code: e.code})
	if err = fsutil.WriteFileAtomic(filepath.Join(e.dir, ClaimFile), raw, 0600); err != nil {
		_ = s.Close()
		http.Error(w, "Installation claim could not be committed", http.StatusServiceUnavailable)
		return
	}
	h, err := NewHandler(s, e.cfg.HTTP.AllowedOrigins)
	if err != nil {
		_ = s.Close()
		http.Error(w, "Initialization unavailable", http.StatusServiceUnavailable)
		return
	}
	e.mu.Lock()
	e.install = s
	e.installHandler = h
	e.state.Phase = "installing"
	err = e.save()
	e.mu.Unlock()
	if err != nil {
		http.Error(w, "Installation state unavailable", http.StatusServiceUnavailable)
		return
	}
	go func() {
		select {
		case err := <-h.done:
			e.done <- err
		case <-e.ctx.Done():
		}
	}()
	h.ServeHTTP(w, r)
}

func (e *Entry) upload(w http.ResponseWriter, r *http.Request) {
	if e.state.Phase != "select" && e.state.Phase != "uploaded" {
		http.Error(w, "Package cannot be replaced after restore begins", http.StatusConflict)
		return
	}
	if r.Header.Get("Content-Type") != "application/octet-stream" || r.ContentLength <= 0 || r.ContentLength > 1<<40 {
		http.Error(w, "A bounded complete package is required", http.StatusRequestEntityTooLarge)
		return
	}
	free, err := fsutil.FreeBytes(e.dir)
	if err != nil || uint64(r.ContentLength) > free || free-uint64(r.ContentLength) < e.cfg.MinFreeBytes {
		http.Error(w, "Insufficient private recovery space", http.StatusInsufficientStorage)
		return
	}
	f, err := os.CreateTemp(e.dir, ".recovery-upload-")
	if err != nil {
		http.Error(w, "Private upload unavailable", http.StatusServiceUnavailable)
		return
	}
	defer os.Remove(f.Name())
	defer f.Close()
	r.Body = http.MaxBytesReader(w, r.Body, r.ContentLength)
	controller := http.NewResponseController(w)
	_ = controller.SetWriteDeadline(time.Now().Add(2 * time.Minute))
	_, err = io.Copy(f, &entryUploadReader{r: r.Body, c: controller})
	if err == nil {
		err = f.Sync()
	}
	if err == nil {
		err = f.Close()
	}
	if err != nil {
		http.Error(w, "Upload interrupted", http.StatusBadRequest)
		return
	}
	info, err := operations.VerifyRecoveryPackage(r.Context(), f.Name())
	if err != nil {
		http.Error(w, "Package integrity or format check failed", http.StatusUnprocessableEntity)
		return
	}
	if err = os.Rename(f.Name(), e.packagePath()); err == nil {
		err = fsutil.SyncDir(e.dir)
	}
	if err != nil {
		http.Error(w, "Package could not be committed", http.StatusServiceUnavailable)
		return
	}
	e.mu.Lock()
	e.state.Package = &recoverySummary{info.Manifest.PackageID, info.Manifest.BackupID, info.Manifest.InstanceID, info.SHA256, info.Size, len(info.Manifest.Files), info.Manifest.CapturedAt}
	e.state.Phase = "uploaded"
	e.state.Error = ""
	err = e.save()
	e.mu.Unlock()
	if err != nil {
		http.Error(w, "Upload state unavailable", http.StatusServiceUnavailable)
		return
	}
	reply(w, map[string]bool{"verified": true})
}

type entryUploadReader struct {
	r io.Reader
	c *http.ResponseController
}

func (r *entryUploadReader) Read(p []byte) (int, error) {
	_ = r.c.SetReadDeadline(time.Now().Add(2 * time.Minute))
	return r.r.Read(p)
}

func (e *Entry) restore(w http.ResponseWriter, r *http.Request) {
	var in struct {
		SHA256  string `json:"package_sha256"`
		Confirm bool   `json:"confirm_new_target"`
	}
	if readJSON(r, &in) != nil || !in.Confirm || e.state.Package == nil || in.SHA256 != e.state.Package.SHA256 {
		http.Error(w, "Confirm this package and new target", http.StatusBadRequest)
		return
	}
	if e.state.Phase != "uploaded" && e.state.Phase != "interrupted" && e.state.Phase != "failed" {
		http.Error(w, "Restore is not available in this stage", http.StatusConflict)
		return
	}
	e.mu.Lock()
	e.state.Phase = "restoring"
	e.state.Error = ""
	err := e.save()
	e.mu.Unlock()
	if err != nil {
		http.Error(w, "Restore state unavailable", http.StatusServiceUnavailable)
		return
	}
	timeout := e.restoreTimeout
	if timeout == 0 {
		timeout = 2 * time.Hour
	}
	ctx, cancel := context.WithTimeout(e.ctx, timeout)
	e.mu.Lock()
	e.worker = true
	e.workerCancel = cancel
	e.mu.Unlock()
	e.wg.Add(1)
	go func() { defer e.wg.Done(); defer cancel(); e.restoreWorker(ctx) }()
	reply(w, map[string]bool{"accepted": true})
}

func (e *Entry) restoreWorker(ctx context.Context) {
	e.mu.Lock()
	e.busy = true
	summary := *e.state.Package
	config := append([]byte(nil), e.state.Config...)
	e.mu.Unlock()
	var err error
	if e.restoreStep != nil {
		err = e.restoreStep(ctx)
	}
	var info operations.RecoveryPackageInfo
	if err == nil {
		info, err = operations.VerifyRecoveryPackage(ctx, e.packagePath())
	}
	if err == nil && info.SHA256 != summary.SHA256 {
		err = errors.New("package changed")
	}
	if err == nil {
		l, _ := operations.NewLayout(e.opts.Home)
		if _, markerErr := operations.ReadMarker(l); errors.Is(markerErr, os.ErrNotExist) {
			keys, keyErr := os.ReadDir(e.cfg.SecretsDir)
			if keyErr != nil && !errors.Is(keyErr, os.ErrNotExist) {
				err = keyErr
			} else if len(keys) > 0 {
				err = errors.New("new target secret directory is not empty")
			}
			current, readErr := os.ReadFile(l.ConfigPath())
			if err != nil {
			} else if errors.Is(readErr, os.ErrNotExist) {
			} else if readErr != nil || !sameBytes(current, config) {
				err = errors.New("new target configuration changed")
			} else {
				err = os.Remove(l.ConfigPath())
			}
		}
	}
	if err == nil && e.lock != nil {
		_ = e.lock.Unlock()
		e.lock = nil
	}
	var marker operations.Marker
	if err == nil {
		marker, err = application.RestorePackage(ctx, e.packagePath(), operations.RestoreOptions{Home: e.opts.Home, TargetConfig: config, StagingDir: e.dir}, e.identityConfig)
	}
	var app *application.App
	var admins []identity.RestoreAdministrator
	if err == nil {
		app, err = application.OpenOffline(ctx, application.Options{Instance: e.opts, Identity: e.identityConfig})
	}
	if err == nil {
		err = app.Maintenance(ctx, func(c context.Context) error {
			var e error
			admins, e = app.Identity.RestoreAdministrators(c)
			return e
		})
	}
	if err != nil && app != nil {
		_ = app.Close(context.Background())
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.busy = false
	e.worker = false
	e.workerCancel = nil
	if err != nil {
		e.state.Phase = "failed"
		e.state.Error = "restore_failed"
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			e.state.Phase = "interrupted"
			e.state.Error = "restore_interrupted"
		}
	} else {
		e.app = app
		e.state.Administrators = admins
		e.state.Phase = "administrator"
		e.state.RunID = marker.Restore.RunID
	}
	if e.save() != nil {
		e.state.Error = "state_commit_failed"
	}
}

func (e *Entry) ensureApp() error {
	if e.app != nil {
		return nil
	}
	a, err := application.OpenOffline(e.ctx, application.Options{Instance: e.opts, Identity: e.identityConfig})
	if err != nil {
		return err
	}
	e.app = a
	return nil
}
func (e *Entry) adminBegin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name     string `json:"name"`
		Password string `json:"password"`
	}
	if readJSON(r, &in) != nil || e.state.RunID == "" || e.state.Phase != "administrator" {
		http.Error(w, "Administrator reset request invalid", http.StatusBadRequest)
		return
	}
	if err := e.ensureApp(); err != nil {
		http.Error(w, "Recovery target unavailable", http.StatusServiceUnavailable)
		return
	}
	err := e.app.Maintenance(r.Context(), func(ctx context.Context) error {
		var err error
		e.reset, err = e.app.Identity.BeginOfflineReset(ctx, in.Name, in.Password)
		return err
	})
	if err != nil {
		http.Error(w, "Use an existing administrator name and a new valid password", http.StatusBadRequest)
		return
	}
	codes, err := e.reset.PrepareRecoveryCodes()
	if err != nil {
		http.Error(w, "Administrator enrollment unavailable", http.StatusServiceUnavailable)
		return
	}
	e.resetExpires = time.Now().Add(10 * time.Minute)
	e.resetAttempts = 0
	e.mu.Lock()
	e.state.Administrator = e.reset.Principal().ID
	err = e.save()
	e.mu.Unlock()
	if err != nil {
		http.Error(w, "Administrator state unavailable", http.StatusServiceUnavailable)
		return
	}
	reply(w, map[string]any{"totp_secret": e.reset.Enrollment.Secret, "totp_uri": e.reset.Enrollment.URI, "recovery_codes": codes})
}
func (e *Entry) adminConfirm(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Code  string `json:"totp_code"`
		Saved bool   `json:"recovery_codes_saved"`
	}
	if readJSON(r, &in) != nil || e.state.Phase != "administrator" || !in.Saved || e.reset == nil || !time.Now().Before(e.resetExpires) || e.resetAttempts >= 5 {
		http.Error(w, "Start administrator enrollment again", http.StatusConflict)
		return
	}
	e.resetAttempts++
	err := e.app.Maintenance(r.Context(), func(ctx context.Context) error {
		_, err := e.reset.Confirm(ctx, in.Code, "confirmed by new-target recovery owner")
		return err
	})
	if err != nil {
		http.Error(w, "Authenticator confirmation failed", http.StatusBadRequest)
		return
	}
	e.reset = nil
	e.mu.Lock()
	e.state.Phase = "review"
	err = e.save()
	e.mu.Unlock()
	if err != nil {
		http.Error(w, "Administrator saved; reload recovery status", http.StatusServiceUnavailable)
		return
	}
	reply(w, map[string]bool{"confirmed": true})
}
func (e *Entry) prepareReview(w http.ResponseWriter, r *http.Request) {
	var in struct{}
	if readJSON(r, &in) != nil || !e.state.Administrator.Valid() || e.state.Phase != "review" {
		http.Error(w, "Administrator confirmation required", http.StatusConflict)
		return
	}
	if err := e.ensureApp(); err != nil {
		http.Error(w, "Recovery target unavailable", http.StatusServiceUnavailable)
		return
	}
	review := &recoveryReview{ID: ids.New(), RunID: e.state.RunID, PackageSHA256: e.state.Package.SHA256, Unknown: []string{"revocations after capture", "deletions after capture", "external effects after capture"}}
	err := e.app.Maintenance(r.Context(), func(ctx context.Context) error {
		if err := e.app.Identity.VerifyRestoreAdministrator(ctx, e.state.RunID, e.state.Administrator); err != nil {
			return err
		}
		report, err := e.app.Recover(ctx)
		review.Unresolved = len(report.NeedsReconciliation)
		return err
	})
	if err != nil {
		http.Error(w, "Recovery domain checks failed", http.StatusUnprocessableEntity)
		return
	}
	review.AutomaticChecksPassed = review.Unresolved == 0
	e.mu.Lock()
	e.review = review
	e.state.Phase = "review"
	err = e.save()
	e.mu.Unlock()
	if err != nil {
		http.Error(w, "Recovery review could not be saved", http.StatusServiceUnavailable)
		return
	}
	reply(w, review)
}
func (e *Entry) complete(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ReviewID      ids.ID `json:"review_id"`
		KnownChanges  bool   `json:"known_changes_reviewed"`
		SourceOffline bool   `json:"source_offline"`
		Activate      bool   `json:"activate"`
		AcceptPoint   bool   `json:"accept_recovery_point"`
	}
	if readJSON(r, &in) != nil || !in.KnownChanges || !in.SourceOffline || !in.Activate || !in.AcceptPoint || e.review == nil || in.ReviewID != e.review.ID || !e.review.AutomaticChecksPassed {
		http.Error(w, "A current core review and explicit cutover confirmations are required", http.StatusConflict)
		return
	}
	// The record distinguishes verified local checks from the operator's
	// declarations. It never claims knowledge of a disconnected source.
	evidence, _ := json.Marshal(map[string]any{"core_review": e.review, "operator_confirmed_known_changes_reviewed": true, "operator_confirmed_source_offline": true, "operator_accepted_recovery_point": true, "unknown_after_capture_verified": false})
	m := e.app.Instance.Marker()
	if m.Restore == nil {
		http.Error(w, "No active restore", http.StatusConflict)
		return
	}
	review := operations.RecoveryReview{Contract: "lantai.recovery-review/v2", RunID: m.Restore.RunID, BackupID: m.Restore.BackupID, ManifestDigest: m.Restore.ManifestDigest, RecoveryEpoch: m.RecoveryEpoch, Administrator: string(e.state.Administrator), AcceptRecoveryPoint: true, UnknownAfterCapture: true, EvidenceDigest: digest.Of(evidence), Note: "Core checks passed; owner accepted the recovery point and confirmed known-change review and source isolation. Unknown source events remain unknown, not reconciled or certified."}
	e.mu.Lock()
	e.state.Phase = "activating"
	err := e.save()
	e.mu.Unlock()
	if err != nil {
		http.Error(w, "Cutover state unavailable", http.StatusServiceUnavailable)
		return
	}
	if err = e.app.CompleteRestore(r.Context(), review, evidence); err != nil {
		e.mu.Lock()
		e.state.Phase = "review"
		e.state.Error = "reconciliation_required"
		_ = e.save()
		e.mu.Unlock()
		http.Error(w, "Recovery remains closed; domain reconciliation required", http.StatusConflict)
		return
	}
	e.mu.Lock()
	e.state.Phase = "complete"
	err = e.save()
	e.mu.Unlock()
	if err != nil {
		http.Error(w, "Restore committed; restart to activate", http.StatusServiceUnavailable)
		return
	}
	removeClaim(e.dir, string(e.state.Target), e.code)
	reply(w, map[string]bool{"complete": true})
	e.done <- nil
}

func (e *Entry) Serve(ctx context.Context) error {
	ln, err := net.Listen("tcp", e.cfg.Listen.API)
	if err != nil {
		return err
	}
	s := &http.Server{Handler: e, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 8192, ErrorLog: log.New(io.Discard, "", 0), BaseContext: func(net.Listener) context.Context { return ctx }}
	result := make(chan error, 1)
	go func() { result <- s.Serve(ln) }()
	select {
	case <-ctx.Done():
		err = ctx.Err()
	case err = <-e.done:
	case err = <-result:
	}
	c, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err = errors.Join(err, s.Shutdown(c))
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
func (e *Entry) Close() error {
	e.mu.Lock()
	cancel := e.workerCancel
	e.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	e.requests.Wait()
	e.wg.Wait()
	if e.install != nil {
		return e.install.Close()
	}
	if e.app != nil {
		return e.app.Close(context.Background())
	}
	if e.lock != nil {
		return e.lock.Unlock()
	}
	return nil
}
func (e *Entry) Description() string {
	return "First installation or complete-package recovery is pending at /setup/; use the private new-device installation claim."
}

func sameBytes(a, b []byte) bool { return sha256.Sum256(a) == sha256.Sum256(b) }
