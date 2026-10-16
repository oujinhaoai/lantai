package identity

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/platform/fsutil"
)

// This ledger is an authentication anti-replay overlay, not a second identity
// database. It stores no passwords, seeds, session tokens or authorization.
// backups/ is deliberately outside operations.Backup's file-owner allowlist.
type controlFailure struct {
	ID        ids.ID `json:"id"`
	Principal ids.ID `json:"principal,omitempty"`
	Source    string `json:"source,omitempty"`
	At        int64  `json:"at"`
}
type controlAuthState struct {
	Version    int              `json:"version"`
	Instance   ids.ID           `json:"instance"`
	KeyID      ids.ID           `json:"key_id"`
	Counters   map[ids.ID]int64 `json:"counters"`
	Failures   []controlFailure `json:"failures"`
	ObservedAt int64            `json:"observed_at"`
}
type controlAuthLedger struct {
	path   string
	key    []byte
	state  atomic.Pointer[controlAuthState]
	err    atomic.Pointer[error]
	digest atomic.Pointer[[32]byte]
}

func controlAuthUnavailable() error {
	return errcode.New(errcode.StorageUnavailable, "authentication safety ledger is unavailable; restore or reconcile it before authentication").WithDetails(errcode.Detail{Reason: "authentication_safety_ledger_unavailable"})
}
func (l *controlAuthLedger) problem() error {
	if l == nil {
		return nil
	}
	if l.err.Load() != nil {
		return controlAuthUnavailable()
	}
	if e := checkControlAuthDir(filepath.Dir(filepath.Dir(filepath.Dir(l.path))), false); e != nil {
		l.fail(e)
		return controlAuthUnavailable()
	}
	if e := checkControlAuthDir(filepath.Dir(filepath.Dir(l.path)), true); e != nil {
		l.fail(e)
		return controlAuthUnavailable()
	}
	if e := checkControlAuthDir(filepath.Dir(l.path), true); e != nil {
		l.fail(e)
		return controlAuthUnavailable()
	}
	st, e := os.Lstat(l.path)
	if e != nil || !st.Mode().IsRegular() || st.Mode()&os.ModeSymlink != 0 || !controlAuthOwner(st) || !controlAuthSingleLink(st) {
		if e == nil {
			e = errors.New("unsafe auth ledger file")
		}
		l.fail(e)
		return controlAuthUnavailable()
	}
	if e = fsutil.CheckPrivate(l.path); e != nil {
		l.fail(e)
		return controlAuthUnavailable()
	}
	raw, e := readControlLedger(l.path)
	if e != nil {
		if e == nil {
			e = errors.New("oversized auth ledger")
		}
		l.fail(e)
		return controlAuthUnavailable()
	}
	expected := l.digest.Load()
	actual := sha256.Sum256(raw)
	if expected == nil || actual != *expected {
		l.fail(errors.New("auth ledger changed outside owner"))
		return controlAuthUnavailable()
	}
	return nil
}
func (l *controlAuthLedger) fail(e error) { l.err.Store(&e) }
func (s *Service) initControlAuth(path string) {
	if path == "" {
		return
	}
	// Resolve only the trusted instance-home anchor (for example macOS /var).
	// Its backup/control children are checked with Lstat and never followed.
	parentName := filepath.Base(filepath.Dir(path))
	leaf := filepath.Base(path)
	home, homeErr := filepath.EvalSymlinks(filepath.Dir(filepath.Dir(path)))
	if homeErr == nil {
		path = filepath.Join(home, parentName, leaf)
	}
	l := &controlAuthLedger{path: filepath.Join(path, "state.json")}
	s.controlLedger = l
	if homeErr != nil {
		l.fail(homeErr)
		return
	}
	key, e := s.key.Derive("backup-control-auth-ledger/v1")
	if e != nil {
		l.fail(e)
		return
	}
	l.key = key

	parent := filepath.Dir(path)
	// The trusted instance home may be readable, but neither it nor the private
	// backups parent may be writable by another user. Never follow an alias.
	if e = checkControlAuthDir(filepath.Dir(parent), false); e != nil {
		l.fail(e)
		return
	}
	if _, e = os.Lstat(parent); errors.Is(e, os.ErrNotExist) {
		e = os.Mkdir(parent, 0700)
	}
	if e != nil {
		l.fail(e)
		return
	}
	if e = checkControlAuthDir(parent, true); e != nil {
		l.fail(e)
		return
	}
	if _, e = os.Lstat(path); errors.Is(e, os.ErrNotExist) {
		tmp, err := os.MkdirTemp(parent, ".control-auth-init-")
		if err != nil {
			l.fail(err)
			return
		}
		defer os.RemoveAll(tmp)
		if s.controlInitFault != nil {
			if err = s.controlInitFault("temporary_directory"); err != nil {
				l.fail(err)
				return
			}
		}
		l.path = filepath.Join(tmp, "state.json")
		v := &controlAuthState{Version: 1, Instance: s.instance, KeyID: s.key.ID(), Counters: map[ids.ID]int64{}, Failures: []controlFailure{}}
		if err = l.save(v); err != nil {
			l.fail(err)
			return
		}
		if s.controlInitFault != nil {
			if err = s.controlInitFault("before_install"); err != nil {
				l.fail(err)
				return
			}
		}
		if err = os.Rename(tmp, path); err != nil {
			l.fail(err)
			return
		}
		l.path = filepath.Join(path, "state.json")
		if err = fsutil.SyncDir(parent); err != nil {
			l.fail(err)
		}
		return
	}
	if e != nil {
		l.fail(e)
		return
	}
	if e = checkControlAuthDir(path, true); e != nil {
		l.fail(e)
		return
	}
	st, e := os.Lstat(l.path)
	if e != nil || !st.Mode().IsRegular() || st.Mode()&os.ModeSymlink != 0 || !controlAuthOwner(st) || !controlAuthSingleLink(st) {
		if e == nil {
			e = errors.New("invalid auth ledger file")
		}
		l.fail(e)
		return
	}
	if e = fsutil.CheckPrivate(l.path); e != nil {
		l.fail(e)
		return
	}
	raw, e := readControlLedger(l.path)
	if e != nil {
		l.fail(e)
		return
	}
	var wire struct {
		Payload json.RawMessage `json:"payload"`
		MAC     string          `json:"mac"`
	}
	if e = json.Unmarshal(raw, &wire); e != nil {
		l.fail(e)
		return
	}
	mac, e := hex.DecodeString(wire.MAC)
	h := hmac.New(sha256.New, l.key)
	h.Write(wire.Payload)
	if e != nil || !hmac.Equal(mac, h.Sum(nil)) {
		l.fail(errors.New("auth ledger integrity failed"))
		return
	}
	var v controlAuthState
	if e = json.Unmarshal(wire.Payload, &v); e != nil || v.Version != 1 || v.Instance != s.instance || v.KeyID != s.key.ID() || v.Counters == nil {
		if e == nil {
			e = errors.New("auth ledger binding failed")
		}
		l.fail(e)
		return
	}
	for id, n := range v.Counters {
		if !id.Valid() || n < 0 {
			l.fail(errors.New("invalid auth counter"))
			return
		}
	}
	for _, f := range v.Failures {
		if !f.ID.Valid() || f.At < 0 || (f.Principal != "" && !f.Principal.Valid()) {
			l.fail(errors.New("invalid auth failure"))
			return
		}
	}
	sum := sha256.Sum256(raw)
	l.digest.Store(&sum)
	l.state.Store(&v)
}

// save must be serialized with Service.proofMu. Publish only after durable IO.
func (l *controlAuthLedger) save(v *controlAuthState) error {
	raw, e := json.Marshal(v)
	if e != nil {
		return e
	}
	h := hmac.New(sha256.New, l.key)
	h.Write(raw)
	wire, e := json.Marshal(struct {
		Payload json.RawMessage `json:"payload"`
		MAC     string          `json:"mac"`
	}{raw, hex.EncodeToString(h.Sum(nil))})
	if e != nil {
		return e
	}
	if len(wire) > 8<<20 {
		l.fail(errors.New("oversized auth ledger"))
		return controlAuthUnavailable()
	}
	if e = fsutil.WriteFileAtomic(l.path, wire, 0600); e != nil {
		l.fail(e)
		return controlAuthUnavailable()
	}
	sum := sha256.Sum256(wire)
	l.digest.Store(&sum)
	l.state.Store(v)
	return nil
}
func (l *controlAuthLedger) clone(now time.Time) *controlAuthState {
	old := l.state.Load()
	v := *old
	v.Counters = make(map[ids.ID]int64, len(old.Counters))
	for k, n := range old.Counters {
		v.Counters[k] = n
	}
	v.Failures = nil
	cutoff := clock.Millis(now.Add(-failureRetention))
	for _, f := range old.Failures {
		if f.At > cutoff {
			v.Failures = append(v.Failures, f)
		}
	}
	if clock.Millis(now) > v.ObservedAt {
		v.ObservedAt = clock.Millis(now)
	}
	return &v
}
func controlSource(src string) string {
	if src == "" {
		return ""
	}
	v := sha256.Sum256([]byte(src))
	return hex.EncodeToString(v[:])
}
func (s *Service) controlCounter(id ids.ID) (int64, error) {
	l := s.controlLedger
	if l == nil {
		return -1, nil
	}
	if e := l.problem(); e != nil {
		return 0, e
	}
	n, ok := l.state.Load().Counters[id]
	if !ok {
		return -1, nil
	}
	return n, nil
}
func (s *Service) controlFailures(principal ids.ID, source string, since int64, bucket []string) (int, int64, int, int64, error) {
	l := s.controlLedger
	if l == nil || !slices.Contains(bucket, failLogin) {
		return 0, 0, 0, 0, nil
	}
	if e := l.problem(); e != nil {
		return 0, 0, 0, 0, e
	}
	pn, sn := 0, 0
	var po, so int64
	source = controlSource(source)
	for _, f := range l.state.Load().Failures {
		if f.At <= since {
			continue
		}
		if principal != "" && f.Principal == principal {
			pn++
			if po == 0 || f.At < po {
				po = f.At
			}
		}
		if source != "" && f.Source == source {
			sn++
			if so == 0 || f.At < so {
				so = f.At
			}
		}
	}
	return pn, po, sn, so, nil
}

// lockProof bounds queued proof verification by the request context.
func (s *Service) lockProof(ctx context.Context) error {
	select {
	case s.proofMu <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (s *Service) unlockProof() { <-s.proofMu }

func checkControlAuthDir(path string, private bool) error {
	st, e := os.Lstat(path)
	if e != nil {
		return e
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 || !controlAuthOwner(st) || !controlAuthMode(st, false) {
		return errors.New("unsafe authentication ledger parent")
	}
	if private && !controlAuthMode(st, true) {
		return errors.New("authentication ledger directory is not private")
	}
	resolved, e := filepath.EvalSymlinks(path)
	if e != nil || resolved != path {
		return errors.New("authentication ledger path contains aliases")
	}
	return nil
}

func readControlLedger(path string) ([]byte, error) {
	f, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil {
		return nil, e
	}
	if !st.Mode().IsRegular() || !controlAuthOwner(st) || !controlAuthSingleLink(st) || !controlAuthMode(st, true) || st.Size() > 8<<20 {
		return nil, errors.New("unsafe auth ledger file")
	}
	named, e := os.Lstat(path)
	if e != nil || !os.SameFile(st, named) || named.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("auth ledger file replaced")
	}
	raw, e := io.ReadAll(io.LimitReader(f, (8<<20)+1))
	if len(raw) > 8<<20 {
		return nil, errors.New("oversized auth ledger")
	}
	return raw, e
}
