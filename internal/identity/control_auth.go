package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"sync"
	"time"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity/totp"
)

type BackupControlScope struct {
	JobID   ids.ID `json:"job_id"`
	Attempt int    `json:"attempt"`
}
type BackupControlScopeReader func() (BackupControlScope, bool)
type BackupControlPrincipal struct {
	ID          ids.ID `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
}
type BackupControlSession struct {
	Principal    BackupControlPrincipal `json:"principal"`
	ExpiresAt    time.Time              `json:"expires_at"`
	CSRFToken    string                 `json:"csrf_token"`
	Scope        BackupControlScope     `json:"scope"`
	Capabilities []string               `json:"capabilities"`
	Token        string                 `json:"-"`
}
type controlSession struct {
	info             BackupControlSession
	epoch, authEpoch int64
	factor           ids.ID
}
type backupControlSessions struct {
	mu       sync.Mutex
	sessions map[[32]byte]controlSession
}

func (s *Service) initControlSessions() {
	s.control = &backupControlSessions{sessions: map[[32]byte]controlSession{}}
}
func randomControlToken() (string, error) {
	var b [32]byte
	if _, e := rand.Read(b[:]); e != nil {
		return "", e
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}
func controlScope(scope BackupControlScopeReader) (BackupControlScope, error) {
	if scope == nil {
		return BackupControlScope{}, errcode.New(errcode.MaintenanceMode, "backup reauthentication requires an active backup pause")
	}
	v, ok := scope()
	if !ok || !v.JobID.Valid() || v.Attempt < 1 {
		return v, errcode.New(errcode.MaintenanceMode, "backup reauthentication requires an active backup pause")
	}
	return v, nil
}

// LoginBackupControl is a separate authority: it does not issue an ordinary
// identity session or write any identity database. Only the sidecar anti-replay
// ledger is persisted. The scope callback must confirm an active backup and closed business admission.
func (s *Service) LoginBackupControl(ctx context.Context, name, pw, code, source string, scope BackupControlScopeReader) (BackupControlSession, error) {
	initial, e := controlScope(scope)
	if e != nil {
		return BackupControlSession{}, e
	}
	if s.controlLedger == nil {
		return BackupControlSession{}, controlAuthUnavailable()
	}
	source = normalizeSource(source)
	var reservation ids.ID
	// Reserve under the proof/security locks, then release both for Argon2.
	e = func() error {
		lctx, h, e := s.gate.Coordinator().Acquire(ctx, commands.Request{Security: commands.ModeShared})
		if e != nil {
			return e
		}
		defer h.Release()
		if e = s.lockProof(lctx); e != nil {
			return e
		}
		defer s.unlockProof()
		current, e := controlScope(scope)
		if e != nil || current != initial {
			return errcode.New(errcode.MaintenanceMode, "backup maintenance changed")
		}
		if e = s.controlLedger.problem(); e != nil {
			return e
		}
		p, e := loadPrincipalByName(lctx, s.main, name)
		if e != nil && !errors.Is(e, errNotFound) {
			return e
		}
		if e = s.checkRate(lctx, s.main, p.ID, source, bucketSignIn); e != nil {
			return e
		}
		state := s.controlLedger.clone(s.now())
		activeFailures := 0
		for _, f := range state.Failures {
			if f.At > clock.Millis(s.now().Add(-s.cfg.FailureWindow)) {
				activeFailures++
			}
		}
		if activeFailures >= 100 {
			return errcode.New(errcode.RateLimited, "too many maintenance authentication attempts").WithRetryAfter(s.cfg.FailureWindow)
		}
		reservation, e = s.newID()
		if e != nil {
			return e
		}
		state.Failures = append(state.Failures, controlFailure{ID: reservation, Principal: p.ID, Source: controlSource(source), At: clock.Millis(s.now())})
		return s.controlLedger.save(state)
	}()
	if e != nil {
		return BackupControlSession{}, e
	}
	if s.controlTestHook != nil {
		s.controlTestHook("before_hash")
	}
	p, rec, ok, e := s.verifyPassword(ctx, name, pw)
	if e != nil {
		return BackupControlSession{}, e
	}
	if !ok {
		return BackupControlSession{}, invalidCredentials()
	}
	// Final acceptance rechecks every authoritative value after slow hashing.
	lctx, h, e := s.gate.Coordinator().Acquire(ctx, commands.Request{Security: commands.ModeShared})
	if e != nil {
		return BackupControlSession{}, e
	}
	defer h.Release()
	if e = s.lockProof(lctx); e != nil {
		return BackupControlSession{}, e
	}
	defer s.unlockProof()
	current, e := controlScope(scope)
	if e != nil || current != initial {
		return BackupControlSession{}, errcode.New(errcode.MaintenanceMode, "backup maintenance changed")
	}
	if e = s.controlLedger.problem(); e != nil {
		return BackupControlSession{}, e
	}
	live, e := loadPrincipal(lctx, s.main, p.ID)
	if e != nil || live.Kind != authz.Human || live.State != StateActive || live.AuthEpoch != p.AuthEpoch {
		return BackupControlSession{}, invalidCredentials()
	}
	p = live
	cur, e := loadPassword(lctx, s.main, p.ID)
	if e != nil || (subtle.ConstantTimeCompare(cur.Hash, rec.Hash) != 1 || subtle.ConstantTimeCompare(cur.Salt, rec.Salt) != 1 || cur.Params != rec.Params) {
		return BackupControlSession{}, invalidCredentials()
	}
	role, e := hasSystemRole(lctx, s.main, p.ID, RoleAdmin)
	if e != nil {
		return BackupControlSession{}, e
	}
	if !role {
		return BackupControlSession{}, invalidCredentials()
	}
	acct, e := loadAccount(lctx, s.main, p.ID)
	if e != nil || acct.FactorState != factorEnrolled {
		return BackupControlSession{}, invalidCredentials()
	}
	f, e := loadFactor(lctx, s.main, p.ID, factorEnabled)
	if e != nil {
		return BackupControlSession{}, invalidCredentials()
	}
	secret, e := s.openFactor(f)
	if e != nil {
		return BackupControlSession{}, e
	}
	overlay, e := s.controlCounter(f.ID)
	if e != nil {
		return BackupControlSession{}, e
	}
	last := f.LastCounter
	if overlay > last {
		last = overlay
	}
	result, counter := totp.Verify(secret, code, s.now(), last)
	if result == totp.Replayed {
		return BackupControlSession{}, errcode.New(errcode.TOTPReplay, "this code has already been used; wait for the next code")
	}
	if result != totp.Accepted {
		return BackupControlSession{}, invalidCredentials()
	}
	current, e = controlScope(scope)
	if e != nil || current != initial {
		return BackupControlSession{}, errcode.New(errcode.MaintenanceMode, "backup maintenance changed")
	}
	// Remove only this reservation after all checks succeed, and persist the
	// accepted factor counter before returning any capability to the browser.
	if s.controlTestHook != nil {
		s.controlTestHook("before_commit")
	}
	state := s.controlLedger.clone(s.now())
	found := false
	failures := make([]controlFailure, 0, len(state.Failures))
	for _, f := range state.Failures {
		if f.ID == reservation {
			found = true
			continue
		}
		failures = append(failures, f)
	}
	if !found {
		return BackupControlSession{}, controlAuthUnavailable()
	}
	state.Failures = failures
	state.Counters[f.ID] = counter
	if e = s.controlLedger.save(state); e != nil {
		return BackupControlSession{}, e
	}
	epoch, e := s.recoveryEpoch(lctx)
	if e != nil {
		return BackupControlSession{}, e
	}
	token, e := randomControlToken()
	if e != nil {
		return BackupControlSession{}, e
	}
	token = "lbc_" + token
	csrf, e := randomControlToken()
	if e != nil {
		return BackupControlSession{}, e
	}
	info := BackupControlSession{Principal: BackupControlPrincipal{ID: p.ID, Name: p.Name, DisplayName: p.DisplayName}, ExpiresAt: s.now().Add(10 * time.Minute), CSRFToken: csrf, Scope: initial, Capabilities: []string{"backup.read", "backup.cancel"}, Token: token}
	s.control.mu.Lock()
	defer s.control.mu.Unlock()
	for k, v := range s.control.sessions {
		if !s.now().Before(v.info.ExpiresAt) || v.info.Principal.ID == p.ID {
			delete(s.control.sessions, k)
		}
	}
	if len(s.control.sessions) >= 100 {
		return BackupControlSession{}, errcode.New(errcode.RateLimited, "too many maintenance sessions")
	}
	info.Token = ""
	s.control.sessions[sha256.Sum256([]byte(token))] = controlSession{info: info, epoch: epoch, authEpoch: p.AuthEpoch, factor: f.ID}
	info.Token = token
	return info, nil
}

// WithBackupControl verifies the memory-only capability and reads current
// authoritative identity state under security_guard for each accepted request.
// It cannot be used with a general authz.Context or the ordinary session Guard.
func (s *Service) WithBackupControl(ctx context.Context, token, csrf string, write bool, scope BackupControlScopeReader, run func(BackupControlSession) error) error {
	lctx, h, e := s.gate.Coordinator().Acquire(ctx, commands.Request{Security: commands.ModeShared})
	if e != nil {
		return e
	}
	defer h.Release()
	key := sha256.Sum256([]byte(token))
	s.control.mu.Lock()
	v, ok := s.control.sessions[key]
	s.control.mu.Unlock()
	if !ok || token == "" {
		return sessionErr(errcode.TokenRevoked)
	}
	if !s.now().Before(v.info.ExpiresAt) {
		return sessionErr(errcode.TokenExpired)
	}
	current, e := controlScope(scope)
	if e != nil || current != v.info.Scope {
		return sessionErr(errcode.TokenRevoked)
	}
	p, e := loadPrincipal(lctx, s.main, v.info.Principal.ID)
	if e != nil {
		return sessionErr(errcode.TokenRevoked)
	}
	epoch, e := s.recoveryEpoch(lctx)
	if e != nil {
		return e
	}
	if p.Kind != authz.Human || p.State != StateActive || p.AuthEpoch != v.authEpoch || epoch != v.epoch {
		return sessionErr(errcode.TokenRevoked)
	}
	role, e := hasSystemRole(lctx, s.main, p.ID, RoleAdmin)
	if e != nil {
		return e
	}
	if !role {
		return sessionErr(errcode.TokenRevoked)
	}
	f, e := loadFactor(lctx, s.main, p.ID, factorEnabled)
	if e != nil || f.ID != v.factor {
		return sessionErr(errcode.TokenRevoked)
	}
	if write && subtle.ConstantTimeCompare([]byte(csrf), []byte(v.info.CSRFToken)) != 1 {
		return errcode.New(errcode.Forbidden, "missing or invalid maintenance CSRF token")
	}
	return run(v.info)
}
func (s *Service) EndBackupControl(token string) {
	s.control.mu.Lock()
	delete(s.control.sessions, sha256.Sum256([]byte(token)))
	s.control.mu.Unlock()
}

// DrainBackupControl fences final proof persistence/token issuance before the
// backup owner reopens ordinary writes. It performs no IO and grants no access.
func (s *Service) DrainBackupControl() func() {
	_ = s.lockProof(context.Background())
	return s.unlockProof
}

// LogoutBackupControl also accepts an expired or finished-job token, so the
// browser can discard it before returning to ordinary authentication. Knowing
// a live token is insufficient to revoke it without its independent CSRF.
func (s *Service) LogoutBackupControl(token, csrf string) error {
	key := sha256.Sum256([]byte(token))
	s.control.mu.Lock()
	defer s.control.mu.Unlock()
	if v, ok := s.control.sessions[key]; ok && subtle.ConstantTimeCompare([]byte(csrf), []byte(v.info.CSRFToken)) != 1 {
		return errcode.New(errcode.Forbidden, "missing or invalid maintenance CSRF token")
	}
	delete(s.control.sessions, key)
	return nil
}
