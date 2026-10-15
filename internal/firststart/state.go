package firststart

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/identity/masterkey"
	"github.com/oujinhaoai/lantai/internal/operations"
	"github.com/oujinhaoai/lantai/internal/platform/fsutil"
)

const ClaimFile = "setup-code.json"

type claim struct {
	Schema     string `json:"schema"`
	InstanceID string `json:"instance_id"`
	Code       string `json:"code"`
}

// Session retains the exclusive instance lock while only the setup handler is
// listening. It never opens the normal business gate.
type Session struct {
	Instance *operations.Instance
	Identity *identity.Service
	code     string
	dir      string
}

// Open returns nil for a completed instance. Unknown nonempty directories and
// incomplete database sets fail closed, leaving explicit repair to an operator.
func Open(ctx context.Context, opts operations.Options, dir string) (_ *Session, err error) {
	l, err := operations.NewLayout(opts.Home)
	if err != nil {
		return nil, err
	}
	m, err := operations.ReadMarker(l)
	if err == nil && m.State == operations.MarkerActive {
		return nil, nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	cfg, e := operations.LoadConfig(l)
	if e != nil {
		return nil, e
	}
	if e = validateOrigins(cfg.HTTP.AllowedOrigins); e != nil {
		return nil, e
	}
	info, e := os.Lstat(dir)
	if e != nil || !info.IsDir() || info.Mode().Perm() != 0o700 || !privateMetadata(info) {
		return nil, errors.New("setup directory must be private, owned by the service, and not a link")
	}
	if errors.Is(err, os.ErrNotExist) {
		entries, e := os.ReadDir(l.Home)
		if e != nil {
			return nil, e
		}
		for _, entry := range entries {
			if entry.Name() != "config.yaml" && entry.Name() != "lantai.lock" {
				return nil, errors.New("first start refuses unmarked nonempty data; inspect or restore it")
			}
			info, e := entry.Info()
			if e != nil || !info.Mode().IsRegular() {
				return nil, errors.New("first start refuses linked or unexpected data entries")
			}
		}
	}
	i, err := operations.Create(ctx, operations.CreateOptions{Options: opts, StrictResume: true})
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = i.Close(context.Background())
		}
	}()
	open, err := identity.BootstrapIsOpen(ctx, i.DB(ownership.Main))
	if err != nil {
		return nil, err
	}
	k, err := masterkey.Load(i.Config().SecretsDir)
	if errors.Is(err, masterkey.ErrMissing) && open {
		k, err = masterkey.Generate(i.IDs(), i.Clock(), nil)
		if err == nil {
			err = masterkey.Save(i.Config().SecretsDir, k)
		}
	}
	if err != nil {
		return nil, err
	}
	svc, err := identity.New(identity.Deps{AuthStateDir: filepath.Join(i.Layout().Home, "backups", "control-auth"), Main: i.DB(ownership.Main), Runtime: i.DB(ownership.Runtime), Gate: i.Gate(), Epochs: i, Clock: i.Clock(), IDs: i.IDs(), Key: k, InstanceID: i.InstanceID(), InstanceName: i.Marker().Name}, identity.Config{})
	if err != nil {
		return nil, err
	}
	if !open {
		if err = svc.CheckStartup(ctx); err != nil {
			return nil, err
		}
		if err = i.Activate(ctx); err != nil {
			return nil, err
		}
		removeClaim(dir, string(i.InstanceID()), "")
		err = i.Close(ctx)
		return nil, err
	}
	c, err := loadClaim(dir, string(i.InstanceID()))
	if err != nil {
		return nil, err
	}
	return &Session{Instance: i, Identity: svc, code: c.Code, dir: dir}, nil
}

func loadClaim(dir, id string) (claim, error) {
	return readClaim(dir, id, true)
}

func readClaim(dir, id string, create bool) (claim, error) {
	var c claim
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 || !privateMetadata(info) {
		return c, errors.New("setup directory must be private, owned by the service, and not a link")
	}
	path := filepath.Join(dir, ClaimFile)
	info, err = os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if !create {
			return c, err
		}
		b := make([]byte, 32)
		if _, err = rand.Read(b); err != nil {
			return c, err
		}
		c = claim{Schema: "lantai.first-start/v1", InstanceID: id, Code: base64.RawURLEncoding.EncodeToString(b)}
		raw, e := json.MarshalIndent(c, "", "  ")
		if e != nil {
			return c, e
		}
		if err = fsutil.CreateFileExclusive(path, append(raw, '\n'), 0o600); err != nil {
			return c, err
		}
		return c, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1024 || info.Mode().Perm() != 0o600 || !privateMetadata(info) {
		return c, errors.New("setup code must be a private regular file owned by the service")
	}
	f, err := os.Open(path)
	if err != nil {
		return c, err
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil || !os.SameFile(info, after) {
		return c, errors.New("setup file changed while opening")
	}
	d := json.NewDecoder(io.LimitReader(f, 1025))
	d.DisallowUnknownFields()
	if err = d.Decode(&c); err != nil {
		return claim{}, errors.New("invalid setup code file")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return claim{}, errors.New("invalid setup code file")
	}
	b, err := base64.RawURLEncoding.DecodeString(c.Code)
	if err != nil || len(b) != 32 || base64.RawURLEncoding.EncodeToString(b) != c.Code || c.Schema != "lantai.first-start/v1" || c.InstanceID != id {
		return claim{}, errors.New("setup code does not belong to this instance")
	}
	return c, nil
}

func (s *Session) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return s.Instance.Close(ctx)
}

func (s *Session) finish(ctx context.Context) error {
	if err := s.Identity.CheckStartup(ctx); err != nil {
		return err
	}
	if err := s.Instance.Activate(ctx); err != nil {
		return err
	}
	// The database is authoritative. Failure to remove this now-useless code
	// must never reopen setup or block the already-created administrator.
	removeClaim(s.dir, string(s.Instance.InstanceID()), s.code)
	return nil
}

func removeClaim(dir, id, code string) {
	c, err := readClaim(dir, id, false)
	if err == nil && (code == "" || c.Code == code) {
		_ = os.Remove(filepath.Join(dir, ClaimFile))
	}
}

func (s *Session) Description() string {
	return fmt.Sprintf("First setup is pending. Open /setup/ over the configured HTTPS origin; read the code from %s.", filepath.Join(s.dir, ClaimFile))
}
